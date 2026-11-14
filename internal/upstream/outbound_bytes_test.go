package upstream

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// TestPrepareBodyNoHTMLEscape 出站请求体不得把 < > & 转义成 \u003c / \u003e / \u0026。
//
// 依据是官方 CLI 的抓包：Node 的 JSON.stringify 不碰这三个字符，而 Go 的
// json.Marshal 默认转义。78KB 的系统提示里通常有几十个 <>&（代码块、箭头、
// 比较运算），实测官方 77907B / 我们 78297B，差值 390 = 78 字符 × 5 —— 体积差
// 100% 来自这里。这类差异在按体积、按字节哈希、按内容特征做的判定里都成立。
func TestPrepareBodyNoHTMLEscape(t *testing.T) {
	src := []byte(`{"model":"m","messages":[{"role":"system","content":"if a < b && c > d { }"}]}`)
	out := PrepareBodyOpt(src, false)

	for _, esc := range [][]byte{[]byte(`\u003c`), []byte(`\u003e`), []byte(`\u0026`)} {
		if bytes.Contains(out, esc) {
			t.Errorf("出站 body 仍含 %s：%s", esc, out)
		}
	}
	if !bytes.Contains(out, []byte("a < b && c > d")) {
		t.Errorf("原文未逐字保留：%s", out)
	}
	// Encoder.Encode 会补换行，必须去掉（官方 body 末尾无换行）。
	if n := len(out); n > 0 && out[n-1] == '\n' {
		t.Errorf("出站 body 末尾不应有换行：%q", out[n-3:])
	}
}

// TestChatHeadersWireCase 头名在**线上**必须保持官方客户端的大小写。
//
// http.Header.Set 会规范化键名（x-stainless-arch → X-Stainless-Arch、
// X-B3-TraceId → X-B3-Traceid、X-Conversation-ID → X-Conversation-Id），而官方
// 客户端是 Node，按代码里写的键名原样发送。这里抓裸 TCP 字节来钉住：
// 断言的是**线上形态**，不是内存里的 map 形态。
func TestChatHeadersWireCase(t *testing.T) {
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
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
	}()

	req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	(&Client{}).ChatHeaders(req, testAuth(), "", ChatMeta{})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	wire := <-rawCh

	// 必须保持原始大小写的头（官方抓包里的形态）。
	wantExact := []string{
		"x-stainless-arch:", "x-stainless-lang:", "x-stainless-os:",
		"x-stainless-package-version:", "x-stainless-retry-count:",
		"x-stainless-runtime:", "x-stainless-runtime-version:",
		"x-codebuddy-request:",
		"X-Request-ID:", "X-Conversation-ID:", "X-Conversation-Message-ID:",
		"X-Conversation-Request-ID:", "X-Root-Request-ID:",
		"X-IDE-Name:", "X-IDE-Type:", "X-IDE-Version:",
		"X-Trace-ID:", "X-B3-TraceId:", "X-B3-SpanId:", "X-B3-Sampled:",
		"Connection:",
		"traceparent:", "b3:", "baggage:",
	}
	for _, k := range wantExact {
		if !strings.Contains(wire, "\r\n"+k) {
			t.Errorf("线上缺少原始大小写的头 %q", k)
		}
	}

	// 规范化形态不得出现（出现了说明某处又走了 Set）。
	wantAbsent := []string{
		"X-Stainless-Arch:", "X-Stainless-Runtime:", "X-B3-Traceid:",
		"X-Conversation-Id:", "X-Request-Id:", "X-Ide-Type:", "Traceparent:", "Baggage:", "B3:",
	}
	for _, k := range wantAbsent {
		if strings.Contains(wire, "\r\n"+k) {
			t.Errorf("线上出现了规范化头 %q（setExact 被绕过）", k)
		}
	}
}
