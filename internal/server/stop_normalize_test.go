package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// stopCaptureUpstream 假上游：把收到的请求体读出来（自动解 gzip），其余同 newFakeUpstream。
func stopCaptureUpstream(t *testing.T, got *[]byte) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			*got = readUpstreamBody(r)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

func runStopCase(t *testing.T, body string) map[string]any {
	t.Helper()
	var sent []byte
	up := stopCaptureUpstream(t, &sent)
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var obj map[string]any
	if err := json.Unmarshal(sent, &obj); err != nil {
		t.Fatalf("上游收到的 body 不是 JSON: %v (%s)", err, sent)
	}
	return obj
}

// TestChatStopStringNormalizedToArray OpenAI 规范里 stop 是 string | string[]，但上游
// Go 端字段是 []string，收到字符串会 400：
//
//	{"code":11101,"msg":"...cannot unmarshal string into Go struct field Request.stop of type []string"}
//
// 这类 400 全部由这一个原因引起。网关在出站前把字符串包成单元素数组。
func TestChatStopStringNormalizedToArray(t *testing.T) {
	obj := runStopCase(t, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stop":"END"}`)
	arr, ok := obj["stop"].([]any)
	if !ok {
		t.Fatalf("stop 应被归一成数组，实际 %T: %v", obj["stop"], obj["stop"])
	}
	if len(arr) != 1 || arr[0] != "END" {
		t.Errorf("stop=%v want [END]", arr)
	}
}

// TestChatStopArrayPassedThrough 数组保持原样（含多元素）。
func TestChatStopArrayPassedThrough(t *testing.T) {
	obj := runStopCase(t, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stop":["A","B"]}`)
	arr, ok := obj["stop"].([]any)
	if !ok || len(arr) != 2 || arr[0] != "A" || arr[1] != "B" {
		t.Fatalf("stop=%v want [A B]", obj["stop"])
	}
}

// TestChatStopEmptyStringDropped 空串按「未设置」处理：上游对 [\"\"] 也无意义。
func TestChatStopEmptyStringDropped(t *testing.T) {
	obj := runStopCase(t, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stop":""}`)
	if v, has := obj["stop"]; has {
		t.Errorf("空串 stop 应被删除，实际仍在: %v", v)
	}
}

// TestChatStopOtherTypesUntouched 畸形类型不猜：留给上游报 11101，不掩盖脏数据。
func TestChatStopOtherTypesUntouched(t *testing.T) {
	obj := runStopCase(t, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stop":123}`)
	if v, ok := obj["stop"].(float64); !ok || v != 123 {
		t.Errorf("数字形态不该被改写，实际 %T: %v", obj["stop"], obj["stop"])
	}
}
