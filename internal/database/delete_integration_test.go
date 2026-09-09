package database

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"audiorecording/internal/httpapi"
	"audiorecording/internal/model"
	"audiorecording/internal/recording"
	"gorm.io/gorm"
)

func testDelete(t *testing.T, db *gorm.DB) {
	service, err := recording.NewService(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	router := httpapi.NewRouter(service)
	request := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	seed := func(t *testing.T, status string) (model.Recording, model.Task) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "delete.wav")
		if err := os.WriteFile(path, []byte("audio"), 0600); err != nil {
			t.Fatal(err)
		}
		rec := model.Recording{OriginalFilename: "delete.wav", StoragePath: path, FileSize: 5}
		if err := db.Create(&rec).Error; err != nil {
			t.Fatal(err)
		}
		task := model.Task{RecordingID: rec.ID, Status: status}
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
	assertRows := func(t *testing.T, rec model.Recording, task model.Task, want int64) {
		t.Helper()
		var records, tasks int64
		if err := db.Model(&model.Recording{}).Where("id = ?", rec.ID).Count(&records).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Model(&model.Task{}).Where("id = ?", task.ID).Count(&tasks).Error; err != nil {
			t.Fatal(err)
		}
		if records != want || tasks != want {
			t.Fatalf("rows: recordings=%d tasks=%d want=%d", records, tasks, want)
		}
	}
	remove := func(id uint64) *httptest.ResponseRecorder {
		return request(http.MethodDelete, fmt.Sprintf("/v1/recordings/%d", id))
	}
	for _, status := range []string{model.TaskDone, model.TaskFailed} {
		t.Run("terminal_"+status, func(t *testing.T) {
			rec, task := seed(t, status)
			response := remove(rec.ID)
			if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
				t.Fatalf("delete: %d %s", response.Code, response.Body)
			}
			assertRows(t, rec, task, 0)
			if _, err := os.Stat(rec.StoragePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("audio remains: %v", err)
			}
			response = remove(rec.ID)
			if response.Code != http.StatusNotFound {
				t.Fatalf("repeat delete: %d", response.Code)
			}
			assertErrorJSON(t, response)
		})
	}
	for _, status := range []string{model.TaskPending, model.TaskTranscribing, model.TaskSummarizing} {
		t.Run("active_"+status, func(t *testing.T) {
			rec, task := seed(t, status)
			response := remove(rec.ID)
			if response.Code != http.StatusConflict {
				t.Fatalf("active delete: %d %s", response.Code, response.Body)
			}
			assertErrorJSON(t, response)
			assertRows(t, rec, task, 1)
			if _, err := os.Stat(rec.StoragePath); err != nil {
				t.Fatalf("active audio lost: %v", err)
			}
		})
	}
	t.Run("missing_file", func(t *testing.T) {
		rec, task := seed(t, model.TaskFailed)
		if err := os.Remove(rec.StoragePath); err != nil {
			t.Fatal(err)
		}
		response := remove(rec.ID)
		if response.Code != http.StatusNoContent {
			t.Fatalf("missing file: %d %s", response.Code, response.Body)
		}
		assertRows(t, rec, task, 0)
	})
	t.Run("file_remove_failure_preserves_rows", func(t *testing.T) {
		rec, task := seed(t, model.TaskFailed)
		if err := os.Remove(rec.StoragePath); err != nil {
			t.Fatal(err)
		}
		// 非空目录可稳定触发 Remove 失败，不依赖运行账号是否有 root 权限。
		if err := os.Mkdir(rec.StoragePath, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rec.StoragePath, "child"), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		response := remove(rec.ID)
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("remove failure: %d %s", response.Code, response.Body)
		}
		assertErrorJSON(t, response)
		assertRows(t, rec, task, 1)
	})
	t.Run("database_failure_can_retry_delete", func(t *testing.T) {
		rec, task := seed(t, model.TaskFailed)
		if err := db.Exec("CREATE TRIGGER fail_recording_delete BEFORE DELETE ON recordings FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected recording delete failure'").Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := db.Exec("DROP TRIGGER IF EXISTS fail_recording_delete").Error; err != nil {
				t.Error(err)
			}
		})
		response := remove(rec.ID)
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("database failure: %d %s", response.Code, response.Body)
		}
		assertErrorJSON(t, response)
		assertRows(t, rec, task, 1)
		if _, err := os.Stat(rec.StoragePath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected file-first removal: %v", err)
		}
		if err := db.Exec("DROP TRIGGER fail_recording_delete").Error; err != nil {
			t.Fatal(err)
		}
		response = remove(rec.ID)
		if response.Code != http.StatusNoContent {
			t.Fatalf("retry after database failure: %d %s", response.Code, response.Body)
		}
		assertRows(t, rec, task, 0)
	})
	t.Run("delete_retry_race", func(t *testing.T) {
		rec, task := seed(t, model.TaskFailed)
		gate := make(chan struct{})
		deletion, retry := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
		go func() { <-gate; deletion <- remove(rec.ID) }()
		go func() { <-gate; retry <- request(http.MethodPost, fmt.Sprintf("/v1/tasks/%d/retry", task.ID)) }()
		close(gate)
		d, r := <-deletion, <-retry
		switch {
		case d.Code == http.StatusNoContent && r.Code == http.StatusNotFound:
			assertRows(t, rec, task, 0)
			if _, err := os.Stat(rec.StoragePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("deleted audio still exists")
			}
		case d.Code == http.StatusConflict && r.Code == http.StatusAccepted:
			assertRows(t, rec, task, 1)
			if _, err := os.Stat(rec.StoragePath); err != nil {
				t.Fatal("accepted retry lost audio")
			}
		default:
			t.Fatalf("illegal competing outcome: delete=%d %s retry=%d %s", d.Code, d.Body, r.Code, r.Body)
		}
	})
	t.Run("invalid_id", func(t *testing.T) {
		for _, id := range []string{"0", "abc", "18446744073709551616"} {
			response := request(http.MethodDelete, "/v1/recordings/"+id)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("id %s: %d", id, response.Code)
			}
			assertErrorJSON(t, response)
		}
	})
}
