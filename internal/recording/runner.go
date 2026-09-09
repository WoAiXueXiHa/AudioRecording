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
	workers sync.WaitGroup
}

func NewRunner(service *Service, asr Transcriber) *Runner {
	return &Runner{service: service, asr: asr}
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
		now := time.Now().UTC()
		// 查询和领取不是一个操作：再次限定 pending，只有 RowsAffected=1 才获得执行权。
		claimed := r.service.db.WithContext(dbCtx).Model(&model.Task{}).
			Where("id = ? AND status = ?", task.ID, model.TaskPending).
			Updates(map[string]any{"status": model.TaskTranscribing, "started_at": now})
		if claimed.Error != nil {
			log.Printf("task claim failed task_id=%d error=%v", task.ID, claimed.Error)
			continue
		}
		if claimed.RowsAffected != 1 {
			continue
		}
		log.Printf("task transition recording_id=%d task_id=%d from=pending to=transcribing", task.RecordingID, task.ID)
		r.workers.Add(1)
		go func() {
			defer r.workers.Done()
			r.transcribe(ctx, task)
		}()
	}
}

func (r *Runner) transcribe(ctx context.Context, task model.Task) {
	// Gin 的 recovery 只保护 HTTP goroutine，后台任务需要自己的 panic 兜底。
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("task panic task_id=%d error=%v stack=%s", task.ID, recovered, debug.Stack())
			r.fail(task, "worker_panic", "background task panicked")
		}
	}()
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	var audio model.Recording
	err := r.service.db.WithContext(dbCtx).Where("id = ?", task.RecordingID).Take(&audio).Error
	cancel()
	if err != nil {
		log.Printf("task read recording failed task_id=%d error=%v", task.ID, err)
		r.fail(task, "database_error", "cannot read recording")
		return
	}
	transcript, err := r.asr.Transcribe(ctx, audio.StoragePath)
	if err != nil {
		log.Printf("task transcription failed task_id=%d error=%v", task.ID, err)
		r.fail(task, "transcription_failed", "audio transcription failed")
		return
	}
	if err := r.saveTranscript(ctx, task, transcript); err != nil {
		log.Printf("task save transcript failed task_id=%d error=%v", task.ID, err)
		r.fail(task, "database_error", "cannot save transcript")
		return
	}
	log.Printf("task transition recording_id=%d task_id=%d from=transcribing to=summarizing", task.RecordingID, task.ID)
	// 当前功能提交到转写为止；真实 LLM 摘要在下一功能提交接入，不伪造完成结果。
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

func (r *Runner) fail(task model.Task, code, message string) {
	// 服务 context 已取消时仍给失败状态一次短暂的落库机会；不自动重试外部调用。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := r.service.db.WithContext(ctx).Model(&model.Task{}).
		Where("id = ? AND status = ?", task.ID, model.TaskTranscribing).
		Updates(map[string]any{
			"status": model.TaskFailed, "failed_stage": model.TaskTranscribing,
			"error_code": code, "error_message": message, "finished_at": time.Now().UTC(),
		})
	if result.Error != nil {
		log.Printf("task failure write failed recording_id=%d task_id=%d code=%s error=%v", task.RecordingID, task.ID, code, result.Error)
		return
	}
	if result.RowsAffected == 1 {
		log.Printf("task transition recording_id=%d task_id=%d from=transcribing to=failed code=%s", task.RecordingID, task.ID, code)
	}
}
