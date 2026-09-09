package recording_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"audiorecording/internal/recording"
)

// 网络失败与格式失败用本地 HTTP 服务复现；这里不会读取真实密钥或调用付费 API。
func TestDeepSeekClient(t *testing.T) {
	valid := `{"summary":"周五交付录音服务。","key_points":["完成接口验证"],"todos":["整理文档"]}`
	for _, tc := range []struct {
		name    string
		content string
		finish  string
		raw     string
		status  int
		code    string
	}{
		{name: "valid", content: valid},
		{name: "valid_empty_arrays", content: `{"summary":"暂无行动项。","key_points":[],"todos":[]}`},
		{name: "non_2xx", status: http.StatusTooManyRequests, raw: `{"error":"provider-private-message"}`, code: "llm_unavailable"},
		{name: "empty_body", raw: " ", code: "llm_invalid_response"},
		{name: "bad_envelope", raw: "not-json", code: "llm_invalid_response"},
		{name: "no_choices", raw: `{"choices":[]}`, code: "llm_invalid_response"},
		{name: "empty_content", content: " ", code: "llm_invalid_response"},
		{name: "bad_json", content: "not-json", code: "llm_invalid_response"},
		{name: "root_null", content: "null", code: "llm_invalid_response"},
		{name: "missing_summary", content: `{"key_points":[],"todos":[]}`, code: "llm_invalid_response"},
		{name: "missing_key_points", content: `{"summary":"x","todos":[]}`, code: "llm_invalid_response"},
		{name: "missing_todos", content: `{"summary":"x","key_points":[]}`, code: "llm_invalid_response"},
		{name: "null_summary", content: `{"summary":null,"key_points":[],"todos":[]}`, code: "llm_invalid_response"},
		{name: "empty_summary", content: `{"summary":" ","key_points":[],"todos":[]}`, code: "llm_invalid_response"},
		{name: "wrong_summary_type", content: `{"summary":12,"key_points":[],"todos":[]}`, code: "llm_invalid_response"},
		{name: "null_key_points", content: `{"summary":"x","key_points":null,"todos":[]}`, code: "llm_invalid_response"},
		{name: "null_todos", content: `{"summary":"x","key_points":[],"todos":null}`, code: "llm_invalid_response"},
		{name: "wrong_key_points_type", content: `{"summary":"x","key_points":"x","todos":[]}`, code: "llm_invalid_response"},
		{name: "wrong_todos_type", content: `{"summary":"x","key_points":[],"todos":{}}`, code: "llm_invalid_response"},
		{name: "null_array_element", content: `{"summary":"x","key_points":[null],"todos":[]}`, code: "llm_invalid_response"},
		{name: "null_todo_element", content: `{"summary":"x","key_points":[],"todos":[null]}`, code: "llm_invalid_response"},
		{name: "wrong_array_element_type", content: `{"summary":"x","key_points":[12],"todos":[]}`, code: "llm_invalid_response"},
		{name: "truncated", content: valid, finish: "length", code: "llm_invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newSummaryTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer test-only-key" {
					t.Error("unexpected endpoint, method or authorization header")
				}
				var request struct {
					Model    string `json:"model"`
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.Model != "deepseek-v4-flash" || len(request.Messages) == 0 || !strings.Contains(request.Messages[len(request.Messages)-1].Content, "转写测试内容") {
					t.Error("request is missing the configured model or transcript")
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				if tc.raw != "" {
					_, _ = w.Write([]byte(tc.raw))
					return
				}
				finish := tc.finish
				if finish == "" {
					finish = "stop"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
					"message": map[string]string{"role": "assistant", "content": tc.content}, "finish_reason": finish,
				}}})
			}))
			defer server.Close()
			client, err := recording.NewDeepSeekClient("test-only-key", server.URL, "deepseek-v4-flash")
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Summarize(context.Background(), "转写测试内容")
			if tc.code != "" {
				assertSummaryError(t, err, tc.code)
				if strings.Contains(err.Error(), "provider-private-message") {
					t.Fatal("provider response leaked through public error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary == "" || result.KeyPoints == nil || result.Todos == nil {
				t.Fatalf("unexpected summary result: %+v", result)
			}
			if tc.name == "valid_empty_arrays" && (len(result.KeyPoints) != 0 || len(result.Todos) != 0) {
				t.Fatal("empty arrays changed during decoding")
			}
		})
	}

	t.Run("timeout", func(t *testing.T) {
		server := newSummaryTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 给网络超时留一个确定的窗口；不等待客户端 context 以避免 Close 卡住。
			time.Sleep(100 * time.Millisecond)
			_, _ = w.Write([]byte(`{}`))
		}))
		defer server.Close()
		client, err := recording.NewDeepSeekClient("test-only-key", server.URL, "deepseek-v4-flash")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err = client.Summarize(ctx, "转写测试内容")
		assertSummaryError(t, err, "llm_timeout")
	})
}

func assertSummaryError(t *testing.T, err error, code string) {
	t.Helper()
	var summaryErr *recording.SummaryError
	if !errors.As(err, &summaryErr) || summaryErr.Code != code {
		t.Fatalf("want summary error %s, got %v", code, err)
	}
}

// 部分 WSL 环境对 IPv4 回环监听的映射存在延迟，优先使用 IPv6 回环。
// 不支持 IPv6 的系统仍使用 httptest 默认的 IPv4 监听器。
func newSummaryTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	listener, err := net.Listen("tcp", "[::1]:0")
	if err == nil {
		server.Listener.Close()
		server.Listener = listener
	}
	server.Start()
	return server
}
