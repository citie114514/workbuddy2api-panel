// promptlimit_test.go 11115 可修复重发的端到端行为：上游按「prompt + 输出预留」
// 判越界，而不带 max_tokens 的客户端会被默认预留顶成 400。网关应当收窄输出上限
// 重发，而不是把必然失败的 400 原样丢回客户端。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// fakeContextUpstream 复刻上游的越界口径：prompt + 输出预留 > limit → 400/11115。
// 预留取值：显式 max_tokens（不超过 clamp）原样用，否则用 defaultReserve。
type fakeContextUpstream struct {
	limit          int64
	defaultReserve int64
	promptTokens   int64

	mu   sync.Mutex
	sent []int64 // 每次调用上游看到的输出预留（-1 = 客户端没给）
}

func (f *fakeContextUpstream) client(t *testing.T) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var body struct {
				MaxTokens *int64 `json:"max_tokens"`
			}
			_ = json.Unmarshal(raw, &body)
			reserve := f.defaultReserve
			if body.MaxTokens != nil {
				reserve = *body.MaxTokens
			}
			f.mu.Lock()
			if body.MaxTokens == nil {
				f.sent = append(f.sent, -1)
			} else {
				f.sent = append(f.sent, *body.MaxTokens)
			}
			f.mu.Unlock()
			if f.promptTokens+reserve > f.limit {
				msg := `{"code":11115,"msg":"prompt is too long: ` +
					strconv.FormatInt(f.promptTokens+reserve, 10) + ` tokens > ` +
					strconv.FormatInt(f.limit, 10) + ` maximum","requestId":"req-1"}`
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(msg)),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// TestPromptTooLongRepairedWithoutClientMaxTokens 昔涟实案：客户端不发 max_tokens，
// 上游按 384000 预留把 868040 的输入顶成越界。网关应当补一个明确的小预留并成功
// 返回 200，而不是 400 prompt_too_long。
func TestPromptTooLongRepairedWithoutClientMaxTokens(t *testing.T) {
	f := &fakeContextUpstream{limit: 1048576, defaultReserve: 384000, promptTokens: 868040}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: f.client(t), PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（应自动收窄输出上限重发成功）", rec.Code, rec.Body)
	}
	if len(f.sent) < 2 {
		t.Fatalf("upstream 调用次数=%d，应先 11115 再重发", len(f.sent))
	}
	if f.sent[0] != -1 {
		t.Fatalf("第一次应原样透传（无 max_tokens），got %d", f.sent[0])
	}
	if f.sent[1] <= 0 {
		t.Fatalf("重发必须带显式 max_tokens，got %d", f.sent[1])
	}
	if f.promptTokens+f.sent[1] > f.limit {
		t.Fatalf("重发的 max_tokens=%d 仍越界（prompt %d + 预留 > %d）", f.sent[1], f.promptTokens, f.limit)
	}
}

// TestPromptTooLongTightRepairConverges 输入贴到窗口边缘时，第一跳的 32768 预留
// 仍越界，第二跳按反推的 prompt 精确收敛——两跳内必须成功。
//
// MaxRotate 用默认 3：修复重发不该吃掉轮转预算（否则轮转上限配小的部署永远修不了）。
func TestPromptTooLongTightRepairConverges(t *testing.T) {
	f := &fakeContextUpstream{limit: 1048576, defaultReserve: 384000, promptTokens: 1040000}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: f.client(t), PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（贴顶输入应两跳收敛）", rec.Code, rec.Body)
	}
	last := f.sent[len(f.sent)-1]
	if last <= 0 || f.promptTokens+last > f.limit {
		t.Fatalf("最终 max_tokens=%d 未收敛（prompt %d limit %d），序列=%v", last, f.promptTokens, f.limit, f.sent)
	}
	if len(f.sent) > 3 {
		t.Fatalf("收敛用了 %d 跳（应 ≤ 初始 + 2 次修复）", len(f.sent))
	}
}

// TestPromptTooLongUnfixablePassesThrough 输入本身超过窗口：收窄输出也救不回来，
// 必须原样透传上游 400（客户端要看到真实 token 数与上限才能自行压缩/新建会话）。
func TestPromptTooLongUnfixablePassesThrough(t *testing.T) {
	f := &fakeContextUpstream{limit: 1048576, defaultReserve: 384000, promptTokens: 1200000}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: f.client(t), PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s（输入贴顶应透传 400）", rec.Code, rec.Body)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "prompt_too_long") {
		t.Fatalf("应保留 prompt_too_long 错误码：%s", out)
	}
	// 上游原文必须透传：客户端据此才能判断「输入+预留超了多少、窗口多大」
	// 从而自行压缩上下文或换更长的模型。
	if !strings.Contains(out, "prompt is too long") || !strings.Contains(out, "1048576") {
		t.Fatalf("上游原文的越界说明/上限应透传：%s", out)
	}
}

// TestPromptTooLongRepairKeepsAccountHealthy 重发不罚号：11115 是请求的问题，
// 修好之后账号必须仍是健康可用状态（不冷却、不熔断、不计错）。
func TestPromptTooLongRepairKeepsAccountHealthy(t *testing.T) {
	f := &fakeContextUpstream{limit: 1048576, defaultReserve: 384000, promptTokens: 900000}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBreaker(1, 0, 0) // 误罚一次即熔断，任何 NoteError 都会暴露
	h := NewHandler(Config{Pool: p, Upstream: f.client(t), PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Fatalf("11115 修复不应罚号：%+v", st)
	}
}

// TestPromptTooLongKeepsExplicitSmallMaxTokens 客户端自己给了正常大小的 max_tokens
// 且不越界时，网关绝不动它的值（修复只发生在已经 400 的路径上）。
func TestPromptTooLongKeepsExplicitSmallMaxTokens(t *testing.T) {
	f := &fakeContextUpstream{limit: 1048576, defaultReserve: 384000, promptTokens: 1000}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: f.client(t), PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","stream":true,"max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(f.sent) != 1 || f.sent[0] != 4096 {
		t.Fatalf("正常请求的 max_tokens 不应被改写：%v", f.sent)
	}
}
