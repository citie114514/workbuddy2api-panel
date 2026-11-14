package upstream

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// msgIDRe /v2/report 的 X-Request-ID：32 位小写 hex。
var msgIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TestReportHeadersWireCase 断言 `/v2/report` 的**线上字节**与官方内置 CLI 一致。
//
// 基准是官方内置 CLI 进程的出站形态（13 个头）与官方桌面进程的（11 个头）。要点：
//   - 头名大小写必须原样（X-Request-ID 的 ID 全大写；Set 会规范化成 X-Request-Id）；
//   - 三个「不该有」的头不得出现：x-codebuddy-request / Accept-Language / X-Device-Token
//     （官方两条链都不发，它们是 billing 其它端点的形态）；
//   - Accept-Encoding 保留 Go 默认 gzip（已知残留差异）。
//
// 这里抓裸 TCP，因为 httptest/服务端的 r.Header 已按 textproto 规范化，看不到线上形态。
func TestReportHeadersWireCase(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	rawCh := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var buf bytes.Buffer
		tmp := make([]byte, 4096)
		for {
			n, err := c.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
				if bytes.Contains(buf.Bytes(), []byte("\r\n\r\n")) {
					break
				}
			}
			if err != nil {
				break
			}
		}
		rawCh <- buf.String()
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n{}")
	}()

	body := `[{"eventCode":"chat_request_send"}]`
	req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/v2/report",
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{}
	c.ReportHeaders(req, &auth.Auth{
		AccessToken: "at-report",
		UID:         "00000000-0000-4000-8000-000000000001",
		Domain:      "www.workbuddy.cn",
	})

	// 必须用项目自己的 uidTransport（线上出站都经它）：它负责注入 Accept-Encoding
	// 并挂解压链。用 http.DefaultClient 会绕过该层，测到标准库默认的 gzip，
	// 得到与线上不一致的假基线。
	resp, err := (&http.Client{Transport: newUIDTransport()}).Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_ = resp.Body.Close()
	wire := <-rawCh

	// 1) 必须出现、且取值精确（头名大小写原样）。
	wantExact := []string{
		"Accept: application/json, text/plain, */*\r\n",
		"Content-Type: application/json;charset=UTF-8\r\n",
		"X-Product: SaaS\r\n",
		"User-Agent: WorkBuddy/" + defaultClientVersion + "\r\n",
		"Authorization: Bearer at-report\r\n",
		"X-User-Id: 00000000-0000-4000-8000-000000000001\r\n",
		"X-Domain: www.workbuddy.cn\r\n",
		"Connection: keep-alive\r\n",
		// Accept-Encoding 由 uidTransport 统一注入官方 axios 全值（ReportHeaders 不设它，
		// 设了反而会覆盖掉统一注入）。必须走真实 transport 才看得到这个值。
		"Accept-Encoding: gzip, compress, deflate, br\r\n",
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n",
		"Host: " + ln.Addr().String() + "\r\n",
	}
	for _, k := range wantExact {
		if !strings.Contains(wire, "\r\n"+k) {
			t.Errorf("线上缺少 %q", k)
		}
	}

	// 2) X-Request-ID：ID 全大写 + 32 hex（每请求不同）。
	if !strings.Contains(wire, "\r\nX-Request-ID: ") {
		t.Errorf("线上缺少原始大小写 X-Request-ID，wire=%q", wire)
	}
	if strings.Contains(wire, "\r\nX-Request-Id: ") {
		t.Errorf("X-Request-ID 被规范化成 X-Request-Id（setExact 被绕过）")
	}
	if m := regexp.MustCompile(`\r\nX-Request-ID: ([0-9a-f]{32})\r\n`).FindStringSubmatch(wire); m == nil {
		t.Errorf("X-Request-ID 不是 32 位小写 hex，wire=%q", wire)
	}

	// 3) 官方没有的头，一个都不能有。
	wantAbsent := []string{
		"\r\nx-codebuddy-request:", "\r\nX-CodeBuddy-Request:",
		"\r\nAccept-Language:", "\r\nX-Device-Token:", "\r\nX-Api-Key:",
		"\r\nX-Requested-With:",
		"\r\nTransfer-Encoding:",
	}
	for _, k := range wantAbsent {
		if strings.Contains(wire, k) {
			t.Errorf("线上出现了官方没有的头 %q", k)
		}
	}
}

// TestReportChatActivityHeaders 钉住接线：ReportChatActivity 必须走 ReportHeaders
// （而不是旧 BillingHeaders）。服务端 r.Header 已规范化，因此只断取值与存在性。
func TestReportChatActivityHeaders(t *testing.T) {
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at-wire", UID: "u-wire", Domain: "www.workbuddy.cn"}
	if err := c.ReportChatActivity(a, "wb2api-1", ""); err != nil {
		t.Fatalf("report: %v", err)
	}

	want := map[string]string{
		"Accept":       "application/json, text/plain, */*",
		"Content-Type": "application/json;charset=UTF-8",
		"X-Product":    "SaaS",
		"User-Agent":   "WorkBuddy/" + defaultClientVersion,
		"X-User-Id":    "u-wire",
		"X-Domain":     "www.workbuddy.cn",
		"Connection":   "keep-alive",
	}
	for k, v := range want {
		if got := seen.Get(k); got != v {
			t.Errorf("%s=%q want %q", k, got, v)
		}
	}
	if got := seen.Get("X-Request-ID"); !msgIDRe.MatchString(got) {
		t.Errorf("X-Request-ID=%q want 32 hex", got)
	}
	for _, k := range []string{"x-codebuddy-request", "Accept-Language", "X-Device-Token"} {
		if got := seen.Get(k); got != "" {
			t.Errorf("%s=%q 不该出现在 /v2/report", k, got)
		}
	}
}
