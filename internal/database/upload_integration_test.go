package database

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"audiorecording/internal/httpapi"
	"audiorecording/internal/model"
	"audiorecording/internal/recording"
	"gorm.io/gorm"
)

// 复用映射测试创建的隔离库；不是对业务库注入故障。
func testUpload(t *testing.T, db *gorm.DB) {
	dir := t.TempDir()
	service, err := recording.NewService(db, dir)
	if err != nil {
		t.Fatal(err)
	}
	router := httpapi.NewRouter(service)
	counts := func() [3]int64 {
		t.Helper()
		var result [3]int64
		if err := db.Model(&model.Recording{}).Count(&result[0]).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Model(&model.Task{}).Count(&result[1]).Error; err != nil {
			t.Fatal(err)
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		result[2] = int64(len(files))
		return result
	}
	request := func(filename string, size int64) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if filename != "" {
			part, err := writer.CreateFormFile("file", filename)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.CopyN(part, zeroReader{}, size); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/recordings", &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	baseline := counts()
	response := request("../../sample.WAV", 42)
	if response.Code != 201 {
		t.Fatalf("upload: %d %s", response.Code, response.Body)
	}
	var result recording.UploadResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var rec model.Recording
	var task model.Task
	if err := db.First(&rec, result.RecordingID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&task, result.TaskID).Error; err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(rec.StoragePath)
	if err != nil || len(data) != 42 || rec.FileSize != 42 || rec.OriginalFilename != "sample.WAV" || task.RecordingID != rec.ID || result.Status != model.TaskPending || task.Status != model.TaskPending {
		t.Fatalf("unexpected persisted upload: %+v %+v err=%v", rec, task, err)
	}
	expected := [3]int64{baseline[0] + 1, baseline[1] + 1, baseline[2] + 1}
	if got := counts(); got != expected {
		t.Fatalf("success counts: %v", got)
	}
	for _, tc := range []struct {
		name, filename string
		size           int64
		status         int
	}{
		{"missing", "", 0, 400}, {"extension", "sample.exe", 8, 400},
		{"empty", "sample.wav", 0, 400},
		{"file_limit", "sample.wav", recording.MaxFileBytes + 1, 413},
		{"body_limit", "sample.wav", recording.MaxFileBytes + (1 << 20) + 1, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := request(tc.filename, tc.size)
			if response.Code != tc.status {
				t.Fatalf("got %d: %s", response.Code, response.Body)
			}
			var failure struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.Error.Code == "" {
				t.Fatalf("invalid error response: %s", response.Body)
			}
			if got := counts(); got != expected {
				t.Fatalf("rejected upload left data: %v", got)
			}
		})
	}
	t.Run("exact_limit", func(t *testing.T) {
		response := request("max.mp3", recording.MaxFileBytes)
		if response.Code != 201 {
			t.Fatalf("exact limit: %d %s", response.Code, response.Body)
		}
	})
	// MySQL 触发器让第二次 INSERT 失败；验证真实事务回滚，不只模拟返回 error。
	if err := db.Exec("CREATE TRIGGER fail_task_insert BEFORE INSERT ON tasks FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected task insert failure'").Error; err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Exec("DROP TRIGGER fail_task_insert").Error; err != nil {
			t.Error(err)
		}
	}()
	beforeFailure := counts()
	response = request("rollback.wav", 16)
	if response.Code != 500 {
		t.Fatalf("injected failure: %d %s", response.Code, response.Body)
	}
	if got := counts(); got != beforeFailure {
		t.Fatalf("rollback/cleanup failed: before=%v after=%v", beforeFailure, got)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
