package recording

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"audiorecording/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Runner 从数据库领取已提交的任务。它不使用上传请求的 context，
// 因此客户端断开或上传响应结束不会取消后台处理。
type Runner struct {
	service *Service
	asr     Transcriber
	llm     Summarizer
	workers sync.WaitGroup
	slots   chan struct{}
}

func NewRunner(service *Service, asr Transcriber, concurrency int, summaries ...Summarizer) (*Runner, error) {
	if concurrency <= 0 {
		return nil, errors.New("worker concurrency must be positive")
	}
	r := &Runner{service: service, asr: asr, slots: make(chan struct{}, concurrency)}
	if len(summaries) > 0 {
		r.llm = summaries[0]
	}
	return r, nil
}

// Run 在一个 goroutine 中调用；停止后再调用 Wait，避免领取任务与等待互相竞争。
func (r *Runner) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		r.dispatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Runner) Wait() { r.workers.Wait() }

func (r *Runner) dispatch(ctx context.Context) {
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var tasks []model.Task
	// 每轮分批读取，避免一次把全部待处理行加载进内存；这不是并发任务数限制。
	if err := r.service.db.WithContext(dbCtx).Where("status = ?", model.TaskPending).
		Order("created_at ASC, id ASC").Limit(100).Find(&tasks).Error; err != nil {
		log.Printf("task poll failed error=%v", err)
		return
	}
	for _, task := range tasks {
		if ctx.Err() != nil {
			return
		}
		select {
		case r.slots <- struct{}{}:
		default:
			return
		}
		now := time.Now().UTC()
		// 查询和领取不是一个操作：再次限定 pending，只有 RowsAffected=1 才获得执行权。
		claimed := r.service.db.WithContext(dbCtx).Model(&model.Task{}).
			Where("id = ? AND status = ?", task.ID, model.TaskPending).
			Updates(map[string]any{"status": model.TaskTranscribing, "started_at": now})
		if claimed.Error != nil {
			<-r.slots
			log.Printf("task claim failed task_id=%d error=%v", task.ID, claimed.Error)
			continue
		}
		if claimed.RowsAffected != 1 {
			<-r.slots
			continue
		}
		log.Printf("task transition recording_id=%d task_id=%d from=pending to=transcribing", task.RecordingID, task.ID)
		r.workers.Add(1)
		go func() {
			defer r.workers.Done()
			defer func() { <-r.slots }()
			r.transcribe(ctx, task)
		}()
	}
}

func (r *Runner) transcribe(ctx context.Context, task model.Task) {
	stage := model.TaskTranscribing
	stageStarted := time.Now()
	defer func() {
		log.Printf("task stage ended task_id=%d stage=%s elapsed_ms=%d", task.ID, stage, time.Since(stageStarted).Milliseconds())
	}()
	// Gin 的 recovery 只保护 HTTP goroutine，后台任务需要自己的 panic 兜底。
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("task panic task_id=%d error=%v stack=%s", task.ID, recovered, debug.Stack())
			r.fail(task, stage, "worker_panic", "background task panicked")
		}
	}()
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	var audio model.Recording
	err := r.service.db.WithContext(dbCtx).Where("id = ?", task.RecordingID).Take(&audio).Error
	cancel()
	if err != nil {
		log.Printf("task read recording failed task_id=%d error=%v", task.ID, err)
		r.fail(task, stage, "database_error", "cannot read recording")
		return
	}
	transcript, err := r.asr.Transcribe(ctx, audio.StoragePath)
	if err != nil {
		log.Printf("task transcription failed task_id=%d error=%v", task.ID, err)
		r.fail(task, stage, "transcription_failed", "audio transcription failed")
		return
	}
	if err := r.saveTranscript(ctx, task, transcript); err != nil {
		log.Printf("task save transcript failed task_id=%d error=%v", task.ID, err)
		r.fail(task, stage, "database_error", "cannot save transcript")
		return
	}
	log.Printf("task transition recording_id=%d task_id=%d from=transcribing to=summarizing", task.RecordingID, task.ID)
	log.Printf("task stage ended task_id=%d stage=%s elapsed_ms=%d", task.ID, stage, time.Since(stageStarted).Milliseconds())
	stage = model.TaskSummarizing
	stageStarted = time.Now()
	// nil 只用于独立转写测试；生产启动强制创建真实摘要客户端。
	if r.llm == nil {
		return
	}
	summary, err := r.llm.Summarize(ctx, transcript)
	if err != nil {
		code := "llm_unavailable"
		var summaryErr *SummaryError
		if errors.As(err, &summaryErr) {
			code = summaryErr.Code
		}
		log.Printf("task summary failed task_id=%d code=%s", task.ID, code)
		r.fail(task, stage, code, "summary generation failed")
		return
	}
	if err := r.saveSummary(ctx, task, summary); err != nil {
		log.Printf("task save summary failed task_id=%d error=%v", task.ID, err)
		r.fail(task, stage, "database_error", "cannot save summary")
		return
	}
	log.Printf("task transition recording_id=%d task_id=%d from=summarizing to=done", task.RecordingID, task.ID)
}

func (r *Runner) saveTranscript(ctx context.Context, task model.Task, transcript string) error {
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return r.service.db.WithContext(dbCtx).Transaction(func(tx *gorm.DB) error {
		// 所有跨两表的后台写入先锁任务再修改录音，保持后续重试/删除的锁顺序一致。
		var current model.Task
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", task.ID).Take(&current).Error; err != nil {
			return err
		}
		if current.Status != model.TaskTranscribing {
			return errors.New("task is no longer transcribing")
		}
		result := tx.Model(&model.Recording{}).Where("id = ?", task.RecordingID).Update("transcript", transcript)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("recording %d missing", task.RecordingID)
		}
		return tx.Model(&model.Task{}).Where("id = ?", task.ID).Update("status", model.TaskSummarizing).Error
	})
}

func (r *Runner) fail(task model.Task, stage, code, message string) {
	// 服务 context 已取消时仍给失败状态一次短暂的落库机会；不自动重试外部调用。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := r.service.db.WithContext(ctx).Model(&model.Task{}).
		Where("id = ? AND status = ?", task.ID, stage).
		Updates(map[string]any{
			"status": model.TaskFailed, "failed_stage": stage,
			"error_code": code, "error_message": message, "finished_at": time.Now().UTC(),
		})
	if result.Error != nil {
		log.Printf("task failure write failed recording_id=%d task_id=%d code=%s error=%v", task.RecordingID, task.ID, code, result.Error)
		return
	}
	if result.RowsAffected == 1 {
		log.Printf("task transition recording_id=%d task_id=%d from=%s to=failed code=%s", task.RecordingID, task.ID, stage, code)
	}
}

// saveSummary 与任务完成标记同事务提交，避免看到 done 却没有摘要。
func (r *Runner) saveSummary(ctx context.Context, task model.Task, summary SummaryResult) error {
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return r.service.db.WithContext(dbCtx).Transaction(func(tx *gorm.DB) error {
		var current model.Task
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", task.ID).Take(&current).Error; err != nil {
			return err
		}
		if current.Status != model.TaskSummarizing {
			return errors.New("task is no longer summarizing")
		}
		// 使用 model 字段写入，确保 GORM 的 JSON serializer 正确处理字符串切片。
		audio := model.Recording{Summary: &summary.Summary, KeyPoints: summary.KeyPoints, Todos: summary.Todos}
		result := tx.Model(&model.Recording{}).Where("id = ?", task.RecordingID).Select("Summary", "KeyPoints", "Todos", "UpdatedAt").Updates(&audio)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("recording %d missing", task.RecordingID)
		}
		return tx.Model(&model.Task{}).Where("id = ?", task.ID).Updates(map[string]any{"status": model.TaskDone, "finished_at": time.Now().UTC()}).Error
	})
}
