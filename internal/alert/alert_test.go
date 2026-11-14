package alert

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewEmptyWebhookReturnsNil 未配 webhook 时返回 nil，调用方按 nil 短路。
func TestNewEmptyWebhookReturnsNil(t *testing.T) {
	if n := New("", "", 0); n != nil {
		t.Fatalf("空 webhook 应返回 nil，got %+v", n)
	}
}

// TestNilNotifierSafe nil 接收者上调用 Report 必须安全（未配置告警是常态）。
func TestNilNotifierSafe(t *testing.T) {
	var n *Notifier
	if n.Report("k", "t", "d") {
		t.Fatal("nil Notifier 不应返回 true")
	}
}

// TestReportDeliversPayload 投递体字段是 PushPlus 兼容的 title/content，
// 另带 kind/at 供通用 webhook 使用。
func TestReportDeliversPayload(t *testing.T) {
	type got struct {
		Title, Content, Kind, Template string
	}
	ch := make(chan got, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type=%s", ct)
		}
		var p payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode: %v", err)
		}
		select {
		case ch <- got{p.Title, p.Content, p.Kind, p.Template}:
		default:
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	n := New(srv.URL, "tok-123", time.Minute)
	if ok := n.Report("channel_refused", "标题", "细节"); !ok {
		t.Fatal("首次上报应投递")
	}
	select {
	case g := <-ch:
		if g.Title != "标题" || g.Content != "细节" || g.Kind != "channel_refused" {
			t.Fatalf("投递体不符: %+v", g)
		}
		if g.Template != "markdown" {
			t.Errorf("template=%q want markdown（PushPlus 渲染用）", g.Template)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("超时未收到投递")
	}
}

// TestReportDeduplicatesByKind 同类事件在静默窗口内只投一次；不同 kind 互不影响。
// 风控信号往往成串出现，逐条推送会让人对告警脱敏。
func TestReportDeduplicatesByKind(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	n := New(srv.URL, "", time.Hour)
	for i := 0; i < 5; i++ {
		n.Report("waf_block", "t", "d")
	}
	if ok := n.Report("session_dead", "t", "d"); !ok {
		t.Fatal("不同 kind 不应被静默")
	}
	// 给异步投递一点时间
	time.Sleep(200 * time.Millisecond)
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("投递次数=%d want 2（同类 5 次只发 1 次 + 另一类 1 次）", got)
	}
}

// TestSendFailureDoesNotPanic 投递失败只记日志，绝不影响调用方（请求路径）。
func TestSendFailureDoesNotPanic(t *testing.T) {
	n := New("http://127.0.0.1:1/none", "", time.Hour) // 必然连不上
	n.Report("k", "t", "d")
	time.Sleep(300 * time.Millisecond) // 崩了会在这里被 -race/panic 捕获
}
