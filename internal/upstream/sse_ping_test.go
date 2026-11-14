package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type pingTestWriter struct {
	header http.Header
	writes chan string
}

func newPingTestWriter() *pingTestWriter {
	return &pingTestWriter{header: make(http.Header), writes: make(chan string, 64)}
}

func (w *pingTestWriter) Header() http.Header         { return w.header }
func (w *pingTestWriter) WriteHeader(statusCode int)  {}
func (w *pingTestWriter) Flush()                      {}
func (w *pingTestWriter) Write(p []byte) (int, error) { w.writes <- string(p); return len(p), nil }

func waitForSSEWrite(t *testing.T, writes <-chan string, want string) string {
	t.Helper()
	var out strings.Builder
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for !strings.Contains(out.String(), want) {
		select {
		case s := <-writes:
			out.WriteString(s)
		case <-timer.C:
			t.Fatalf("等待输出 %q 超时，已收到 %q", want, out.String())
		}
	}
	return out.String()
}

func waitForSSEStream(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("StreamHint 未及时返回")
		return nil
	}
}

func TestStreamHintSendsPingDuringUpstreamSilence(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	w := newPingTestWriter()
	done := make(chan error, 1)
	go func() {
		done <- StreamHint(w, pr, nil, WithPingInterval(25*time.Millisecond))
	}()

	out := waitForSSEWrite(t, w.writes, ": ping\n\n")
	if _, err := io.WriteString(pw, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"after silence\"}}]}\n\ndata: [DONE]\n\n"); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	out += waitForSSEWrite(t, w.writes, "data: [DONE]")
	if err := waitForSSEStream(t, done); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	if !strings.Contains(out, `"content":"after silence"`) {
		t.Fatalf("静默后的正文丢失：%q", out)
	}
	if got := strings.Count(out, "data: [DONE]"); got != 1 {
		t.Fatalf("[DONE] 出现 %d 次，期望 1 次：%q", got, out)
	}
}

func TestStreamHintPingKeepsFrameOrder(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	defer pw.Close()
	w := newPingTestWriter()
	done := make(chan error, 1)
	go func() {
		done <- StreamHint(w, pr, nil, WithPingInterval(25*time.Millisecond))
	}()

	if _, err := io.WriteString(pw, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"}}]}\n\n"); err != nil {
		t.Fatal(err)
	}
	out := waitForSSEWrite(t, w.writes, ": ping\n\n")
	if _, err := io.WriteString(pw, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"second\"}}]}\n\ndata: [DONE]\n\n"); err != nil {
		t.Fatal(err)
	}
	_ = pw.Close()
	out += waitForSSEWrite(t, w.writes, "data: [DONE]")
	if err := waitForSSEStream(t, done); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}

	first := strings.Index(out, `"content":"first"`)
	ping := strings.Index(out, ": ping\n\n")
	second := strings.Index(out, `"content":"second"`)
	if first < 0 || ping < 0 || second < 0 || !(first < ping && ping < second) {
		t.Fatalf("保活与正文顺序错误：first=%d ping=%d second=%d，输出=%q", first, ping, second, out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("流应以 [DONE] 结尾：%q", out)
	}
}

func TestStreamHintTinyPositivePingInterval(t *testing.T) {
	raw := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"
	w := httptest.NewRecorder()
	if err := StreamHint(w, strings.NewReader(raw), nil, WithPingInterval(time.Nanosecond)); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	if !strings.Contains(w.Body.String(), `"content":"ok"`) {
		t.Fatalf("正文丢失：%q", w.Body.String())
	}
}

// 保活间隔必须显著小于「客户端零字节等待窗口」。
//
// 线上 interrupted 请求里绝大多数 duration 与 ttfb 几乎相等——客户端在首字节到达前
// 等几秒就走了。间隔若接近或超过这个窗口，注释帧永远来不及发，等于没有保活。
func TestDefaultStreamPingIntervalShortEnough(t *testing.T) {
	// 上限取 1.5 秒：线上 interrupted 请求的「总耗时 ≈ 首字节耗时」，说明客户端的
	// 零字节等待窗口只有 1 秒量级；间隔一旦超过 1.5 秒就等于从未发出过。
	if DefaultStreamPingInterval > 1500*time.Millisecond {
		t.Fatalf("默认保活间隔 %v 过长：客户端在零字节状态等 1 秒量级就会断开", DefaultStreamPingInterval)
	}
	if DefaultStreamPingInterval <= 0 {
		t.Fatalf("默认保活间隔必须为正，实际 %v", DefaultStreamPingInterval)
	}
}
