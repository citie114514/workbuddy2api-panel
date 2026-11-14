package upstream

import (
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestUIDTransportSeparatesAccounts 连接池必须按账号分桶：同账号复用连接、跨账号不复用。
//
// 这是「一条连接只能有一个身份」的线上证明——用裸 TCP 计数，而不是看内存里的 map。
// 共用单个 Transport 时（改造前）第二个账号会捡起第一个账号的空闲连接，连接数仍是 1。
func TestUIDTransportSeparatesAccounts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var accepted int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&accepted, 1)
			go func(conn net.Conn) {
				defer conn.Close()
				for {
					buf := make([]byte, 4096)
					if _, err := conn.Read(buf); err != nil {
						return
					}
					// Content-Length: 0 + HTTP/1.1 = 连接保持可复用
					if _, err := io.WriteString(conn,
						"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"); err != nil {
						return
					}
				}
			}(c)
		}
	}()

	cl := &http.Client{Timeout: 5 * time.Second, Transport: newUIDTransport()}
	base := "http://" + ln.Addr().String() + "/x"

	do := func(uid string) {
		req, err := http.NewRequest(http.MethodPost, base, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		attachAccountUID(req, &auth.Auth{UID: uid})
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatalf("uid=%s: %v", uid, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	do("acct-a")
	do("acct-a") // 同账号：应复用同一条连接
	if got := atomic.LoadInt32(&accepted); got != 1 {
		t.Fatalf("同账号两次请求应复用 1 条连接，实测 %d 条", got)
	}

	do("acct-b") // 换账号：必须新建连接
	if got := atomic.LoadInt32(&accepted); got != 2 {
		t.Fatalf("跨账号应新建连接（共 2 条），实测 %d 条——连接池未按账号隔离", got)
	}

	do("acct-b") // 新账号自身复用
	if got := atomic.LoadInt32(&accepted); got != 2 {
		t.Fatalf("同一新账号第二次应复用，实测 %d 条", got)
	}
}

// TestUIDTransportSharedBucket 未绑定账号的请求走 "" 桶（改造前行为），不 panic。
func TestUIDTransportSharedBucket(t *testing.T) {
	tr := newUIDTransport()
	a := tr.forUID("")
	b := tr.forUID("")
	if a != b {
		t.Fatal("空 UID 应复用同一个桶")
	}
	if tr.forUID("u1") == a {
		t.Fatal("绑定账号不得与共享桶共用连接池")
	}
}

// TestAttachAccountUIDKeepsWireClean UID 只进上下文，不新增任何请求头。
func TestAttachAccountUIDKeepsWireClean(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/x", nil)
	before := len(req.Header)
	attachAccountUID(req, &auth.Auth{UID: "u1"})
	if len(req.Header) != before {
		t.Fatalf("attachAccountUID 不应改请求头：%d → %d", before, len(req.Header))
	}
	if got := requestAccountUID(req); got != "u1" {
		t.Fatalf("上下文 UID = %q want u1", got)
	}
	// 兜底：只有 X-User-Id 头、没有上下文时也能取到。
	req2, _ := http.NewRequest(http.MethodPost, "https://example.com/x", nil)
	req2.Header.Set("X-User-Id", "u2")
	if got := requestAccountUID(req2); got != "u2" {
		t.Fatalf("头兜底 UID = %q want u2", got)
	}
}
