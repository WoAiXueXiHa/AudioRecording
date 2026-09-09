package database

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"audiorecording/internal/httpapi"
	"audiorecording/internal/model"
	"audiorecording/internal/recording"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Trace 在每条 SQL 完成后调用，用它核对查询次数，不依赖肉眼数日志。
type selectCounter struct {
	logger.Interface
	selects int
}

func (l *selectCounter) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	sql, rows := fc()
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sql)), "SELECT") {
		l.selects++
	}
	l.Interface.Trace(ctx, begin, func() (string, int64) { return sql, rows }, err)
}

func testQueries(t *testing.T, db *gorm.DB) {
	counter := &selectCounter{Interface: db.Logger}
	service, err := recording.NewService(db.Session(&gorm.Session{Logger: counter}), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	router := httpapi.NewRouter(service)
	get := func(path string, status int, out any) string {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != status {
			t.Fatalf("%s: got %d: %s", path, w.Code, w.Body)
		}
		if out != nil {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatal(err)
			}
		}
		return w.Body.String()
	}
	var page recording.RecordingPage
	get("/v1/recordings", 200, &page)
	if page.Total != 0 || page.Items == nil || len(page.Items) != 0 || page.Page != 1 || page.PageSize != 20 {
		t.Fatalf("empty list: %+v", page)
	}

	// 同一时间戳的数据用于验证 id 作为第二排序键。仅操作本次新建的隔离库。
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var ids []uint64
	defer func() {
		if err := db.Where("recording_id IN ?", ids).Delete(&model.Task{}).Error; err != nil {
			t.Error(err)
		}
		if err := db.Where("id IN ?", ids).Delete(&model.Recording{}).Error; err != nil {
			t.Error(err)
		}
	}()
	for i := 0; i < 3; i++ {
		rec := model.Recording{OriginalFilename: fmt.Sprintf("query-%d.wav", i), StoragePath: "test/query", FileSize: 8, CreatedAt: stamp}
		if err := db.Create(&rec).Error; err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rec.ID)
		if err := db.Create(&model.Task{RecordingID: rec.ID}).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, size := range []int{1, 3} {
		counter.selects = 0
		get(fmt.Sprintf("/v1/recordings?page_size=%d", size), 200, &page)
		if counter.selects != 2 {
			t.Fatalf("size %d executed %d SELECTs", size, counter.selects)
		}
		if page.Total != 3 || len(page.Items) != size {
			t.Fatalf("page: %+v", page)
		}
		t.Logf("page_size=%d: %d SELECTs", size, counter.selects)
	}
	for i, item := range page.Items {
		if item.ID != ids[2-i] {
			t.Fatalf("unstable sort: %+v", page.Items)
		}
		var detail recording.RecordingDetail
		body := get(fmt.Sprintf("/v1/recordings/%d", item.ID), 200, &detail)
		if strings.Contains(body, "storage_path") {
			t.Fatal("storage path exposed")
		}
		if detail.ID != item.ID || detail.TaskID == nil || item.TaskID == nil || *detail.TaskID != *item.TaskID || detail.Status == nil || *detail.Status != model.TaskPending || detail.KeyPoints != nil {
			t.Fatalf("detail mismatch: %+v", detail)
		}
		var task recording.TaskDetail
		get(fmt.Sprintf("/v1/tasks/%d", *item.TaskID), 200, &task)
		if task.RecordingID != item.ID || task.Status != *item.Status {
			t.Fatalf("task mismatch: %+v", task)
		}
	}
	get("/v1/recordings?page=2&page_size=2", 200, &page)
	if len(page.Items) != 1 || page.Items[0].ID != ids[0] {
		t.Fatalf("second page: %+v", page)
	}
	get("/v1/recordings?page=10", 200, &page)
	if page.Items == nil || len(page.Items) != 0 || page.Total != 3 {
		t.Fatalf("beyond end: %+v", page)
	}
	rec := model.Recording{ID: ids[0], KeyPoints: []string{"one"}, Todos: []string{}}
	if err := db.Model(&rec).Select("KeyPoints", "Todos").Updates(&rec).Error; err != nil {
		t.Fatal(err)
	}
	var detail recording.RecordingDetail
	get(fmt.Sprintf("/v1/recordings/%d", rec.ID), 200, &detail)
	if len(detail.KeyPoints) != 1 || detail.KeyPoints[0] != "one" || detail.Todos == nil || len(detail.Todos) != 0 {
		t.Fatalf("JSON detail mapping: %+v", detail)
	}
	for _, path := range []string{
		"/v1/recordings/0", "/v1/tasks/-1", "/v1/tasks/abc", "/v1/recordings/18446744073709551616",
		"/v1/recordings?page=0", "/v1/recordings?page=-1", "/v1/recordings?page=1.5", "/v1/recordings?page=",
		"/v1/recordings?page=1000001", "/v1/recordings?page_size=0", "/v1/recordings?page_size=101",
		"/v1/recordings?page_size=abc", "/v1/recordings?page=1&page=2",
	} {
		counter.selects = 0
		get(path, 400, nil)
		if counter.selects != 0 {
			t.Fatalf("invalid parameters queried database: %s", path)
		}
	}
	get("/v1/tasks/18446744073709551615", 404, nil)
	get("/v1/recordings/18446744073709551615", 404, nil)
	// 执行真实 EXPLAIN 并记录优化器选择；小表可能全表扫描，不能硬断言一定走索引。
	var plan []struct {
		Table string
		Type  string
		Key   *string
		Extra string
	}
	if err := db.Raw("EXPLAIN SELECT r.id,r.original_filename,r.file_size,r.created_at,t.id AS task_id,t.status FROM recordings r LEFT JOIN tasks t ON t.recording_id=r.id ORDER BY r.created_at DESC,r.id DESC LIMIT 20").Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	for _, row := range plan {
		key := "NULL"
		if row.Key != nil {
			key = *row.Key
		}
		t.Logf("EXPLAIN table=%s type=%s key=%s extra=%s", row.Table, row.Type, key, row.Extra)
	}
}
