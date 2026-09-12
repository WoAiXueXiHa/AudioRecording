package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 无效 ID 应到达对应 handler 并被拒绝，不需要连接数据库。
func TestMutationRoutesRejectInvalidID(t *testing.T) {
	router := NewRouter(nil)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/tasks/invalid/retry"},
		{http.MethodDelete, "/v1/recordings/invalid"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", response.Code, response.Body.String())
			}
			var body errorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != "invalid_id" {
				t.Fatalf("error code = %q, want invalid_id", body.Error.Code)
			}
		})
	}
}

// 页面资源必须随二进制可用，同时保留 API 的 JSON 404。
func TestWebAssets(t *testing.T) {
	router := NewRouter(nil)
	for _, tc := range []struct{ path, contentType string }{
		{"/", "text/html; charset=utf-8"},
		{"/assets/style.css", "text/css; charset=utf-8"},
		{"/assets/app.js", "text/javascript; charset=utf-8"},
	} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if response.Code != 200 || response.Header().Get("Content-Type") != tc.contentType || response.Body.Len() == 0 {
			t.Fatalf("asset %s: status=%d type=%s", tc.path, response.Code, response.Header().Get("Content-Type"))
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/not-found", nil))
	var body errorResponse
	if response.Code != 404 || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != "not_found" {
		t.Fatalf("unknown route must return JSON 404: %s", response.Body.String())
	}
}
