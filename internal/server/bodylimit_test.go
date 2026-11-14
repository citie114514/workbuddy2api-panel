// bodylimit_test.go 入站请求体安全上限：护栏不是内容策略。
//
// 两条边界必须同时成立，否则这个开关就变成了当年被移除的 max_body_mb：
//   - 真实会话量级（几 MB）必须照常通过，不能被 413；
//   - 大到足以把进程读死的上传必须被挡，且**先排空再回 413**（直接 return 会让
//     net/http 关连接，客户端只看到 connection reset，看不到那份 413）。
package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 超过上限 → 413 + payload_too_large，且不打上游。
func TestBodyLimitRejectsOversize(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxPayloadBytes: 8 << 20})

	pad := strings.Repeat("a", 9<<20) // 超过 8MiB 上限
	body := []byte(`{"model":"glm-5.2","messages":[],"pad":"` + pad + `"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413, body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "payload_too_large") {
		t.Errorf("应给出 payload_too_large 业务码: %s", rec.Body)
	}
	if calls != 0 {
		t.Errorf("超限请求不该打上游, calls=%d", calls)
	}
}

// 上限之下的真实会话量级必须照常通过。
func TestBodyLimitAllowsRealisticSession(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxPayloadBytes: 8 << 20})

	pad := strings.Repeat("a", 4<<20) // 4MiB：长会话多图 base64 的真实量级
	body := []byte(`{"model":"glm-5.2","messages":[],"pad":"` + pad + `"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (真实会话量级不该被 413)", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
}

// Anthropic 端点按自己的错误形状回 413（不是 OpenAI 形状）。
func TestBodyLimitAnthropicShape(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxPayloadBytes: 8 << 20})

	pad := strings.Repeat("a", 9<<20)
	body := []byte(`{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"` + pad + `"}]}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413, body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Errorf("Anthropic 端点应回 Anthropic 错误形状: %s", rec.Body)
	}
}

// 未配置时用内置 64MiB：几 MB 的合法 body 照常通过（与既有
// TestChatLargeBodyNoGatewayLimit 同口径，确保新护栏没有把老行为改回去）。
func TestBodyLimitDefaultIsGenerous(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up}) // 不设 MaxPayloadBytes

	pad := strings.Repeat("a", 16<<20)
	body := []byte(`{"model":"glm-5.2","messages":[],"pad":"` + pad + `"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (16MiB 在 64MiB 默认上限内)", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
}
