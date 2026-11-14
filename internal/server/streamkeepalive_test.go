package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 保活层的行为约定：
//   - 首字节写出之前，按间隔持续写 `: ping`；
//   - 一旦写出真实内容，立刻停手（此后由流内保活接管）；
//   - 快请求（间隔内就写出内容）一个注释帧都不写，行为与改动前逐字节一致。

func TestStreamKeepAlivePingsBeforeRealWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	k := newStreamKeepAlive(rec)
	k.start(20 * time.Millisecond)
	time.Sleep(90 * time.Millisecond)
	k.stop()

	body := rec.Body.String()
	if strings.Count(body, ": ping\n\n") < 2 {
		t.Fatalf("等待期间应写多个注释帧，实际 body=%q", body)
	}
	if !k.Committed() {
		t.Fatal("写过注释帧后 Committed() 应为 true")
	}
	// 注释帧必须先于任何真实内容——这正是它的意义。
	if strings.Contains(body, "data: ") {
		t.Fatalf("不应出现真实内容帧，body=%q", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("注释帧必须自带流式响应头，实际 Content-Type=%q", ct)
	}
	if v := rec.Header().Get("X-Accel-Buffering"); v != "no" {
		t.Fatalf("必须关掉 nginx 缓冲，实际 X-Accel-Buffering=%q", v)
	}
}

func TestStreamKeepAliveStopsAfterRealWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	k := newStreamKeepAlive(rec)
	k.start(20 * time.Millisecond)
	time.Sleep(60 * time.Millisecond) // 至少先发一帧
	if _, err := io.WriteString(k, "data: hello\n\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(80 * time.Millisecond) // 若未停手，这段时间会再写若干帧
	k.stop()

	body := rec.Body.String()
	idx := strings.Index(body, "data: hello")
	if idx < 0 {
		t.Fatalf("真实内容缺失，body=%q", body)
	}
	if strings.Contains(body[idx:], ": ping") {
		t.Fatalf("真实内容之后不应再有注释帧（会撕裂客户端解析），body=%q", body)
	}
}

func TestStreamKeepAliveSilentWhenFast(t *testing.T) {
	rec := httptest.NewRecorder()
	k := newStreamKeepAlive(rec)
	k.start(time.Second)
	if _, err := io.WriteString(k, "data: fast\n\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	k.stop()

	if body := rec.Body.String(); strings.Contains(body, ": ping") {
		t.Fatalf("间隔内就写出内容的快请求不应产生注释帧，body=%q", body)
	}
}

// 响应头已被注释帧提交时，错误响应必须降级成流内 error 帧——否则客户端会在一条
// 已开的 SSE 流里读到一段解析不了的 JSON（状态码已是 200，改不回去）。
func TestWriteOpenAIErrorDowngradesAfterCommit(t *testing.T) {
	rec := httptest.NewRecorder()
	k := newStreamKeepAlive(rec)
	k.start(10 * time.Millisecond)
	time.Sleep(40 * time.Millisecond)
	k.stop()
	if !k.Committed() {
		t.Fatal("前置条件不成立：应已提交")
	}

	downgraded := writeOpenAIErrorHint(k, http.StatusBadRequest, "prompt_too_long", "too long", "hint-here")
	if !downgraded {
		t.Fatal("响应头已提交时必须报告降级")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: {") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("应写成 SSE error 帧 + 结束帧，实际 body=%q", body)
	}
	var payload struct {
		Error struct {
			Code        string `json:"code"`
			Message     string `json:"message"`
			GatewayHint string `json:"gateway_hint"`
		} `json:"error"`
	}
	line := body[strings.Index(body, "data: {")+len("data: "):]
	line = line[:strings.Index(line, "\n")]
	if err := json.Unmarshal([]byte(line), &payload); err != nil {
		t.Fatalf("error 帧不是合法 JSON: %v (%q)", err, line)
	}
	if payload.Error.Code != "prompt_too_long" || payload.Error.Message != "too long" || payload.Error.GatewayHint != "hint-here" {
		t.Fatalf("error 帧字段不符: %+v", payload.Error)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("已提交的响应状态不应被改写，实际 %d", rec.Code)
	}
}

// 未提交时行为必须与改动前完全一致：状态码 + JSON。
func TestWriteOpenAIErrorKeepsJSONWhenNotCommitted(t *testing.T) {
	rec := httptest.NewRecorder()
	k := newStreamKeepAlive(rec)

	downgraded := writeOpenAIErrorHint(k, http.StatusBadRequest, "prompt_too_long", "too long", "")
	if downgraded {
		t.Fatal("响应头未提交时不应降级")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("ct=%q", ct)
	}
	var payload struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("不是 JSON: %v body=%s", err, rec.Body)
	}
	if payload.Error["code"] != "prompt_too_long" {
		t.Fatalf("error=%v", payload.Error)
	}
}

// 端到端：上游迟迟不回响应头时，客户端必须在上游首个内容帧之前先收到注释帧。
// 这是本次改动的全部意义——handler 层保活覆盖「响应头到达之前」那一段。
func TestStreamPingReachesClientBeforeUpstreamResponds(t *testing.T) {
	delay := upstream.DefaultStreamPingInterval + 200*time.Millisecond
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			// 模拟「建连 + 上游排队/预填」耗时：这段时间网关还没有响应头可写。
			time.Sleep(delay)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	pingAt := strings.Index(body, ": ping")
	if pingAt < 0 {
		t.Fatalf("上游慢于保活间隔时必须先发注释帧，实际 body=%q", body)
	}
	contentAt := strings.Index(body, "data: ")
	if contentAt < 0 {
		t.Fatalf("内容帧缺失，body=%q", body)
	}
	if pingAt > contentAt {
		t.Fatalf("注释帧必须在内容帧之前，ping@%d content@%d body=%q", pingAt, contentAt, body)
	}
	// 内容本身不能被保活破坏。
	if !strings.Contains(body, "data: [DONE]") || !strings.Contains(body, "你好") {
		t.Fatalf("正文被破坏，body=%q", body)
	}
}
