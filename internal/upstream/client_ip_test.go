package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// resetReportClientIPForTest 清空进程级缓存，让每个用例从干净状态开始。
func resetReportClientIPForTest() {
	reportClientIPOnce = sync.Once{}
	reportClientIPVal = ""
}

// TestInjectReportClientIP 探测成功时给每条事件补 clientIp，已有值不覆盖。
func TestInjectReportClientIP(t *testing.T) {
	resetReportClientIPForTest()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != reportClientIPPath {
			t.Errorf("探测路径 = %q want %q", r.URL.Path, reportClientIPPath)
		}
		// 官方这条是裸请求，不应带鉴权头。
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("探测请求不该带 Authorization，实际 %q", got)
		}
		w.Write([]byte(`{"ip":"203.0.113.9","country":"CN"}`))
	}))
	defer srv.Close()

	c := &Client{ChatBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	body := []byte(`[{"eventCode":"chat_request_send"},{"eventCode":"chat_message_send","clientIp":"198.51.100.7"}]`)
	out := c.injectReportClientIP(a, body)

	var evs []map[string]any
	if err := json.Unmarshal(out, &evs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("事件数 = %d want 2", len(evs))
	}
	if got := evs[0]["clientIp"]; got != "203.0.113.9" {
		t.Errorf("event[0].clientIp = %v want 203.0.113.9", got)
	}
	if got := evs[1]["clientIp"]; got != "198.51.100.7" {
		t.Errorf("已有的 clientIp 被覆盖: %v", got)
	}
}

// TestInjectReportClientIPSkipsOnFailure 探测失败时原样返回，不写空值也不写脏值。
func TestInjectReportClientIPSkipsOnFailure(t *testing.T) {
	cases := []struct {
		name string
		h    http.HandlerFunc
	}{
		{"非 200", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }},
		{"非 JSON", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`<html>oops</html>`)) }},
		{"没有 ip 字段", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"country":"CN"}`)) }},
		{"ip 不是地址", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ip":"not-an-ip"}`)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetReportClientIPForTest()
			srv := httptest.NewServer(tc.h)
			defer srv.Close()

			c := &Client{ChatBaseCN: srv.URL}
			body := []byte(`[{"eventCode":"chat_request_send"}]`)
			out := c.injectReportClientIP(&auth.Auth{AccessToken: "at"}, body)
			if string(out) != string(body) {
				t.Errorf("探测失败时不该改动 body：%s", out)
			}
		})
	}
}

// TestInjectReportClientIPNonArray 非数组负载原样返回（不动非事件批量）。
func TestInjectReportClientIPNonArray(t *testing.T) {
	resetReportClientIPForTest()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ip":"203.0.113.9"}`))
	}))
	defer srv.Close()

	c := &Client{ChatBaseCN: srv.URL}
	body := []byte(`{"eventCode":"single"}`)
	out := c.injectReportClientIP(&auth.Auth{AccessToken: "at"}, body)
	if string(out) != string(body) {
		t.Errorf("非数组负载被改动：%s", out)
	}
}

// TestReportDesktopEventCarriesClientIP 桌面事件链的每条事件都要带 clientIp，
// 且必须加在白名单裁剪之后（否则会被当作「官方没有的字段」删掉）。
func TestReportDesktopEventCarriesClientIP(t *testing.T) {
	resetReportClientIPForTest()
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == reportClientIPPath {
			w.Write([]byte(`{"ip":"203.0.113.9"}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"code":0,"msg":"ok"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	err := c.ReportDesktopEvent(&auth.Auth{AccessToken: "at", UID: "u1"},
		DesktopEvent{"eventCode": "chat_request_send"})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("事件数 = %d want 1", len(got))
	}
	if v, _ := got[0]["clientIp"].(string); v != "203.0.113.9" {
		t.Errorf("clientIp = %v want 203.0.113.9", got[0]["clientIp"])
	}
}
