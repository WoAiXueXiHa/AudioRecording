package database

import (
	"context"
	"testing"
	"time"

	"audiorecording/internal/model"
	"audiorecording/internal/recording"
	"gorm.io/gorm"
)

type summarizeFunc func(context.Context, string) (recording.SummaryResult, error)

func (f summarizeFunc) Summarize(ctx context.Context, transcript string) (recording.SummaryResult, error) {
	return f(ctx, transcript)
}

func testSummaryRunner(t *testing.T, db *gorm.DB) {
	for _, tc := range []struct {
		name       string
		llmCode    string
		rollback   bool
		wantStatus string
		wantCode   string
	}{
		{name: "done", wantStatus: model.TaskDone},
		{name: "timeout", llmCode: "llm_timeout", wantStatus: model.TaskFailed, wantCode: "llm_timeout"},
		{name: "invalid_response", llmCode: "llm_invalid_response", wantStatus: model.TaskFailed, wantCode: "llm_invalid_response"},
		{name: "unavailable", llmCode: "llm_unavailable", wantStatus: model.TaskFailed, wantCode: "llm_unavailable"},
		{name: "done_update_rolls_back_summary", rollback: true, wantStatus: model.TaskFailed, wantCode: "database_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := model.Recording{OriginalFilename: "summary.wav", StoragePath: "test/summary.wav", FileSize: 8}
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
			if tc.rollback {
				// 结果写入后令 done 更新失败，验证结果不会在事务外提前提交。
				if err := db.Exec("CREATE TRIGGER fail_done BEFORE UPDATE ON tasks FOR EACH ROW BEGIN IF NEW.status = 'done' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected done update failure'; END IF; END").Error; err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := db.Exec("DROP TRIGGER fail_done").Error; err != nil {
						t.Error(err)
					}
				})
			}
			service, err := recording.NewService(db, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			asr := transcribeFunc(func(context.Context, string) (string, error) { return "周五交付，先验证接口。", nil })
			llm := summarizeFunc(func(_ context.Context, transcript string) (recording.SummaryResult, error) {
				if transcript != "周五交付，先验证接口。" {
					t.Error("LLM did not receive saved transcript")
				}
				var stage model.Task
				if err := db.First(&stage, task.ID).Error; err != nil {
					t.Error(err)
				}
				if stage.Status != model.TaskSummarizing {
					t.Errorf("LLM called before summarizing stage: %s", stage.Status)
				}
				if tc.llmCode != "" {
					return recording.SummaryResult{}, &recording.SummaryError{Code: tc.llmCode}
				}
				return recording.SummaryResult{Summary: "周五交付服务。", KeyPoints: []string{"验证接口"}, Todos: []string{}}, nil
			})
			runner := recording.NewRunner(service, asr, llm)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				runner.Run(ctx)
				runner.Wait()
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("summary runner did not stop")
				}
			})
			deadline := time.Now().Add(5 * time.Second)
			var saved model.Task
			for time.Now().Before(deadline) {
				if err := db.First(&saved, task.ID).Error; err != nil {
					t.Fatal(err)
				}
				if saved.Status == tc.wantStatus {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if saved.Status != tc.wantStatus || saved.StartedAt == nil || saved.FinishedAt == nil {
				t.Fatalf("unexpected terminal task: %+v", saved)
			}
			var result model.Recording
			if err := db.First(&result, rec.ID).Error; err != nil {
				t.Fatal(err)
			}
			if result.Transcript == nil || *result.Transcript != "周五交付，先验证接口。" {
				t.Fatal("summary processing lost the successful transcript")
			}
			if tc.wantCode != "" {
				if saved.FailedStage == nil || *saved.FailedStage != model.TaskSummarizing || saved.ErrorCode == nil || *saved.ErrorCode != tc.wantCode || saved.ErrorMessage == nil || *saved.ErrorMessage == "" {
					t.Fatalf("unexpected summary failure: %+v", saved)
				}
				if result.Summary != nil || result.KeyPoints != nil || result.Todos != nil {
					t.Fatalf("failed summary left partial results: %+v", result)
				}
				return
			}
			if result.Summary == nil || *result.Summary != "周五交付服务。" || len(result.KeyPoints) != 1 || result.KeyPoints[0] != "验证接口" || result.Todos == nil || len(result.Todos) != 0 || saved.ErrorCode != nil {
				t.Fatalf("completed summary fields do not match: %+v, %+v", result, saved)
			}
		})
	}
}
