package upstream

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestMonitorBodyCloseDrainsTail Close 必须先把剩余字节读到底（EOF）再取消+关闭。
// 顺序反了（先 cancel）会让 Transport 判定 body 未读完，直接丢弃连接。
func TestMonitorBodyCloseDrainsTail(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := &countingReader{data: []byte("data: [DONE]\n\n0\r\n\r\n")}
	body := monitorBody(nopCloserBody{Reader: src}, 30*time.Second, cancel)

	// 模拟 SSE 读者：在 [DONE] 处就停，剩下的是结束块。
	buf := make([]byte, 13)
	if _, err := io.ReadFull(body, buf); err != nil {
		t.Fatalf("read head: %v", err)
	}
	before := src.n
	if err := body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !src.eof() {
		t.Fatalf("Close 未读掉流尾：关闭前读了 %d 字节，底层还有 %d 字节未读",
			before, len(src.data)-src.n)
	}
}

// TestMonitorBodyCloseIdempotent 续写层与处理函数各关一次是常态：第二次不得再读底流。
func TestMonitorBodyCloseIdempotent(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := &countingReader{data: []byte("tail")}
	body := monitorBody(nopCloserBody{Reader: src}, 30*time.Second, cancel)

	if err := body.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	after := src.n
	if err := body.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if src.n != after {
		t.Fatalf("第二次 Close 又读了底流：%d → %d", after, src.n)
	}
}

// TestStreamCloseKeepsUpstreamConnReusable 端到端：流在 [DONE] 处关闭后，
// 同一 Transport 的下一次请求必须复用同一条连接（不重做 TCP/TLS 握手）。
//
// 这条钉的是行为面：连接复用率就是「一条连接几次握手」的倒数，握手次数是
// 上游可统计的特征；改动前每条流都新建连接。
func TestStreamCloseKeepsUpstreamConnReusable(t *testing.T) {
	var newConns int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"n\":1}\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			atomic.AddInt32(&newConns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	tr := &http.Transport{}
	client := &http.Client{Transport: tr}
	defer tr.CloseIdleConnections()

	doOne := func() {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		body := monitorBody(resp.Body, 30*time.Second, cancel)
		br := bufio.NewReader(body)
		if _, err := br.ReadString('\n'); err != nil {
			t.Fatalf("read first frame: %v", err)
		}
		// 与 SSE 读者一致的收尾方式：读完首帧就关，不读到 EOF。
		if err := body.Close(); err != nil {
			t.Fatalf("close body: %v", err)
		}
	}

	doOne()
	// Transport 在 body 读到 EOF 后由 readLoop 异步把连接放回空闲池，给它一点点时间。
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&newConns) != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	doOne()

	if n := atomic.LoadInt32(&newConns); n != 1 {
		t.Fatalf("连接未复用：new conns = %d（期望 1）。Close 前没有读掉流尾", n)
	}
}

// countingReader 记录已读字节数，末尾返回 io.EOF。
type countingReader struct {
	data []byte
	n    int
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.n >= len(r.data) {
		return 0, io.EOF
	}
	// 一次只吐一点，模拟网络分片，逼出「只读首帧就停」的形态。
	chunk := 4
	if len(r.data)-r.n < chunk {
		chunk = len(r.data) - r.n
	}
	c := copy(p, r.data[r.n:r.n+chunk])
	r.n += c
	return c, nil
}

func (r *countingReader) eof() bool { return r.n >= len(r.data) }
