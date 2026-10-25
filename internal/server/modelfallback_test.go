package server

// modelfallback_test.go v1.15 模型付费回退端到端：免费额度型模型被 6004 全池
// 模型级阻塞时，选号改用付费兄弟型号（-sg）重选一次 —— 对齐官方客户端行为。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// newFallbackHandler 构造「主模型已 6004 全池阻塞」的测试环境：
//   - 单 global 账号，主模型 deepseek-v4.1-flash 被 BlockModelBackoff 锁定；
//   - 目录里知道 -sg 型号（x0.03），主模型 x0.00；
//   - fake 上游记录收到的出站 model 字段并正常回 SSE。
func newFallbackHandler(t *testing.T, modelFallbackEnabled bool, custom map[string]string) (*Handler, *atomic.Value) {
	t.Helper()
	var seenModel atomic.Value
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	// 包一层记录出站 body 的 model 字段。
	base := up.HTTP.Transport
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Body != nil {
			var peek struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&peek)
			seenModel.Store(peek.Model)
		}
		return base.RoundTrip(r)
	})
	g := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}
	auth.BackfillRealmFor(g, "global")
	p := testPoolWith(g)
	p.SetModelRateOf(func(realm, model string) string {
		if model == "deepseek-v4.1-flash" {
			return "x0.00"
		}
		if model == "deepseek-v4.1-flash-sg" {
			return "x0.03"
		}
		return ""
	})
	p.SetModelFallback(modelFallbackEnabled, custom)
	// 免费额度用尽形态：6004 模型级冷却（BlockModelBackoff 指数 TTL）。
	p.BlockModelBackoff("u1", "deepseek-v4.1-flash", "6004 model rate limit")
	h := NewHandler(Config{Pool: p, Upstream: up})
	return h, &seenModel
}

func doFallbackRequest(t *testing.T, h *Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"global:deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestChatFallsBackToPaidSibling 主模型 6004 全池阻塞 → 自动改用 -sg 付费型号出站。
func TestChatFallsBackToPaidSibling(t *testing.T) {
	h, seen := newFallbackHandler(t, true, nil)
	rec := doFallbackRequest(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got, _ := seen.Load().(string); got != "deepseek-v4.1-flash-sg" {
		t.Errorf("upstream model=%q, want paid sibling deepseek-v4.1-flash-sg", got)
	}
}

// TestChatFallbackDisabled 开关关闭 → 维持既有 model_blocked 口径（503），不出站。
func TestChatFallbackDisabled(t *testing.T) {
	h, seen := newFallbackHandler(t, false, nil)
	rec := doFallbackRequest(t, h)
	// 既有口径：模型级阻塞 = 400 model_unavailable（model_blocked 在 gateway_hint）。
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model_blocked") {
		t.Errorf("body must carry model_blocked hint: %s", rec.Body)
	}
	if seen.Load() != nil {
		t.Errorf("disabled fallback must not hit upstream, model=%v", seen.Load())
	}
}

// TestChatFallbackCustomMap 显式映射优先于内置规则。
func TestChatFallbackCustomMap(t *testing.T) {
	h, seen := newFallbackHandler(t, true, map[string]string{"global:deepseek-v4.1-flash": "deepseek-v4.1-flash-sg"})
	rec := doFallbackRequest(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got, _ := seen.Load().(string); got != "deepseek-v4.1-flash-sg" {
		t.Errorf("custom map target=%q", got)
	}
}

// TestChatFallbackExplicitDisable 显式空值映射 = 该模型禁用回退。
func TestChatFallbackExplicitDisable(t *testing.T) {
	h, seen := newFallbackHandler(t, true, map[string]string{"global:deepseek-v4.1-flash": ""})
	rec := doFallbackRequest(t, h)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if seen.Load() != nil {
		t.Errorf("explicitly disabled model must not hit upstream, model=%v", seen.Load())
	}
}

// TestChatFallbackTargetAlsoBlocked 回退型号也被模型级阻塞 → 一次回退后按
// model_blocked 收场（不递归 "-sg-sg"、不无限轮转）。
func TestChatFallbackTargetAlsoBlocked(t *testing.T) {
	h, seen := newFallbackHandler(t, true, nil)
	h.cfg.Pool.BlockModelBackoff("u1", "deepseek-v4.1-flash-sg", "6004 model rate limit")
	rec := doFallbackRequest(t, h)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model_blocked") {
		t.Errorf("body must carry model_blocked hint: %s", rec.Body)
	}
	if seen.Load() != nil {
		t.Errorf("blocked sibling must not hit upstream, model=%v", seen.Load())
	}
	// 仍应是主模型的阻塞快照（回退没成功不改写错误口径）。
	if !bytes.Contains(rec.Body.Bytes(), []byte("deepseek-v4.1-flash")) {
		t.Errorf("blocked reason should reference the model: %s", rec.Body)
	}
}
