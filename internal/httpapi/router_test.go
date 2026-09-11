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
