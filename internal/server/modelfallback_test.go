package server

// modelfallback_test.go 模型付费回退端到端：免费额度型模型被 6004 全池模型级
// 配额阻塞时，选号改用付费兄弟型号（-sg）重选一次 —— 对齐官方客户端行为。
//
// 移植自 citie114514 fork v1.15；基线换成 integrated 后做了两处适配：
//  1. 出站请求体在 integrated 上默认 gzip（client.go 压缩默认级别），抓 model
//     字段前必须先按 Content-Encoding 解压，否则解出来是二进制；
//  2. 错误口径跟随 integrated：6004/6000/6008 属用量配额，按 429 +
//     model_quota_exhausted 返回（旧 fork 上是 400 + model_blocked）。

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// decodeOutbound 解压并解析出站 body 的 model 字段（gzip 或明文）。
func decodeOutbound(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil || len(raw) == 0 {
		return ""
	}
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		zr, gerr := gzip.NewReader(bytes.NewReader(raw))
		if gerr != nil {
			return ""
		}
		if plain, derr := io.ReadAll(zr); derr == nil {
			raw = plain
		}
	}
	var peek struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(raw, &peek) != nil {
		return ""
	}
	return peek.Model
}

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
	// 包一层记录出站 body 的 model 字段（解压后读）。
	base := up.HTTP.Transport
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seenModel.Store(decodeOutbound(r))
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

// TestChatFallbackDisabled 开关关闭 → 维持既有配额阻塞口径（429），不出站。
func TestChatFallbackDisabled(t *testing.T) {
	h, seen := newFallbackHandler(t, false, nil)
	rec := doFallbackRequest(t, h)
	// integrated 口径：6004 属用量配额 → 429 + model_quota_exhausted。
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model_quota_exhausted") {
		t.Errorf("body must carry model_quota_exhausted: %s", rec.Body)
	}
	if got, _ := seen.Load().(string); got != "" {
		t.Errorf("disabled fallback must not hit upstream, model=%q", got)
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
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got, _ := seen.Load().(string); got != "" {
		t.Errorf("explicitly disabled model must not hit upstream, model=%q", got)
	}
}

// TestChatFallbackTargetAlsoBlocked 回退型号也被模型级阻塞 → 一次回退后按
// 配额阻塞口径收场（不递归 "-sg-sg"、不无限轮转）。
func TestChatFallbackTargetAlsoBlocked(t *testing.T) {
	h, seen := newFallbackHandler(t, true, nil)
	h.cfg.Pool.BlockModelBackoff("u1", "deepseek-v4.1-flash-sg", "6004 model rate limit")
	rec := doFallbackRequest(t, h)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model_quota_exhausted") {
		t.Errorf("body must carry model_quota_exhausted: %s", rec.Body)
	}
	if got, _ := seen.Load().(string); got != "" {
		t.Errorf("blocked sibling must not hit upstream, model=%q", got)
	}
	// 回退没成功不改写错误口径：hint 仍是模型级阻塞计数 + 上游 6004 原因。
	if !strings.Contains(rec.Body.String(), "6004 model rate limit") {
		t.Errorf("blocked reason should carry upstream reason: %s", rec.Body)
	}
}
