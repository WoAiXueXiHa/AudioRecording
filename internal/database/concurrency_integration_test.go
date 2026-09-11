package database

import (
	"audiorecording/internal/model"
	"audiorecording/internal/recording"
	"context"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"testing"
	"time"
)

func testConcurrency(t *testing.T, db *gorm.DB) {
	for _, outcome := range []string{"success", "error", "panic", "claim_error"} {
		t.Run(outcome, func(t *testing.T) {
			service, err := recording.NewService(db, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var records []model.Recording
			var tasks []model.Task
			for i := 0; i < 4; i++ {
				rec := model.Recording{OriginalFilename: "slot.wav", StoragePath: fmt.Sprint(i), FileSize: 1}
				if err := db.Create(&rec).Error; err != nil {
					t.Fatal(err)
				}
				task := model.Task{RecordingID: rec.ID, Status: model.TaskPending}
				if err := db.Create(&task).Error; err != nil {
					t.Fatal(err)
				}
				records = append(records, rec)
				tasks = append(tasks, task)
			}
			defer func() {
				for _, task := range tasks {
					db.Delete(&task)
				}
				for _, rec := range records {
					db.Delete(&rec)
				}
			}()
			entered := make(chan struct{}, 4)
			release := make(chan struct{})
			runner, err := recording.NewRunner(service, transcribeFunc(func(context.Context, string) (string, error) { return "text", nil }), 3,
				summarizeFunc(func(ctx context.Context, _ string) (recording.SummaryResult, error) {
					entered <- struct{}{}
					select {
					case <-release:
					case <-ctx.Done():
						return recording.SummaryResult{}, ctx.Err()
					}
					if outcome == "panic" {
						panic("test")
					}
					if outcome == "error" {
						return recording.SummaryResult{}, errors.New("test")
					}
					return recording.SummaryResult{Summary: "ok", KeyPoints: []string{}, Todos: []string{}}, nil
				}))
			if err != nil {
				t.Fatal(err)
			}
			if outcome == "claim_error" {
				if err := db.Exec("CREATE TRIGGER fail_slot_claim BEFORE UPDATE ON tasks FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'claim failure'").Error; err != nil {
					t.Fatal(err)
				}
				defer db.Exec("DROP TRIGGER IF EXISTS fail_slot_claim")
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); runner.Run(ctx); runner.Wait() }()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(8 * time.Second):
					t.Error("shutdown timeout")
				}
			}()
			if outcome == "claim_error" {
				select {
				case <-entered:
					t.Fatal("claim unexpectedly succeeded")
				case <-time.After(1200 * time.Millisecond):
				}
				if err := db.Exec("DROP TRIGGER fail_slot_claim").Error; err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 3; i++ {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("workers did not enter summary")
				}
			}
			// 跨过至少一个轮询周期，验证摘要仍占槽，第四条不提前领取。
			select {
			case <-entered:
				t.Fatal("concurrency exceeded 3")
			case <-time.After(1200 * time.Millisecond):
			}
			var fourth model.Task
			if err := db.First(&fourth, tasks[3].ID).Error; err != nil {
				t.Fatal(err)
			}
			if fourth.Status != model.TaskPending {
				t.Fatalf("fourth status=%s", fourth.Status)
			}
			release <- struct{}{}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("slot not released")
			}
			// 剩余阻塞任务在取消后必须全部收尾。
		})
	}
}
