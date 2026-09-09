package database

import (
	"context"
	"reflect"
	"testing"
	"time"

	"audiorecording/internal/model"
	"audiorecording/internal/recording"
	"gorm.io/gorm"
)

func testInterruptTasks(t *testing.T, db *gorm.DB) {
	service, err := recording.NewService(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tasks := make(map[string]model.Task)
	records := make(map[string]model.Recording)
	for _, status := range []string{model.TaskPending, model.TaskTranscribing, model.TaskSummarizing, model.TaskDone, model.TaskFailed} {
		transcript, summary := "saved transcript", "saved summary"
		rec := model.Recording{OriginalFilename: "restart.wav", StoragePath: "test/restart.wav", FileSize: 8, Transcript: &transcript, Summary: &summary, KeyPoints: []string{"saved"}, Todos: []string{}}
		if err := db.Create(&rec).Error; err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().UTC().Add(-time.Minute)
		task := model.Task{RecordingID: rec.ID, Status: status, RetryCount: 2}
		if status != model.TaskPending {
			task.StartedAt = &stamp
		}
		if status == model.TaskDone || status == model.TaskFailed {
			task.FinishedAt = &stamp
		}
		if status == model.TaskFailed {
			stage, code, message := model.TaskTranscribing, "transcription_failed", "old error"
			task.FailedStage, task.ErrorCode, task.ErrorMessage = &stage, &code, &message
		}
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
		// 以数据库往返后的时间精度为基线，避免毫秒列与 Go 纳秒时间的无关差异。
		var baselineTask model.Task
		var baselineRec model.Recording
		if err := db.First(&baselineTask, task.ID).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.First(&baselineRec, rec.ID).Error; err != nil {
			t.Fatal(err)
		}
		tasks[status], records[status] = baselineTask, baselineRec
	}
	if err := service.InterruptTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	for status, before := range tasks {
		var after model.Task
		if err := db.First(&after, before.ID).Error; err != nil {
			t.Fatal(err)
		}
		if status == model.TaskTranscribing || status == model.TaskSummarizing {
			if after.Status != model.TaskFailed || after.FailedStage == nil || *after.FailedStage != status || after.ErrorCode == nil || *after.ErrorCode != "service_interrupted" || after.ErrorMessage == nil || *after.ErrorMessage != "processing interrupted by service restart" || after.FinishedAt == nil || after.StartedAt == nil || !after.StartedAt.Equal(*before.StartedAt) || after.RetryCount != before.RetryCount {
				t.Fatalf("incorrect interrupted task: before=%+v after=%+v", before, after)
			}
		} else if !reflect.DeepEqual(before, after) {
			t.Fatalf("startup changed %s task: before=%+v after=%+v", status, before, after)
		}
		var audio model.Recording
		if err := db.First(&audio, before.RecordingID).Error; err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(records[status], audio) {
			t.Fatalf("startup changed saved results for %s", status)
		}
	}

	// pending 仍由普通轮询执行；已中断任务必须经过手动 Retry 才能再次执行。
	runner := recording.NewRunner(service,
		transcribeFunc(func(context.Context, string) (string, error) { return "fresh transcript", nil }),
		summarizeFunc(func(context.Context, string) (recording.SummaryResult, error) {
			return recording.SummaryResult{Summary: "fresh summary", KeyPoints: []string{}, Todos: []string{}}, nil
		}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); runner.Run(ctx); runner.Wait() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("restart test runner did not stop")
		}
	})
	waitDone := func(id uint64) model.Task {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var task model.Task
			if err := db.First(&task, id).Error; err != nil {
				t.Fatal(err)
			}
			if task.Status == model.TaskDone {
				return task
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("task %d did not complete", id)
		return model.Task{}
	}
	waitDone(tasks[model.TaskPending].ID)
	interrupted := tasks[model.TaskTranscribing]
	var beforeRetry model.Task
	if err := db.First(&beforeRetry, interrupted.ID).Error; err != nil {
		t.Fatal(err)
	}
	if beforeRetry.Status != model.TaskFailed {
		t.Fatal("interrupted task ran automatically")
	}
	result, err := service.Retry(context.Background(), interrupted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.TaskID != interrupted.ID || result.Status != model.TaskPending {
		t.Fatalf("unexpected retry: %+v", result)
	}
	retried := waitDone(interrupted.ID)
	if retried.RetryCount != interrupted.RetryCount+1 || retried.ErrorCode != nil {
		t.Fatalf("incorrect retried task: %+v", retried)
	}
}
