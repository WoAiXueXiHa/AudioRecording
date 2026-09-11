package database

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"audiorecording/internal/httpapi"
	"audiorecording/internal/model"
	"audiorecording/internal/recording"
	"gorm.io/gorm"
)

func testRetry(t *testing.T, db *gorm.DB) {
	service, err := recording.NewService(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	router := httpapi.NewRouter(service)
	seed := func(t *testing.T, status string) (model.Recording, model.Task) {
		t.Helper()
		text, code, stage := "old result", "llm_timeout", model.TaskSummarizing
		stamp := time.Now().UTC().Add(-time.Minute)
		rec := model.Recording{OriginalFilename: "retry.wav", StoragePath: "test/retry.wav", FileSize: 8, Transcript: &text, Summary: &text, KeyPoints: []string{"old"}, Todos: []string{"old"}}
		if err := db.Create(&rec).Error; err != nil {
			t.Fatal(err)
		}
		task := model.Task{RecordingID: rec.ID, Status: status, RetryCount: 2, FailedStage: &stage, ErrorCode: &code, ErrorMessage: &text, StartedAt: &stamp, FinishedAt: &stamp}
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
	post := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		return w
	}

	t.Run("concurrent_retry_clears_and_runs_again", func(t *testing.T) {
		rec, task := seed(t, model.TaskFailed)
		responses := make(chan *httptest.ResponseRecorder, 8)
		var callers sync.WaitGroup
		gate := make(chan struct{})
		for i := 0; i < cap(responses); i++ {
			callers.Add(1)
			go func() {
				defer callers.Done()
				<-gate
				responses <- post(fmt.Sprintf("/v1/tasks/%d/retry", task.ID))
			}()
		}
		close(gate)
		callers.Wait()
		close(responses)
		accepted := 0
		for response := range responses {
			switch response.Code {
			case http.StatusAccepted:
				accepted++
				var result recording.UploadResult
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.TaskID != task.ID || result.RecordingID != rec.ID || result.Status != model.TaskPending {
					t.Fatalf("unexpected retry response: %+v", result)
				}
			case http.StatusConflict:
				assertErrorJSON(t, response)
			default:
				t.Fatalf("unexpected retry response: %d %s", response.Code, response.Body)
			}
		}
		if accepted != 1 {
			t.Fatalf("accepted %d concurrent retries", accepted)
		}
		var saved model.Task
		if err := db.First(&saved, task.ID).Error; err != nil {
			t.Fatal(err)
		}
		if saved.Status != model.TaskPending || saved.RetryCount != 3 || saved.FailedStage != nil || saved.ErrorCode != nil || saved.ErrorMessage != nil || saved.StartedAt != nil || saved.FinishedAt != nil {
			t.Fatalf("retry did not reset task: %+v", saved)
		}
		// SQL NULL 与空数组不同，直接核对数据库列，避免 serializer 掩盖残留值。
		var cleared int64
		if err := db.Model(&model.Recording{}).Where("id = ? AND transcript IS NULL AND summary IS NULL AND key_points IS NULL AND todos IS NULL", rec.ID).Count(&cleared).Error; err != nil {
			t.Fatal(err)
		}
		if cleared != 1 {
			t.Fatal("retry left stale recording results")
		}
		runner, err := recording.NewRunner(service,
			transcribeFunc(func(context.Context, string) (string, error) { return "new transcript", nil }), 3,
			summarizeFunc(func(context.Context, string) (recording.SummaryResult, error) {
				return recording.SummaryResult{Summary: "new summary", KeyPoints: []string{}, Todos: []string{}}, nil
			}),
		)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); runner.Run(ctx); runner.Wait() }()
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("retry runner did not stop")
			}
		})
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			saved = model.Task{}
			if err := db.First(&saved, task.ID).Error; err != nil {
				t.Fatal(err)
			}
			if saved.Status == model.TaskDone {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if saved.Status != model.TaskDone || saved.RetryCount != 3 {
			t.Fatalf("retry was not processed: %+v", saved)
		}
		var fresh model.Recording
		if err := db.First(&fresh, rec.ID).Error; err != nil {
			t.Fatal(err)
		}
		if fresh.Transcript == nil || *fresh.Transcript != "new transcript" || fresh.Summary == nil || *fresh.Summary != "new summary" {
			t.Fatalf("retry did not replace results: %+v", fresh)
		}
	})

	t.Run("reject_invalid_state_and_id", func(t *testing.T) {
		for _, status := range []string{model.TaskPending, model.TaskTranscribing, model.TaskSummarizing, model.TaskDone} {
			_, task := seed(t, status)
			response := post(fmt.Sprintf("/v1/tasks/%d/retry", task.ID))
			if response.Code != http.StatusConflict {
				t.Fatalf("retry %s: %d %s", status, response.Code, response.Body)
			}
			assertErrorJSON(t, response)
			var saved model.Task
			if err := db.First(&saved, task.ID).Error; err != nil {
				t.Fatal(err)
			}
			if saved.RetryCount != task.RetryCount || saved.Status != status {
				t.Fatal("rejected retry changed task")
			}
		}
		for _, tc := range []struct {
			id     string
			status int
		}{{"0", 400}, {"abc", 400}, {"18446744073709551616", 400}, {"18446744073709551615", 404}} {
			response := post("/v1/tasks/" + tc.id + "/retry")
			if response.Code != tc.status {
				t.Fatalf("retry id %s: %d", tc.id, response.Code)
			}
			assertErrorJSON(t, response)
		}
	})

	t.Run("reset_failure_rolls_back", func(t *testing.T) {
		rec, task := seed(t, model.TaskFailed)
		if err := db.Exec("CREATE TRIGGER fail_retry BEFORE UPDATE ON tasks FOR EACH ROW BEGIN IF NEW.status = 'pending' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected retry failure'; END IF; END").Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := db.Exec("DROP TRIGGER fail_retry").Error; err != nil {
				t.Error(err)
			}
		})
		response := post(fmt.Sprintf("/v1/tasks/%d/retry", task.ID))
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("injected failure: %d %s", response.Code, response.Body)
		}
		assertErrorJSON(t, response)
		var saved model.Task
		if err := db.First(&saved, task.ID).Error; err != nil {
			t.Fatal(err)
		}
		var savedRec model.Recording
		if err := db.First(&savedRec, rec.ID).Error; err != nil {
			t.Fatal(err)
		}
		if saved.Status != model.TaskFailed || saved.RetryCount != 2 || saved.ErrorCode == nil || saved.StartedAt == nil || saved.FinishedAt == nil || savedRec.Transcript == nil || *savedRec.Transcript != "old result" || savedRec.Summary == nil || len(savedRec.KeyPoints) != 1 || len(savedRec.Todos) != 1 {
			t.Fatalf("retry failure did not roll back: %+v %+v", saved, savedRec)
		}
	})
}

func assertErrorJSON(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code == "" || body.Error.Message == "" {
		t.Fatalf("invalid error response: %s", response.Body)
	}
}
