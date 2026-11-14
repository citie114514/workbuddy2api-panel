package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// TestMain 默认关闭聊天表格日志（chatLogEnabled=false），消除 go test 期间的 stdout 噪音。
// 断言表格行输出的测试（logging_test.go 中的 ChatLogs/LogChatRow 系列）用 withChatLog 临时开启。
func TestMain(m *testing.M) {
	chatLogEnabled = false
	os.Exit(m.Run())
}

const sseOK = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

// newFakeUpstream 返回一个 ChatStream 走 fake 的 upstream.Client。
// fake 依据 Authorization 头决定行为。
func newFakeUpstream(t *testing.T, behavior func(auth string) (status int, body string, isStream bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			status, body, isStream := behavior(authz)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func newReadFailureUpstream(prefix string) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(io.MultiReader(strings.NewReader(prefix),
					errorReader{errors.New("forced upstream read failure")})),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// bindStore 记录粘性绑定镜像调用（不联网），供 D4 端到端断言绑定收敛到最终成功号。
type bindStore struct {
	redisstore.Noop
	mu     sync.Mutex
	binds  map[string]string
	delCnt int
}

func newBindStore() *bindStore { return &bindStore{binds: map[string]string{}} }

func (b *bindStore) SetBind(key, uid string, ttl time.Duration) {
	b.mu.Lock()
	b.binds[key] = uid
	b.mu.Unlock()
}
func (b *bindStore) DelBind(key string) {
	b.mu.Lock()
	b.delCnt++
	delete(b.binds, key)
	b.mu.Unlock()
}
func (b *bindStore) LoadBinds() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]string{}
	for k, v := range b.binds {
		out[k] = v
	}
	return out
}
func (b *bindStore) lastUID(key string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.binds[key]
	return u, ok
}

// testPoolWith 构建一个所有账号 credits=1000 的池，并注入确定性随机源：
// randInt64N 恒返回 0 → pickWeighted 必选候选集中积分最高者（第一个）。
// 这让依赖"bad 先被选中"的轮转测试（如 TestChatRotatesOnHardCredit）完全确定，
// 不再受加权随机影响而 flake。
func testPoolWith(auths ...*auth.Auth) *pool.Pool {
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	for _, a := range auths {
		p.Add(a)
		p.SetCredits(a.UID, 1000, 0)
	}
	return p
}

// TestChatLargeBodyNoGatewayLimit 请求体无网关侧上限（max_body_mb 已移除）：
// 数 MB 的合法 body 完整读入并照常打上游，网关不再 413（超限类问题交由上游
// 自然响应，对齐上游 e34cfa4 规约）。
func TestChatLargeBodyNoGatewayLimit(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	pad := strings.Repeat("a", 4<<20) // 4MB 合法 JSON 字符串值
	body := []byte(`{"model":"glm-5.2","messages":[],"pad":"` + pad + `"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (large valid body must proceed)", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
}

// TestChatBadParamsFailsFastWithoutPenalty 上游 400 + Unmarshal chat params failed（11101）
// → 请求级错误：不罚账号（无冷却/无禁用/无熔断计数/无 errTotal），**且不轮转**——
// 同一 body 换号必然同样失败。端到端断言只打一次上游、直接回 400、账号完好。
func TestChatBadParamsFailsFastWithoutPenalty(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0) // 确定性源 r=0 → 先选 bad
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400: request-level error must not be retried on other accounts)", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 0 {
		t.Errorf("calls=%v want bad 1 次、good 0 次（零轮转）", calls)
	}
	// 账号完好：无冷却、无禁用、无熔断计数、无 errTotal。
	st, _ := p.Status("bad")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("ErrBadParams must not penalize account: %+v", st)
	}
}

// TestChatQueueFullStopsRotationOnFirstHit 上游 429 + code 6021
// 「queue.full.title」是**按模型**的排队/满载（不是账号问题，也不是渠道级）：
//   - 零惩罚不变：不冷却 / 不熔断 / 不喂连败（2026-10-15 实案：按限流逐号软冷却，
//     2 秒绞杀整池健康号）；
//   - **命中即终止轮转**，不回 503，按 429 + Retry-After 透传排队语义。
//
// 为什么不再换号：2026-11-02 10:43–10:48 的生产窗口里，同一批账号上
// deepseek-v4.1-flash 139 条全部 200，而 glm-5.3 / kimi-k3.1-pro 的 14 条全部
// 6020/6021、全部 attempts=3（三个不同号都撞墙）、耗时 3.4–5.1s。同一模型换号
// 一次都救不回来，轮转只是把 ~1 秒的失败拖成 3.5 秒。本用例钉住「第二个号没有
// 被调用」——这正是省下来的那两轮必然失败的上游请求。
func TestChatQueueFullStopsRotationOnFirstHit(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 429, `{"code":6021,"msg":"queue.full.title","requestId":"b1bc87ea3a771e835e388328713d47cf"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "busy1", AccessToken: "at-busy1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "busy2", AccessToken: "at-busy2", ExpiresAt: 9999999999},
	)
	p.SetCredits("busy1", 2000, 0) // 确定性源 r=0 → 先选 busy1
	p.SetCredits("busy2", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s (want 429：排队语义原样透传，客户端按 Retry-After 重试)", rec.Code, rec.Body)
	}
	if calls["Bearer at-busy1"] != 1 {
		t.Errorf("calls=%v want busy1 恰好 1 次", calls)
	}
	if calls["Bearer at-busy2"] != 0 {
		t.Errorf("calls=%v want busy2 **0** 次（模型级排队换号无效，不得继续轮转）", calls)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "5" {
		t.Errorf("Retry-After=%q want 5（上游未给 data.retry_after 时的本地兜底）", ra)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "6021") || !strings.Contains(body, "queue.full.title") || !strings.Contains(body, "b1bc87ea") {
		t.Errorf("429 body 应透传上游原文（code/msg/requestId）: %s", body)
	}
	if !strings.Contains(body, "upstream_busy") {
		t.Errorf("429 body 应带 upstream_busy code: %s", body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("channel-busy 不得报成账号耗尽: %s", body)
	}
	// 账号完好：无冷却、无禁用、无熔断计数、无 errTotal。两个号都要查——
	// 只查 busy1 会漏掉「轮转把 busy2 也惩罚了」这种偏差。
	for _, uid := range []string{"busy1", "busy2"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
			t.Errorf("%s: ErrUpstreamBusy must not penalize account: %+v", uid, st)
		}
	}
}

func TestChatQueueBusyRetryAfterAndSticky(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		body   string
		want   string
	}{
		{"响应头优先于响应体", http.Header{"Retry-After": {"2"}}, `{"code":6020,"data":{"retry_after":7}}`, "2"},
		{"响应体秒数向上取整", nil, `{"code":6021,"data":{"retry_after":2.25}}`, "3"},
		{"缺省五秒", nil, `{"code":6020,"msg":"queue.waiting"}`, "5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			up := &upstream.Client{
				HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					header := http.Header{"Content-Type": {"application/json"}}
					for key, values := range tc.header {
						header[key] = values
					}
					return &http.Response{StatusCode: http.StatusTooManyRequests, Header: header,
						Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				})},
				ChatBaseCN: "https://fake.example", BillingBaseCN: "https://fake.example",
			}
			p := testPoolWith(&auth.Auth{UID: "busy", AccessToken: "at-busy", ExpiresAt: 9999999999})
			store := newBindStore()
			sess := session.New(session.Config{TTL: time.Minute, Store: store,
				Available: func() []string { return []string{"busy"} }})
			sess.Bind("conv-queue", "busy")
			h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-queue"}}`)))
			if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != tc.want {
				t.Fatalf("code=%d Retry-After=%q want %q body=%s", rec.Code, rec.Header().Get("Retry-After"), tc.want, rec.Body)
			}
			if calls != 1 {
				t.Fatalf("calls=%d want 1（单号池轮换无可换号，末端直接透传）", calls)
			}
			if uid, ok := store.lastUID("conv-queue"); !ok || uid != "busy" {
				t.Fatalf("channel capacity must preserve sticky binding: uid=%q ok=%v", uid, ok)
			}
			st, _ := p.Status("busy")
			if st.Cooling || st.InFlight != 0 || st.ConsecutiveFails != 0 || st.BreakerFails != 0 {
				t.Fatalf("channel capacity must not penalize or retain lease: %+v", st)
			}
		})
	}
}

func TestChatModelBusyKeepsStickyAcrossTemporaryProxy(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-stream", true: "stream"}[stream], func(t *testing.T) {
			calls := map[string]int{}
			up := newFakeUpstream(t, func(authz string) (int, string, bool) {
				calls[authz]++
				if authz == "Bearer at-bad" {
					return http.StatusTooManyRequests, `{"code":14003,"msg":"model capacity busy"}`, false
				}
				return http.StatusOK, sseOK, true
			})
			p := testPoolWith(
				&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
				&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
			)
			p.SetCredits("bad", 2000, 0)
			store := newBindStore()
			sess := session.New(session.Config{TTL: time.Minute, Store: store,
				Available: func() []string { return []string{"bad", "good"} }})
			sess.Bind("conv-capacity", "bad")
			h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})
			body := fmt.Sprintf(`{"model":"glm-5.2","stream":%t,"messages":[],"metadata":{"conversation_id":"conv-capacity"}}`, stream)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
			}
			if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
				t.Fatalf("calls=%v want one 14003 attempt then one temporary proxy", calls)
			}
			if uid, ok := store.lastUID("conv-capacity"); !ok || uid != "bad" {
				t.Fatalf("temporary proxy must not steal sticky binding: uid=%q ok=%v", uid, ok)
			}
			bad, _ := p.Status("bad")
			good, _ := p.Status("good")
			if bad.Cooling || bad.Disabled || bad.InFlight != 0 || bad.ConsecutiveFails != 0 || bad.BreakerFails != 0 || bad.SuccessCount != 0 {
				t.Fatalf("14003 must be model-only capacity without account penalty: %+v", bad)
			}
			if good.SuccessCount != 1 || good.InFlight != 0 {
				t.Fatalf("temporary proxy success accounting incorrect: %+v", good)
			}
		})
	}
}

func TestChatModelBusyAllCapacityReturnsRetryable429(t *testing.T) {
	calls := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusTooManyRequests,
				Header: http.Header{"Content-Type": {"application/json"}, "Retry-After-Ms": {"1500"}},
				Body:   io.NopCloser(strings.NewReader(`{"code":14003,"msg":"model capacity busy"}`))}, nil
		})},
		ChatBaseCN: "https://fake.example", BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "busy", AccessToken: "at-busy", ExpiresAt: 9999999999})
	store := newBindStore()
	sess := session.New(session.Config{TTL: time.Minute, Store: store,
		Available: func() []string { return []string{"busy"} }})
	sess.Bind("conv-capacity-terminal", "busy")
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-capacity-terminal"}}`)))
	// 429 而非 503：14003 是「这一刻没容量」，按限流语义 + Retry-After 让 SDK 退避重试；
	// 503 会被部分客户端当成服务端故障直接中断任务。
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "2" {
		t.Fatalf("code=%d Retry-After=%q body=%s", rec.Code, rec.Header().Get("Retry-After"), rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model_busy") || strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("pure capacity must be model_busy, not model_unavailable: %s", rec.Body)
	}
	if calls != 1 {
		t.Fatalf("single busy account must not be retried repeatedly in one request: calls=%d", calls)
	}
	if uid, ok := store.lastUID("conv-capacity-terminal"); !ok || uid != "busy" {
		t.Fatalf("terminal capacity response must preserve binding: uid=%q ok=%v", uid, ok)
	}
}

func TestChatMixedModelCooldownRemainsUnavailable(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "capacity", AccessToken: "at-capacity", ExpiresAt: 9999999999},
		&auth.Auth{UID: "limited", AccessToken: "at-limited", ExpiresAt: 9999999999},
	)
	p.CooldownModelBusy("capacity", "glm-5.2", 0, "14003")
	p.CooldownSoftForModel("limited", time.Minute, time.Now().Add(time.Hour), "glm-5.2", "6004")
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		t.Fatal("fully model-cooled pool must not call upstream")
		return 0, "", false
	})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("mixed cooldown must retain conservative 400 model_unavailable: code=%d body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("non-retryable mixed model block must not carry Retry-After: %q", got)
	}
}

// TestChatBadParams400CarriesUpstreamBody 11101 的 400 响应必须包含上游原始
// 11101 信息（含 requestId，客户端据此排查），且不再出现空洞的 no_healthy_account。
func TestChatBadParams400CarriesUpstreamBody(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s (want 400)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "11101") || !strings.Contains(body, "Unmarshal chat params failed") {
		t.Errorf("400 message should carry upstream 11101 info: %s", body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("request-level failure must not be reported as account exhaustion: %s", body)
	}
}

func TestChatNonStreamAggregates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz != "Bearer at1" {
			t.Errorf("auth=%q", authz)
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content=%q", msg["content"])
	}
	st, ok := h.cfg.Pool.Status("u1")
	if !ok {
		t.Fatal("account status missing")
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.UsageCount != 1 ||
		st.TokenUsage.PromptTokens != 1 || st.TokenUsage.CompletionTokens != 1 || st.TokenUsage.TotalTokens != 2 {
		t.Errorf("token usage=%+v", st.TokenUsage)
	}
	if st.TokenUsage.LastLatencyMs < 1 || st.TokenUsage.LastTokensPerSecond == nil || *st.TokenUsage.LastTokensPerSecond <= 0 {
		t.Errorf("latest performance=%+v", st.TokenUsage)
	}
}

func TestChatStreamPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("ct=%q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q", body)
	}
	st, ok := h.cfg.Pool.Status("u1")
	if !ok {
		t.Fatal("account status missing")
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.UsageCount != 1 ||
		st.TokenUsage.PromptTokens != 1 || st.TokenUsage.CompletionTokens != 1 || st.TokenUsage.TotalTokens != 2 {
		t.Errorf("token usage=%+v", st.TokenUsage)
	}
	if st.TokenUsage.LastLatencyMs < 1 || st.TokenUsage.LastTokensPerSecond == nil || *st.TokenUsage.LastTokensPerSecond <= 0 {
		t.Errorf("latest performance=%+v", st.TokenUsage)
	}
}

func TestChatRecordsCreditForStreamAndSync(t *testing.T) {
	const sseCredit = "data: {\"id\":\"chatcmpl-credit\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":6,\"total_tokens\":10,\"credit\":1.25}}\n\n" +
		"data: [DONE]\n\n"
	for _, stream := range []bool{false, true} {
		name := "sync"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return 200, sseCredit, true
			})
			rec := usage.New("")
			h := NewHandler(Config{
				Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream: up,
				Usage:    rec,
			})
			body := `{"model":"glm-5.2","messages":[]}`
			if stream {
				body = `{"model":"glm-5.2","stream":true,"messages":[]}`
			}
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
			if recorder.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", recorder.Code, recorder.Body)
			}
			s := rec.Snapshot(24, nil)
			if s.Totals.Credits != 1.25 || s.Totals.CreditSamples != 1 || s.Totals.CreditTokens != 10 || s.Totals.CreditsPer1MTokens != 125000 {
				t.Fatalf("usage totals = %+v, want credit=1.25 tokens=10 ratio=125000", s.Totals)
			}
			if len(s.CreditByAccount) != 1 || s.CreditByAccount[0].Key != "u1" ||
				len(s.CreditByModel) != 1 || s.CreditByModel[0].Key != "glm-5.2" {
				t.Fatalf("credit dimensions = %+v / %+v", s.CreditByAccount, s.CreditByModel)
			}
		})
	}
}

func TestChatRotatesOnHardCredit(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.Reason == "" {
		t.Errorf("bad account should be cooling: %+v", st)
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.UsageCount != 0 {
		t.Errorf("bad token usage=%+v", st.TokenUsage)
	}
	good, _ := p.Status("good")
	if good.TokenUsage.RequestCount != 1 || good.TokenUsage.TotalTokens != 2 {
		t.Errorf("good token usage=%+v", good.TokenUsage)
	}
}

// TestChatSoftCoolsOnRateLimitBody 端到端回归 issue #28：上游用非 429 状态码
// （400 + 限流文案）表达模型侧限流时，该账号必须进入 CoolSoft 冷却，而不是只换号。
// 修复前 Classify 归 ErrClient → applyErrorPolicy 走 default 分支只换号不罚，
// 账号留在可用池里，下一个请求仍会被选中。
func TestChatSoftCoolsOnRateLimitBody(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 400, `{"code":1,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中（与 TestChatRotatesOnHardCredit 同一确定性手法）。
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	const soft = 45 * time.Second
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: soft})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want bad/good 各 1 次", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad 应进入 soft_rate 冷却: %+v", st)
	}
	// 冷却时长取自注入的 SoftCooldown，不依赖真实等待。
	if max := int64(soft / time.Second); st.CoolRemaining <= 0 || st.CoolRemaining > max {
		t.Errorf("cool_remaining_sec=%d want in (0,%d]", st.CoolRemaining, max)
	}

	// 冷却生效：同一账号在冷却期内不得再被选中。
	before := calls["Bearer at-bad"]
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec2.Code != 200 {
		t.Fatalf("second code=%d body=%s", rec2.Code, rec2.Body)
	}
	if calls["Bearer at-bad"] != before {
		t.Errorf("冷却中的账号不应再次被选中: calls=%v", calls)
	}
}

func TestApplyErrorPolicySoftRateExponentialBackoff(t *testing.T) {
	// 吸收上游语义：带解析重置时间的 body 对齐墙钟；无时间文案走有界退避，且
	// **冷却中的重复触发不堆加**（旧「每次都翻倍」正是全池被推到 2h 封顶的元凶）。
	// 跨冷却期的堆加语义由 pool 包 CooldownSoftRate 测试覆盖；此处锚定 handler 侧
	// 的调用形态：连续 3 次软限流错误 → 首次 600s，后续不延长。
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	for i := 1; i <= 3; i++ {
		h.applyErrorPolicy("u1", upstream.ErrSoftRate, "", "", nil)
		st, _ := p.Status("u1")
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Fatalf("call %d: 应为 soft_rate 冷却: %+v", i, st)
		}
		if st.SoftStreak != 1 {
			t.Errorf("call %d: 冷却中兜底探测不应推进 soft_streak, got %d", i, st.SoftStreak)
		}
		if st.CoolRemaining < 600-3 || st.CoolRemaining > 600 {
			t.Errorf("call %d: cool_remaining_sec=%d want ~600（不堆加）", i, st.CoolRemaining)
		}
	}
}

func TestApplyErrorPolicyNotFoundUsesFixedBase(t *testing.T) {
	// 404 分流：偶发上游 404 的冷却基数固定 60s（notFoundCooldown），不取 soft_rate
	// 的 600s 基数，也不受其配置值影响。Cooldown 重构后 404 是固定时长冷却
	//（不堆加、不喂熔断），成功后自然到期恢复。
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 20 * time.Minute}) // soft_rate 配得很大，验证 404 不受其影响

	notFoundSec := int64(notFoundCooldown / time.Second)
	for i := 1; i <= 3; i++ {
		h.applyErrorPolicy("u1", upstream.ErrNotFound, "", "", nil)
		st, _ := p.Status("u1")
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Fatalf("call %d: 应为 soft 冷却: %+v", i, st)
		}
		if st.SoftStreak != 0 {
			t.Errorf("call %d: 404 固定冷却不应推进 soft_streak, got %d", i, st.SoftStreak)
		}
		// 基数取自 notFoundCooldown（60s）而非注入的 soft_rate（20m），且重复触发不延长。
		if st.CoolRemaining < notFoundSec-3 || st.CoolRemaining > notFoundSec {
			t.Errorf("call %d: 404 cool_remaining_sec=%d want ~%d（固定基数 %ds，非 soft_rate）",
				i, st.CoolRemaining, notFoundSec, notFoundSec)
		}
	}
}

// TestApplyErrorPolicyChannelRefusedDoesNotDisable 11128 与 11140 的 msg 同为
// "request illegal"，但语义相反：
//   - 11128 通道校验失败 → 账号健康（官方客户端可正常使用），不得禁用/冷却；
//   - 11140 账号被上游限制 → 硬禁用，等用户在 WorkBuddy 申诉解除；
//   - 11140 的模型级限流变体 → 仍是软限流，不能误禁用。
//
// 回归点：只看文案的旧实现会把 11128 判成账号封禁，硬禁用健康账号（issue 1849
// 同款误伤：用户官方客户端能用，面板里却被标成 banned）。
func TestApplyErrorPolicyChannelRefusedDoesNotDisable(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		kind         upstream.ErrKind
		wantDisabled bool
		wantCooling  bool
	}{
		{"11128 channel refused", `{"code":"11-128","msg":"request illegal","displayTips":{"en":"This call failed channel verification, possibly because it was not sent from an official client."}}`, upstream.ErrChannelRefused, false, false},
		{"11140 account restricted", `{"code":11140,"msg":"request illegal"}`, upstream.ErrAccountFault, true, false},
		{"11140 rate-limiting variant", `{"code":11140,"msg":"The model provider is rate-limiting requests."}`, upstream.ErrSoftRate, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := upstream.Classify(400, tc.body); got != tc.kind {
				t.Fatalf("Classify=%v want %v", got, tc.kind)
			}
			p := pool.New("")
			p.Add(&auth.Auth{UID: "u1"})
			h := NewHandler(Config{Pool: p})
			h.applyErrorPolicy("u1", tc.kind, tc.body, "glm-5.2", nil)
			st, _ := p.Status("u1")
			if st.Disabled != tc.wantDisabled {
				t.Errorf("disabled=%v want %v (%+v)", st.Disabled, tc.wantDisabled, st)
			}
			if st.Cooling != tc.wantCooling {
				t.Errorf("cooling=%v want %v (%+v)", st.Cooling, tc.wantCooling, st)
			}
			if !tc.wantDisabled && st.ErrTotal != 0 {
				t.Errorf("不罚号路径不应累计错误计数: err_total=%d", st.ErrTotal)
			}
		})
	}
}

// TestNewHandlerSoftCooldownDefault 端到端：未注入 SoftCooldown 时基数回落到 600s（原为 60s）。
func TestNewHandlerSoftCooldownDefault(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":1,"msg":"rate limit"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up}) // 不注入 SoftCooldown

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad 应进入 soft_rate 冷却: %+v", st)
	}
	if st.CoolRemaining <= 599 || st.CoolRemaining > 600 {
		t.Errorf("default soft cooldown cool_remaining_sec=%d want 600", st.CoolRemaining)
	}
}

// TestChatStickyFollowsFinalSuccess 端到端验证 D4：粘性号失败换号成功后，会话绑定收敛到成功号。
func TestChatStickyFollowsFinalSuccess(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 先把会话预绑定到 bad（模拟历史粘性），bad 失败、good 成功 → 绑定应切到 good。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 500, `{"code":500}`, false
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	// 预绑定：sess.Bind("conv-1", "bad")，然后请求体带同 conversation_id。
	sess.Bind("conv-1", "bad")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 绑定必须收敛到最终成功的 good。
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("sticky binding should follow final success to good, got %s ok=%v (binds=%v)", uid, ok, st.binds)
	}
}

// TestChatStickySuccessKeepsBinding 粘性号直接成功 → 绑定不变（仍为该号）。
func TestChatStickySuccessKeepsBinding(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"good"} },
	})
	p := testPoolWith(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-1", "good")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("binding should stay good after success, got %s ok=%v", uid, ok)
	}
}

func TestChatUpstreamReadFailureDoesNotSucceedOrKeepSticky(t *testing.T) {
	prefix := strings.Split(sseOK, "data: [DONE]")[0]
	for _, stream := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream", false: "non-stream"}[stream], func(t *testing.T) {
			st := newBindStore()
			sess := session.New(session.Config{TTL: time.Minute, Store: st,
				Available: func() []string { return []string{"u1"} }})
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p, Upstream: newReadFailureUpstream(prefix), Session: sess})
			sess.Bind("conv-read", "u1")
			body := fmt.Sprintf(`{"model":"glm-5.2","stream":%t,"messages":[],"metadata":{"conversation_id":"conv-read"}}`, stream)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
			if stream {
				if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "upstream stream interrupted") ||
					!strings.Contains(rec.Body.String(), "data: [DONE]") {
					t.Fatalf("stream response code=%d body=%s", rec.Code, rec.Body)
				}
			} else if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "forced upstream read failure") {
				t.Fatalf("non-stream response code=%d body=%s", rec.Code, rec.Body)
			}
			account, _ := p.Status("u1")
			if account.SuccessCount != 0 {
				t.Fatalf("interrupted upstream must not count as success: %+v", account)
			}
			if uid, ok := st.lastUID("conv-read"); ok {
				t.Fatalf("failed account must release sticky binding, still bound to %s", uid)
			}
		})
	}
}

// TestChatStickyFullFallsBackToRotation 粘性号满载时本次可溢出到健康账号，
// 但绑定保留在原号；即使整池随后满载，也不能把临时压力变成永久搬号。
func TestChatStickyFullFallsBackToRotation(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// bad 占满唯一在途名额：本次回落轮换，但不解绑。
	p.SetMaxInFlight(1)
	p.Acquire("bad")

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	sess.Bind("conv-1", "bad")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "bad" {
		t.Fatalf("temporary overflow must keep original binding, got %s ok=%v (binds=%v)", uid, ok, st.binds)
	}
	// 整池满载时选不到任何号，绑定仍须保留。
	p.Acquire("good")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("all-full code=%d body=%s", rec.Code, rec.Body)
	}
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "bad" {
		t.Fatalf("all-full must not clear binding, got %s ok=%v", uid, ok)
	}
	p.Release("bad")
	p.Release("good")
}

func TestChatHardCreditCooldownUntilNextDay4AM(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0) // bad 积分高，确定性源 → 先被选中
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, ok := p.Status("bad")
	if !ok || !st.Cooling {
		t.Fatalf("bad should be cooling: %+v ok=%v", st, ok)
	}
	if st.Reason != "余额不足" {
		t.Errorf("reason=%q", st.Reason)
	}
	// 硬信贷冷却必须是次日 04:00，而不是固定 12h/配置时长。
	if st.Until.Hour() != 4 {
		t.Errorf("until hour=%d want 4 (next-day 04:00)", st.Until.Hour())
	}
	// 距次日 04:00 最长 28h（凌晨 00:00~04:00 间运行时 now→次日 04:00 跨度 > 24h，属正常）。
	if d := time.Until(st.Until); d <= 0 || d > 28*time.Hour {
		t.Errorf("until %v not within (0,28h]: %v", st.Until, d)
	}
	// 立即换号成功：good 被选中。
	stGood, _ := p.Status("good")
	if stGood.Cooling || stGood.Disabled {
		t.Errorf("good should stay healthy: %+v", stGood)
	}
}

func TestChat429Code14018UsesHardCreditCooldown(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":14018,"msg":"Credits exhausted"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 2000)
	p.SetCredits("good", 1000, 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	st, ok := p.Status("bad")
	if !ok || !st.Cooling {
		t.Fatalf("14018 account should be cooling: %+v ok=%v", st, ok)
	}
	if st.Reason != "余额不足" || st.Until.Hour() != 4 {
		t.Fatalf("14018 should use hard-credit cooldown, got reason=%q until=%v", st.Reason, st.Until)
	}
	if got := p.Pick(); got == nil || got.UID != "good" {
		t.Fatalf("hard-cooled 14018 account must not be fallback-picked, got %+v", got)
	}
}

// TestChat6004ModelResetCoolsToParsedTime 端到端回归 issue #31：上游 429 + code 6004
// +「将在 … 重置」→ 冷却 until 精确等于解析时间（而非 600s 固定基数/指数退避），
// 且记录触发模型 → 同模型请求仍被冷却、切模型请求按豁免可选。
func TestChat6004ModelResetCoolsToParsedTime(t *testing.T) {
	// 用未来 5 分钟的重置时间（wall-clock）构造上游响应。
	reset := time.Now().Add(5 * time.Minute)
	ts := reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	// 隔离对 breaker 的干扰：熔断阈值默认 3，一次失败不触发。
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after rotate to good)", rec.Code, rec.Body)
	}
	// bad 已进入 6004 模型级独立冷却：账号级不 cooling，台账单行 until ≈ reset。
	st, _ := p.Status("bad")
	if st.Cooling {
		t.Fatalf("6004-with-reset should NOT set account-level cooling: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("want single model ledger row glm-5.3: %+v", st.RateLimitedModels)
	}
	if d := st.RateLimitedModels[0].Until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("model until=%v want ~reset=%v (diff %v)", st.RateLimitedModels[0].Until, reset, d)
	}
	// 记录触发模型（bad 池内 private 字段需经 Status 不可见，改用行为断言）：
	// 同模型 glm-5.3 的请求不应选中 bad（仍冷却）；
	// 不同模型 hy3-x 的请求应豁免冷却选中 bad（最高分）。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	same := p.PickExcludingForModel(nil, "glm-5.3")
	if same == nil || same.UID != "good" {
		t.Fatalf("same-model pick should skip bad (still cooling), got %+v", same)
	}
	diff := p.PickExcludingForModel(nil, "hy3-x")
	if diff == nil || diff.UID != "bad" {
		t.Fatalf("different-model pick should bypass bad soft cooling, got %+v", diff)
	}
}

// TestChat6004WithoutResetFallsBackToBackoff 6004 无时间文案 → 退回 600s 基数软冷却
// （现状不变）。
func TestChat6004WithoutResetUsesModelBackoff(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"model usage limit exceeded"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if st.Cooling {
		t.Fatalf("6004 without reset must not cool the whole account: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" || st.RateLimitedModels[0].Kind != "rate_limit" {
		t.Fatalf("rate-limited models=%+v, want model row for glm-5.3", st.RateLimitedModels)
	}
	until := st.RateLimitedModels[0].Until
	if rem := time.Until(until); rem <= 0 || rem > time.Minute {
		t.Errorf("model cooldown remaining=%v want ~60s", rem)
	}
	if got := p.PickExcludingForModel(nil, "hy3-x"); got == nil || got.UID != "bad" {
		t.Fatalf("other model should still select bad, got %+v", got)
	}
}

func TestChatAllUnavailableReturns503(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestChatSessionDeadDisables(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 401, `{"code":12153,"msg":"Offline user session not found"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("account should be disabled: %+v", st)
	}
}

func TestChatTransportErrorDoesNotPenalize(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	// 传输错误不喂熔断计数：一次 transport error 不应累计 errTotal 也不应熔断。
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("transport error should not penalize account: %+v", st)
	}
}

func TestChatHTTP5xxPenalizes(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 熔断阈值 1：一次 5xx 即触发熔断（连续失败语义并入熔断器）。
	p.SetBreaker(1, time.Hour, time.Hour)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `{"code":500}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatalf("http 5xx should trip breaker (cooling) with threshold=1: %+v", st)
	}
}

func TestChatHTTP4xxClientDoesNotPenalize(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":400,"msg":"bad request"}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("generic 4xx should not penalize account: %+v", st)
	}
}

// TestChatModelForbiddenByAccountTypeReturns400 复现 global:gpt-5.6-sol 在免费账号上
// 的 11158：网关应仅轮转 global 域、写 (账号,模型) 避让，最终返回明确的 400
// model_unavailable，而不是被 CN 账号掩盖成 503 no_healthy_account。
func TestChatModelForbiddenByAccountTypeReturns400(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, `{"code":11158,"msg":"model [gpt-5.6-sol] is not allowed for user type [personal_free_user]","requestId":"req-11158"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "at-cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at-global", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:gpt-5.6-sol","messages":[]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("calls=%d want 1（只能尝试 global 账号，不能跨域轮转）", calls)
	}
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response JSON: %v body=%s", err, rec.Body)
	}
	if resp.Error.Code != "model_unavailable" {
		t.Errorf("error.code=%q want model_unavailable", resp.Error.Code)
	}
	if !strings.Contains(resp.Error.Message, "11158") {
		t.Errorf("响应应透传上游 11158 原文: %q", resp.Error.Message)
	}
	if st := p.ModelBlockedForRealm("gpt-5.6-sol", "global"); !st.Blocked || st.Count != 1 {
		t.Fatalf("global 账号应写入模型级避让: %+v", st)
	}
}

func TestModelsEndpoint(t *testing.T) {
	// 动态目录缓存是包级状态；随机顺序执行时不能继承其他用例的失败退避。
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("object=%v", resp["object"])
	}
	data := resp["data"].([]any)
	if len(data) < 5 {
		t.Errorf("models count=%d", len(data))
	}
	found := false
	for _, m := range data {
		if m.(map[string]any)["id"] == "cn:glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Error("cn:glm-5.2 missing")
	}
}

func TestModelsDynamic(t *testing.T) {
	// 清缓存
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	// 假上游返回动态模型（含 agents + maxInputTokens/maxOutputTokens + reasoning 档位）
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"dyn-model-a","maxInputTokens":65536,"maxOutputTokens":8192,"reasoning":{"effort":"medium","supportedEfforts":["low","medium","high"]}},{"id":"dyn-model-b","maxInputTokens":131072,"maxOutputTokens":16384},{"id":"glm-9.9","maxInputTokens":262144,"maxOutputTokens":32768}],"agents":[{"name":"cli","models":["dyn-model-a","dyn-model-b","glm-9.9"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) != 3 {
		t.Fatalf("want 3 dynamic models, got %d: %v", len(data), data)
	}
	ids := map[string]bool{}
	for _, m := range data {
		ids[m.(map[string]any)["id"].(string)] = true
	}
	if !ids["cn:dyn-model-a"] || !ids["cn:glm-9.9"] {
		t.Errorf("dynamic ids missing: %v", ids)
	}

	// 断言字段映射：maxInputTokens → context_length，maxOutputTokens → max_output_tokens
	for _, m := range data {
		mm := m.(map[string]any)
		switch mm["id"] {
		case "dyn-model-a":
			if mm["context_length"].(float64) != 65536 {
				t.Errorf("dyn-model-a context_length=%v want 65536", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 8192 {
				t.Errorf("dyn-model-a max_output_tokens=%v want 8192", mm["max_output_tokens"])
			}
			// reasoning 档位透出：supported_efforts + default_effort
			efforts, _ := mm["supported_efforts"].([]any)
			if len(efforts) != 3 || efforts[0] != "low" {
				t.Errorf("dyn-model-a supported_efforts=%v", mm["supported_efforts"])
			}
			if mm["default_effort"] != "medium" {
				t.Errorf("dyn-model-a default_effort=%v want medium", mm["default_effort"])
			}
		case "dyn-model-b":
			// 上游未返回 reasoning → 两个档位字段都省略（客户端按自身默认）
			if _, has := mm["supported_efforts"]; has {
				t.Errorf("dyn-model-b supported_efforts should be omitted, got %v", mm["supported_efforts"])
			}
			if _, has := mm["default_effort"]; has {
				t.Errorf("dyn-model-b default_effort should be omitted")
			}
		case "glm-9.9":
			if mm["context_length"].(float64) != 262144 {
				t.Errorf("glm-9.9 context_length=%v want 262144", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 32768 {
				t.Errorf("glm-9.9 max_output_tokens=%v want 32768", mm["max_output_tokens"])
			}
		}
	}

	// 第二次调用走缓存（把上游关掉也成功）
	dynamicModelsCache.RLock()
	cached := len(dynamicModelsCache.ids)
	dynamicModelsCache.RUnlock()
	if cached != 3 {
		t.Errorf("cache not populated: %d", cached)
	}
}

func TestModelsDynamicFallsBackToStatic(t *testing.T) {
	// 纯动态化（产品决策）：上游失败 → 空列表（200），不再回落静态表——
	// 拉不出目录即意味着上游不可用，假名单只会让客户端选到 11102 的模型。
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) != 0 {
		t.Errorf("pure-dynamic failure should yield empty list, got %d", len(data))
	}
}

func TestModelsFetchFailurePenalizesAccount(t *testing.T) {
	// 吸收上游 9832283：models 拉取失败只进负缓存，不 NoteError——
	// NoteError 喂的是 chat 熔断器，models 端点 5xx 跨界惩罚 chat 通道健康号。
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBreaker(1, time.Hour, time.Hour) // 熔断阈值 1：若仍罚号，一次 fetch 失败即熔断
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Fatalf("models fetch failure must not trip chat breaker: %+v", st)
	}
}

func TestModelsNegativeCacheOnFetchFailure(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	var calls atomic.Int32 // FetchModels 企业/v3 两路并发探测回调，计数须原子
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 500, `boom`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 连续 3 次请求，上游持续 500 → 只应触发 1 次 fetch（负缓存生效），
	// 纯动态下空列表仍返回 200。
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
		if rec.Code != 200 {
			t.Fatalf("req %d: code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	// 一轮探测 = 2 次上游调用（企业端点 + /v3/config 并发，两路全失败才进负缓存）。
	if got := calls.Load(); got != 2 {
		t.Errorf("want 2 probes (console + v3), got %d", got)
	}

	// 冷却期结束（把失败时间戳拨回 10 分钟前）→ 应重新 fetch。
	dynamicModelsCache.Lock()
	dynamicModelsCache.lastFail = time.Now().Add(-10 * time.Minute)
	dynamicModelsCache.Unlock()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("after cooldown: code=%d", rec.Code)
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("want 4 probes after cooldown (2 rounds x 2), got %d", got)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	// 无 key
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("no key: code=%d", rec.Code)
	}
	// 错 key
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
	// 对 key（请求会继续打到上游，但此处上游 client 会失败 —— 只要不是 401 就行）
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("right key: code=%d", rec.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 42, 0)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"uid":"u1"`) || !strings.Contains(body, `"credits":42`) {
		t.Errorf("body=%s", body)
	}
	if strings.Contains(body, "AccessToken") || strings.Contains(body, `"at"`) {
		t.Error("token leaked in status output")
	}
	// Phase 3 汇总字段。
	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if statusBody["total"] != float64(1) || statusBody["healthy"] != float64(1) ||
		statusBody["cooling"] != float64(0) || statusBody["disabled"] != float64(0) ||
		statusBody["in_flight_full"] != float64(0) {
		t.Errorf("summary=%v want total=1 healthy=1 cooling=0 disabled=0 in_flight_full=0", statusBody)
	}
	// Phase v3：池级 sticky_sessions + redis_mode。
	if statusBody["sticky_sessions"] != float64(0) {
		t.Errorf("sticky_sessions=%v want 0", statusBody["sticky_sessions"])
	}
	if statusBody["redis_mode"] != "noop" {
		t.Errorf("redis_mode=%v want noop", statusBody["redis_mode"])
	}
}

// TestStatusInFlightFull /status 透出满载计数：healthy 且占满在途的账号数。
func TestStatusInFlightFull(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "full", AccessToken: "at", ExpiresAt: 9999999999},
		&auth.Auth{UID: "free", AccessToken: "at", ExpiresAt: 9999999999},
	)
	p.SetMaxInFlight(1)
	p.Acquire("full")
	defer p.Release("full")

	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if statusBody["in_flight_full"] != float64(1) {
		t.Errorf("in_flight_full=%v want 1", statusBody["in_flight_full"])
	}
	if statusBody["healthy"] != float64(2) {
		t.Errorf("healthy=%v want 2 (full is still healthy by state-machine semantics)", statusBody["healthy"])
	}
}

func TestStatusPortraitFields(t *testing.T) {
	// Phase 3：/status 单账号需返回健康画像字段。
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.NoteSuccess("u1")
	p.NoteSuccess("u1")
	p.NoteError("u1") // 记录 last_err + err_total（累计，不冷却）
	p.Cooldown("u1", pool.CoolSoft, time.Hour, "429 rate limit")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Accounts []pool.Status `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts=%d", len(body.Accounts))
	}
	st := body.Accounts[0]
	if !st.Cooling || st.CoolKind != "soft_rate" || st.CoolRemaining <= 0 {
		t.Errorf("cooling portrait=%+v", st)
	}
	if st.SuccessCount != 2 {
		t.Errorf("success_count=%d want 2", st.SuccessCount)
	}
	if st.ErrTotal != 1 {
		t.Errorf("err_total=%d want 1", st.ErrTotal)
	}
	if st.LastSuccessTime.IsZero() {
		t.Error("last_success should be set")
	}
	if st.LastErrTime.IsZero() {
		t.Error("last_err should be set")
	}
}

// TestHealthzEmptyPool 空池（healthy=0）→ 503，表示暂不可服务。
func TestHealthzEmptyPool(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503 (healthy=0)", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(0) || resp["total"] != float64(0) {
		t.Errorf("healthz json=%v", resp)
	}
}

// TestHealthz503WhenNoHealthy 所有账号禁用/冷却 → 503。
func TestHealthz503WhenNoHealthy(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Disable("u1", "session dead")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("ct=%q want json", ct)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(0) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=0 total=1", resp)
	}
}

// TestHealthz503WhenAllInFlightFull 全部账号 healthy 但都占满在途 → 503（与 chat 同口径），
// 且 healthy 语义未变（仍为 1）：满载不是状态机健康维度的变化，是探活口径单独叠加。
func TestHealthz503WhenAllInFlightFull(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	p.Acquire("u1") // 占满唯一在途名额
	defer p.Release("u1")

	if p.ServableNow() {
		t.Fatal("servable should be false when the only healthy account is in-flight full")
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 (healthy but all in-flight full)", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	// healthy 语义未变：账号仍是 healthy（只占满在途，非冷却/禁用）。
	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1 (healthy semantics unchanged)", resp)
	}
}

// TestHealthz200WhenHealthy 有健康账号 → 200。
func TestHealthz200WithHealthy(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1", resp)
	}
}

// TestHealthzServiceIdentity /healthz 无论 200 还是 503 都必须带网关身份标识
// （响应体 service 字段 + X-Service 头）：宿主探测打到同端口的旧服务/其他服务时，
// 对方即使返回 2xx 也不带本标识，宿主据此判"假成功"。
func TestHealthzServiceIdentity(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(*pool.Pool)
		wantCode int
	}{
		{"healthy", func(*pool.Pool) {}, http.StatusOK},
		{"unhealthy", func(p *pool.Pool) { p.Disable("u1", "session dead") }, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			tc.setup(p)
			h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("code=%d want %d", rec.Code, tc.wantCode)
			}
			if got := rec.Header().Get("X-Service"); got != ServiceName {
				t.Errorf("X-Service=%q want %q", got, ServiceName)
			}
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
			}
			if resp["service"] != ServiceName {
				t.Errorf("service=%v want %q", resp["service"], ServiceName)
			}
		})
	}
}

// TestHealthzServiceIdentityWithoutAuth /healthz 保持无鉴权（负载均衡友好）：
// 配了 api_key 也不要求 Bearer，身份字段照常返回。
func TestHealthzServiceIdentityWithoutAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz must stay unauthenticated: code=%d", rec.Code)
	}
	if got := rec.Header().Get("X-Service"); got != ServiceName {
		t.Errorf("X-Service=%q want %q", got, ServiceName)
	}
}

func TestStatusRequiresAuth(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "secret"})

	// 无 token → 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 401 {
		t.Errorf("no token: code=%d", rec.Code)
	}

	// 带 token → 200
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("with token: code=%d", rec.Code)
	}

	// /healthz 无鉴权仍 200
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("healthz: code=%d", rec.Code)
	}
}

// TestContentBlockedTriggersDegradedRetry passthrough 模式下首请求 400（11128 文案）
// → 降级重试（Degraded）→ 200，客户端无感。验证第二次出站 body 为 Degraded。
func TestContentBlockedTriggersDegradedRetry(t *testing.T) {
	// 记录每次出站请求体，断言第二次为 Degraded 文本。
	var bodies [][]byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw := readUpstreamBody(r)
			bodies = append(bodies, raw)
			// 首次（body 含原始 system）返回 400 内容拦截；后续返回 200。
			if len(bodies) == 1 {
				return &http.Response{
					StatusCode: 400,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":"11-128","msg":"blocked by security policy"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after degraded retry)", rec.Code, rec.Body)
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 upstream calls (first 400 + retry), got %d", len(bodies))
	}
	// 第二次出站 body 的 messages 头部 system 内容应为 Degraded 文本。
	if !strings.Contains(string(bodies[1]), prompt.Degraded) {
		t.Errorf("second body should contain Degraded prompt: %s", bodies[1])
	}
	if strings.Contains(string(bodies[1]), "原始指纹") {
		t.Errorf("second body should not contain original system: %s", bodies[1])
	}
}

// TestContentBlockedStickyDegraded 降级后新请求直达 Degraded（不再先撞 400）。
func TestContentBlockedStickyDegraded(t *testing.T) {
	var firstCall bool
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if !firstCall {
			firstCall = true
			return 400, `{"code":"11-128","msg":"blocked by security policy"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	// 首请求触发降级 → 200。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"x"},{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("first req code=%d", rec.Code)
	}
	// 降级粘性：新请求 Active()=true，body 已被 Rewrite(Degraded)，上游首字节即 200。
	// 但 fake 上游只对 firstCall 返回 400，之后都 200，无法区分"直达"与"重试"。
	// 用 degrade.Active() 直接断言粘性生效。
	if !h.degrade.Active() {
		t.Fatal("degrade should be active after trigger")
	}
}

// TestContentBlockedCustomModeDoesNotDegrade custom 模式不触发降级重试
// （custom 已用自有提示词替换，不应再有 system 来源误报）；仍拦则直接回
// 400 content_blocked 防火墙文案（不轮转、不暴露账号/上游错误码）。
func TestContentBlockedCustomModeDoesNotDegrade(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":"11-128","msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: "SYS"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"system","content":"old"},{"role":"user","content":"hi"}]}`)))
	// custom 模式下仍拦 → 400 content_blocked（内容终态，换号无意义），不降级重试。
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 content_blocked (custom does not degrade)", rec.Code)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if env.Error.Code != "content_blocked" {
		t.Errorf("error.code=%q want content_blocked", env.Error.Code)
	}
	// 错误透传（error-passthrough，吸收上游 5755fe3）：message 装上游 body 原文
	// （code/msg 原样），客户端必须看到真实错误才能排查；gateway_hint 并列补充。
	if !strings.Contains(rec.Body.String(), "11-128") {
		t.Errorf("message should carry upstream original body (passthrough): %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gateway_hint") {
		t.Errorf("content_blocked should carry gateway_hint: %s", rec.Body.String())
	}
	if h.degrade.Active() {
		t.Error("degrade should NOT be active in custom mode")
	}
}

// TestContentBlockedDoesNotPenalizeAccount ErrContentBlocked 不罚账号（无冷却/熔断/NoteError）。
func TestContentBlockedDoesNotPenalizeAccount(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":"11-128","msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 熔断阈值 1：若误罚 NoteError 一次即熔断；content_blocked 不应喂熔断。
	p.SetBreaker(1, time.Hour, time.Hour)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Fatalf("ErrContentBlocked should not penalize account: %+v", st)
	}
}

// TestCustomModeFingerprintSanitizePreserved custom 端到端：user 消息含 Claude Code
// 指纹句（PR39 fixture 串）→ Rewrite 注入自有 system → 经 sanitize → 出站 body 中
// 该指纹被改写、system 为自有提示词。证明两层（提示词替换 + 清洗）叠加工作。
//
// 两层各自职责（互不替代）：
//   - prompt.Rewrite 替换 system/developer 消息（消灭 system 来源指纹）；
//   - sanitizeMessages 改写 user/assistant 消息中残留的指纹串（兜底用户上下文）。
//
// TestCustomModeFingerprintSanitizePerRole custom 端到端：system 被替换成自有提示词；
// sanitize 只洗**身份域**消息（system / developer / assistant）。user 里的同一句属
// 用户数据，必须逐字保留（P4：作用域收窄到身份域）；assistant 必须被改写（证明清洗
// 层仍在工作，不是「全删了」）。
func TestCustomModeFingerprintSanitizePerRole(t *testing.T) {
	const ccIdent = "You are Claude Code, Anthropic's official CLI for Claude."
	const ccBranch = "Main branch (you will usually use this for PRs)"
	const customSys = "网关自有提示词"

	var sentBody []byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			sentBody = readUpstreamBody(r)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	up.SanitizeFingerprints.Store(true) // 开启清洗层（与生产一致）
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: customSys})

	userContent := ccIdent + " " + ccBranch
	body := `{"model":"glm-5.2","stream":true,"messages":[` +
		`{"role":"system","content":"` + ccIdent + `"},` +
		`{"role":"user","content":"` + userContent + `"},` +
		`{"role":"assistant","content":"` + ccIdent + `"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	out := string(sentBody)
	if !strings.Contains(out, customSys) {
		t.Errorf("out body should contain custom system prompt: %s", out)
	}
	var obj map[string]any
	if err := json.Unmarshal(sentBody, &obj); err != nil {
		t.Fatalf("out body not json: %v %s", err, out)
	}
	msgs, _ := obj["messages"].([]any)
	var gotSystem, gotUser, gotAssistant string
	var systemCount int
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		c, _ := mm["content"].(string)
		switch mm["role"] {
		case "system":
			systemCount++
			gotSystem = c
		case "user":
			gotUser = c
		case "assistant":
			gotAssistant = c
		}
	}
	if systemCount != 1 || gotSystem != customSys {
		t.Errorf("want exactly 1 system = %q, got count=%d content=%q", customSys, systemCount, gotSystem)
	}
	if gotUser != userContent {
		t.Errorf("user data must stay verbatim (identity scope narrowed): got %q want %q", gotUser, userContent)
	}
	if gotAssistant == ccIdent {
		t.Errorf("assistant identity sentence should be sanitized, got unchanged: %q", gotAssistant)
	}
}

// --- 模型原生工具调用标记修复（端到端，见 upstream/dsml.go）-----------------

// markupFixture 拼一段「正文 + 原生工具调用标记」，模拟上游在没有 tools 的请求里
// 把工具调用吐成文本的形态。分隔符按码点构造，避免测试源码里出现裸全角字面量。
func markupFixture(prose, cmd string) string {
	return markupFixtureNamed(prose, "exec_command", cmd)
}

// markupFixtureNamed 同 markupFixture，但可指定工具名（用于弱判定的形态把关测试）。
func markupFixtureNamed(prose, name, cmd string) string {
	const pipe = "\uFF5C\uFF5C"
	tag := func(rest string) string { return "<" + pipe + "DSML" + pipe + " " + rest + ">" }
	ctag := func(rest string) string { return "</" + pipe + "DSML" + pipe + " " + rest + ">" }
	return prose + tag("calls") + "\n" +
		tag(`invoke name="`+name+`"`) + "\n" +
		tag(`parameter name="cmd" string="true"`) + cmd + ctag("parameter") + "\n" +
		ctag("invoke") + "\n" + ctag("calls")
}

// markupUpstreamSSE 用一段正文拼上游 SSE（role 首帧 + 正文帧 + stop 收尾帧 + [DONE]）。
func markupUpstreamSSE(content string) string {
	chunk := func(delta map[string]any, finish any) string {
		b, _ := json.Marshal(map[string]any{
			"id": "resp-1", "object": "chat.completion.chunk", "created": 1, "model": "glm-5.2",
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})
		return "data: " + string(b) + "\n\n"
	}
	return chunk(map[string]any{"role": "assistant"}, nil) +
		chunk(map[string]any{"content": content}, nil) +
		chunk(map[string]any{}, "stop") +
		"data: [DONE]\n\n"
}

// declaredToolsBody 是带 tools 声明的请求体（标记修复的启用前提）。
const declaredToolsBody = `"tools":[{"type":"function","function":{"name":"exec_command",` +
	`"parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}}]`

// TestChatNonStreamRepairsMarkupToolCalls 非流式：上游把工具调用吐成正文标记时，
// 网关还原成 message.tool_calls，正文不再残留标记，finish_reason 收敛为 tool_calls。
func TestChatNonStreamRepairsMarkupToolCalls(t *testing.T) {
	content := markupFixture("先读一遍控制器。\n\n", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	reqBody := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],` + declaredToolsBody + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "\uFF5C") {
		t.Fatalf("响应里仍残留标记: %s", rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "先读一遍控制器。\n\n" {
		t.Errorf("content=%q", msg["content"])
	}
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls=%#v want 1 个", msg["tool_calls"])
	}
	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn == nil || fn["name"] != "exec_command" {
		t.Fatalf("function=%#v", tcs[0])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatalf("arguments 不是合法 JSON: %v", fn["arguments"])
	}
	if args["cmd"] != "ls -la" {
		t.Errorf("cmd=%v", args["cmd"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v want tool_calls", choice["finish_reason"])
	}
}

// TestChatStreamRepairsMarkupToolCalls 流式：同样还原成 delta.tool_calls，且收尾帧
// finish_reason 为 tool_calls、排在调用帧之后。
func TestChatStreamRepairsMarkupToolCalls(t *testing.T) {
	content := markupFixture("先读一遍控制器。\n\n", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	reqBody := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}],` + declaredToolsBody + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, "\uFF5C") {
		t.Fatalf("流式响应里仍残留标记: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("缺 [DONE] 收尾: %s", body)
	}
	var text strings.Builder
	var calls int
	frameNo, lastCallFrame, finishFrame := 0, -1, -1
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		frameNo++
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			t.Fatalf("帧不是合法 JSON: %q (%v)", payload, err)
		}
		chs, _ := obj["choices"].([]any)
		if len(chs) == 0 {
			continue
		}
		ch, _ := chs[0].(map[string]any)
		d, _ := ch["delta"].(map[string]any)
		if v, ok := d["content"].(string); ok {
			text.WriteString(v)
		}
		if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
			calls += len(tcs)
			lastCallFrame = frameNo
			fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
			if fn == nil || fn["name"] != "exec_command" {
				t.Fatalf("调用帧 function=%#v", tcs[0])
			}
		}
		if v, ok := ch["finish_reason"].(string); ok && v != "" {
			finishFrame = frameNo
			if v != "tool_calls" {
				t.Errorf("finish_reason=%q want tool_calls", v)
			}
		}
	}
	if text.String() != "先读一遍控制器。\n\n" {
		t.Errorf("正文错误: %q", text.String())
	}
	if calls != 1 {
		t.Errorf("tool_call 帧数=%d want 1", calls)
	}
	if lastCallFrame < 0 || finishFrame < 0 {
		t.Fatalf("缺调用帧或收尾帧: lastCall=%d finish=%d", lastCallFrame, finishFrame)
	}
	if finishFrame < lastCallFrame {
		t.Errorf("收尾帧（第 %d 帧）必须排在调用帧（第 %d 帧）之后", finishFrame, lastCallFrame)
	}
}

// --- 弱判定（名单为空）端到端：这是实测最常见的泄漏形态 ---------------------

// TestChatRepairsMarkupWithoutDeclaredTools 请求体里**没有** tools 声明时（tools 在
// 中转环节丢失是实测常态），弱判定必须照样把正文标记还原成 tool_calls——否则修复层
// 恰好对主场景不设防。夹具用生产环境真实泄漏的正文。
func TestChatRepairsMarkupWithoutDeclaredTools(t *testing.T) {
	content := markupFixture("", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "\uFF5C") {
		t.Fatalf("响应里仍残留标记: %s", rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls=%#v want 1 个", msg["tool_calls"])
	}
	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn == nil || fn["name"] != "exec_command" {
		t.Fatalf("function=%#v", tcs[0])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v want tool_calls", choice["finish_reason"])
	}
}

// TestChatMarkupRepairRejectsNonIdentifierToolName 弱判定的把关：名字不像工具名
// （模型在正文里「讨论」这段语法时的占位符）→ 原文透传，绝不改写成工具调用。
func TestChatMarkupRepairRejectsNonIdentifierToolName(t *testing.T) {
	content := markupFixtureNamed("", "<tool_name>", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, ok := msg["tool_calls"]; ok {
		t.Fatalf("占位符工具名不得转换: %#v", msg["tool_calls"])
	}
	if got, _ := msg["content"].(string); !strings.Contains(got, "\uFF5C") {
		t.Fatalf("应原文透传，实际 content=%q", got)
	}
}

// TestChatMarkupRepairStrictModeRejectsUndeclaredTool 名单非空时走严格判定：
// 块内工具名不在名单里 → 不转换（即使名字形态合法）。
func TestChatMarkupRepairStrictModeRejectsUndeclaredTool(t *testing.T) {
	content := markupFixtureNamed("", "rm_rf_everything", "ls -la")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, markupUpstreamSSE(content), true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	reqBody := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],` + declaredToolsBody + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if _, ok := msg["tool_calls"]; ok {
		t.Fatalf("未声明的工具名不得转换: %#v", msg["tool_calls"])
	}
}

// TestChatHiddenModelRejected models.hidden 黑名单：点名调用直接 400 拒绝，
// 不打上游、不占选号；未隐藏的模型照常放行。
func TestChatHiddenModelRejected(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, ModelsHidden: []string{"glm-5.2", "global:secret-model"}})

	post := func(model string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		body := []byte(`{"model":"` + model + `","messages":[]}`)
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
		return rec
	}

	// 裸名隐藏 → 裸名 / cn: / global: 三种写法都拒绝
	for _, model := range []string{"glm-5.2", "cn:glm-5.2", "global:glm-5.2"} {
		rec := post(model)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("model %q: code=%d, want 400", model, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "model_unavailable") {
			t.Errorf("model %q: body=%s, want model_unavailable", model, rec.Body.String())
		}
	}
	// 前缀精确隐藏：只藏 global 域
	if rec := post("global:secret-model"); rec.Code != http.StatusBadRequest {
		t.Errorf("global:secret-model: code=%d, want 400", rec.Code)
	}
	if calls != 0 {
		t.Errorf("hidden models must not reach upstream: calls=%d", calls)
	}

	// cn:secret-model 不在黑名单 → 放行并打到上游
	rec := post("cn:secret-model")
	if rec.Code != http.StatusOK {
		t.Fatalf("cn:secret-model: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("allowed model: upstream calls=%d, want 1", calls)
	}
}

// TestModelsHiddenFiltered models.hidden 黑名单：/v1/models 列表里过滤掉隐藏模型。
// 用包级 dynamicModelsCache 预置目录，避免联网拉取。
func TestModelsHiddenFiltered(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "glm-5.2"}, {ID: "deepseek-v3"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	defer func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	}()

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"data":[]}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, ModelsHidden: []string{"deepseek-v3"}})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range resp.Data {
		ids[m.ID] = true
	}
	if !ids["cn:glm-5.2"] {
		t.Errorf("visible model missing from list: ids=%v", ids)
	}
	if ids["cn:deepseek-v3"] {
		t.Errorf("hidden model leaked into /v1/models: ids=%v", ids)
	}
}

// TestNonStreamEmptyReplyRescue 非流式：思考吃光 max_tokens（finish_reason=length、
// 正文零字节、无 tool_calls）时，同额度关思考重发一次，客户端拿到正常正文。
//
// 这是 Agent 循环最怕的形态：回合"正常结束"却一个字都没有，比报错更难排查。
func TestNonStreamEmptyReplyRescue(t *testing.T) {
	const emptyTruncated = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	const rescued = "data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"来自救援流\"}}]}\n\n" +
		"data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3,\"total_tokens\":8}}\n\n" +
		"data: [DONE]\n\n"

	var bodies []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw := readUpstreamBody(r)
			bodies = append(bodies, string(raw))
			body := emptyTruncated
			if len(bodies) > 1 {
				body = rescued
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{
		"model":"deepseek-v4.1-flash",
		"max_tokens":200,
		"messages":[{"role":"user","content":"hi"}]
	}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(bodies) != 2 {
		t.Fatalf("上游调用次数=%d want 2（首跳 + 救援跳）", len(bodies))
	}
	if !strings.Contains(rec.Body.String(), "来自救援流") {
		t.Fatalf("客户端应拿到救援流的正文: %s", rec.Body)
	}
	// 救援请求必须关思考，且不放大客户端给的 max_tokens。
	var second map[string]any
	if err := json.Unmarshal([]byte(bodies[1]), &second); err != nil {
		t.Fatalf("救援请求体非法: %v", err)
	}
	th, _ := second["thinking"].(map[string]any)
	if th["type"] != "disabled" {
		t.Fatalf("救援请求 thinking=%v want disabled", second["thinking"])
	}
	if mt, _ := second["max_tokens"].(float64); mt != 200 {
		t.Fatalf("救援请求 max_tokens=%v want 200（不得放大）", second["max_tokens"])
	}
}

// TestStreamEmptyReplyRescue 流式同一形态：换掉空流而不是把空回复透给客户端。
// 客户端不应看到第一跳的 finish_reason=length，也不应看到两个 [DONE]。
func TestStreamEmptyReplyRescue(t *testing.T) {
	const emptyTruncated = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	const rescued = "data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"流式救援\"}}]}\n\n" +
		"data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	var bodies []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw := readUpstreamBody(r)
			bodies = append(bodies, string(raw))
			body := emptyTruncated
			if len(bodies) > 1 {
				body = rescued
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{
		"model":"deepseek-v4.1-flash",
		"stream":true,
		"max_tokens":200,
		"messages":[{"role":"user","content":"hi"}]
	}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(bodies) != 2 {
		t.Fatalf("上游调用次数=%d want 2（首跳 + 救援跳）", len(bodies))
	}
	out := rec.Body.String()
	if !strings.Contains(out, "流式救援") {
		t.Fatalf("客户端应拿到救援流正文: %q", out)
	}
	if strings.Contains(out, `"length"`) {
		t.Fatalf("空回复的 finish_reason=length 不应透出: %q", out)
	}
	if n := strings.Count(out, "[DONE]"); n != 1 {
		t.Fatalf("[DONE] 次数=%d want 1: %q", n, out)
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(bodies[1]), &second); err != nil {
		t.Fatalf("救援请求体非法: %v", err)
	}
	th, _ := second["thinking"].(map[string]any)
	if th["type"] != "disabled" {
		t.Fatalf("救援请求 thinking=%v want disabled", second["thinking"])
	}
}

// TestModelHangCircuitRejectsAtEntry 模型级挂死熔断：连续停滞达阈值后，入口直接
// 503 + Retry-After，不再逐号白等一整个超时（上游调用次数必须为 0）。
func TestModelHangCircuitRejectsAtEntry(t *testing.T) {
	var calls int
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(sseOK))}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.NoteModelHang("cn", "glm-5.2")
	p.NoteModelHang("cn", "glm-5.2") // 第 2 次 → 熔断
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{
		"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503, body=%s", rec.Code, rec.Body)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("熔断应带 Retry-After")
	}
	if calls != 0 {
		t.Fatalf("熔断期不应打上游，calls=%d", calls)
	}
	if !strings.Contains(rec.Body.String(), "model_temporarily_down") {
		t.Fatalf("错误码应为 model_temporarily_down: %s", rec.Body)
	}
}

// TestModelQuotaLockReturns429 全池清一色 6004 配额锁：429 + 明确 code，
// 且**不带** Retry-After（带上会让部分客户端把整段冷却当成静默等待）。
func TestModelQuotaLockReturns429(t *testing.T) {
	var calls int
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(sseOK))}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.CooldownSoftForModel("u1", time.Hour, time.Now().Add(time.Hour), "glm-5.2", "6004 model rate limit")
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{
		"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429, body=%s", rec.Code, rec.Body)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "" {
		t.Fatalf("配额锁不得带 Retry-After，got %q", ra)
	}
	if !strings.Contains(rec.Body.String(), "model_quota_exhausted") {
		t.Fatalf("错误码应为 model_quota_exhausted: %s", rec.Body)
	}
	if calls != 0 {
		t.Fatalf("全池锁定时不应打上游，calls=%d", calls)
	}
}

// TestModelUnavailableStill400 混合原因（11102 确定性不可用）仍按 400 不可重试处理，
// 不能被配额分支误报成 429。
func TestModelUnavailableStill400(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(sseOK))}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.BlockModelBackoff("u1", "glm-5.2", "11102 model not available")
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{
		"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400, body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("错误码应为 model_unavailable: %s", rec.Body)
	}
}
