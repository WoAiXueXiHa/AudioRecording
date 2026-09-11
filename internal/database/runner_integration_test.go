package database

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"audiorecording/internal/model"
	"audiorecording/internal/recording"
	"gorm.io/gorm"
)

// 用可控的转写器验证后台边界，不靠随机失败概率或真实的 5～15 秒等待。
type transcribeFunc func(context.Context, string) (string, error)

func (f transcribeFunc) Transcribe(ctx context.Context, path string) (string, error) {
	return f(ctx, path)
}

func testRunner(t *testing.T, db *gorm.DB) {
	service, err := recording.NewService(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	createTask := func(t *testing.T) (model.Recording, model.Task) {
		t.Helper()
		rec := model.Recording{OriginalFilename: "worker.wav", StoragePath: "test/worker.wav", FileSize: 8}
		if err := db.Create(&rec).Error; err != nil {
			t.Fatal(err)
		}
		task := model.Task{RecordingID: rec.ID, Status: model.TaskPending}
		if err := db.Create(&task).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := db.Delete(&task).Error; err != nil {
				t.Error(err)
			}
			if err := db.Delete(&rec).Error; err != nil {
				t.Error(err)
			}
		})
		return rec, task
	}
	start := func(t *testing.T, asr recording.Transcriber) context.CancelFunc {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		runner, err := recording.NewRunner(service, asr, 3)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			runner.Run(ctx)
			runner.Wait()
		}()
		// 后注册的清理先执行：必须等后台结束，再删除本例的数据库记录。
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("runner did not stop after cancellation")
			}
		})
		return cancel
	}
	waitTask := func(t *testing.T, id uint64, status string) model.Task {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var got model.Task
			if err := db.First(&got, id).Error; err != nil {
				t.Fatal(err)
			}
			if got.Status == status {
				return got
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("task %d did not reach %s", id, status)
		return model.Task{}
	}

	t.Run("claim_once_and_commit_transcript", func(t *testing.T) {
		rec, task := createTask(t)
		var calls atomic.Int32
		release := make(chan struct{})
		asr := transcribeFunc(func(ctx context.Context, path string) (string, error) {
			calls.Add(1)
			if path != rec.StoragePath {
				return "", errors.New("unexpected storage path")
			}
			select {
			case <-release:
				return "会议决定周五交付录音服务。", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		})
		start(t, asr)
		start(t, asr)
		active := waitTask(t, task.ID, model.TaskTranscribing)
		if active.StartedAt == nil || active.FinishedAt != nil {
			t.Fatalf("unexpected processing timestamps: %+v", active)
		}
		close(release)
		waitTask(t, task.ID, model.TaskSummarizing)
		if calls.Load() != 1 {
			t.Fatalf("two runners processed task %d times", calls.Load())
		}
		// 单条联表查询读取同一数据库快照，阶段推进不能早于转写结果落库。
		var result struct {
			Status     string
			Transcript *string
		}
		if err := db.Table("tasks t").Select("t.status, r.transcript").Joins("JOIN recordings r ON r.id = t.recording_id").Where("t.id = ?", task.ID).Take(&result).Error; err != nil {
			t.Fatal(err)
		}
		if result.Status != model.TaskSummarizing || result.Transcript == nil || *result.Transcript != "会议决定周五交付录音服务。" {
			t.Fatalf("transcript/status mismatch: %+v", result)
		}
	})

	t.Run("stage_update_failure_rolls_back_transcript", func(t *testing.T) {
		rec, task := createTask(t)
		// 让事务中的第二次写入失败，证明不能留下只有 transcript 的半成品。
		if err := db.Exec("CREATE TRIGGER fail_summarizing BEFORE UPDATE ON tasks FOR EACH ROW BEGIN IF NEW.status = 'summarizing' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected stage update failure'; END IF; END").Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := db.Exec("DROP TRIGGER fail_summarizing").Error; err != nil {
				t.Error(err)
			}
		})
		start(t, transcribeFunc(func(context.Context, string) (string, error) { return "must roll back", nil }))
		failed := waitTask(t, task.ID, model.TaskFailed)
		assertTranscriptionFailure(t, failed, "database_error")
		var saved model.Recording
		if err := db.First(&saved, rec.ID).Error; err != nil {
			t.Fatal(err)
		}
		if saved.Transcript != nil {
			t.Fatal("stage update failed but transcript was committed")
		}
	})

	for _, tc := range []struct {
		name string
		code string
		asr  transcribeFunc
	}{
		{"transcription_failure", "transcription_failed", func(context.Context, string) (string, error) { return "", errors.New("injected transcription failure") }},
		{"panic", "worker_panic", func(context.Context, string) (string, error) { panic("injected worker panic") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, task := createTask(t)
			start(t, tc.asr)
			failed := waitTask(t, task.ID, model.TaskFailed)
			assertTranscriptionFailure(t, failed, tc.code)
			var saved model.Recording
			if err := db.First(&saved, rec.ID).Error; err != nil {
				t.Fatal(err)
			}
			if saved.Transcript != nil {
				t.Fatal("failed transcription saved a result")
			}
		})
	}

	t.Run("cancellation_persists_failure", func(t *testing.T) {
		_, task := createTask(t)
		entered := make(chan struct{})
		cancel := start(t, transcribeFunc(func(ctx context.Context, _ string) (string, error) {
			close(entered)
			<-ctx.Done()
			return "", ctx.Err()
		}))
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("transcriber was not called")
		}
		cancel()
		failed := waitTask(t, task.ID, model.TaskFailed)
		assertTranscriptionFailure(t, failed, "transcription_failed")
	})
}

func assertTranscriptionFailure(t *testing.T, task model.Task, code string) {
	t.Helper()
	if task.FailedStage == nil || *task.FailedStage != model.TaskTranscribing || task.ErrorCode == nil || *task.ErrorCode != code || task.ErrorMessage == nil || *task.ErrorMessage == "" || task.StartedAt == nil || task.FinishedAt == nil {
		t.Fatalf("incomplete failure information: %+v", task)
	}
}
