package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/alert"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// riskWebhook 起一个收告警的本地端点，返回 kind 通道。
func riskWebhook(t *testing.T) (<-chan string, *httptest.Server) {
	t.Helper()
	ch := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Kind string `json:"kind"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		select {
		case ch <- p.Kind:
		default:
		}
		w.WriteHeader(200)
	}))
	return ch, srv
}

// TestRiskSignalReportsChannelRefused 11128（上游判定请求不是官方客户端）必须
// 触发告警——这类长期是 0 条，一旦出现就意味着内容指纹被识破。
func TestRiskSignalReportsChannelRefused(t *testing.T) {
	ch, srv := riskWebhook(t)
	defer srv.Close()

	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 400, `{"code":"11-128","msg":"request illegal"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, Alert: alert.New(srv.URL, "", time.Hour)})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	select {
	case k := <-ch:
		if k != "channel_refused" {
			t.Fatalf("kind=%q want channel_refused", k)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("11128 未触发告警")
	}
}

// TestRiskSignalIgnoresOrdinaryErrors 普通限流/参数错误不该告警——429 和 400
// 天天有，报了只会让人对告警脱敏。
func TestRiskSignalIgnoresOrdinaryErrors(t *testing.T) {
	ch, srv := riskWebhook(t)
	defer srv.Close()

	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 429, `{"code":6004,"msg":"rate limited"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, Alert: alert.New(srv.URL, "", time.Hour)})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	select {
	case k := <-ch:
		t.Fatalf("普通限流不应告警，got kind=%q", k)
	case <-time.After(700 * time.Millisecond):
		// 期望：没有任何告警
	}
}

// TestRiskSignalNilAlerterNoPanic 未配置告警（nil）时整条路径正常，不 panic。
func TestRiskSignalNilAlerterNoPanic(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 400, `{"code":"11-128","msg":"request illegal"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code == 0 {
		t.Fatal("nil alerter 下请求未产生响应")
	}
}
