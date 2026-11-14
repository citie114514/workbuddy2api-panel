package upstream

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// readJA3 从裸连接读一个 TLS ClientHello，按 JA3 规范拼出原文：
// `version,ciphers,extensions,curves,point_formats`（十进制、`-` 分隔）。
//
// 只解析握手记录，不参与握手——这是「我们到底发出去了什么」最直接的证据，
// 不依赖任何第三方指纹库的口径。
func readJA3(r io.Reader) (string, error) {
	br := bufio.NewReader(r)
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return "", fmt.Errorf("读记录头: %w", err)
	}
	if hdr[0] != 0x16 {
		return "", fmt.Errorf("不是握手记录: type=%#x", hdr[0])
	}
	recLen := int(binary.BigEndian.Uint16(hdr[3:5]))
	body := make([]byte, recLen)
	if _, err := io.ReadFull(br, body); err != nil {
		return "", fmt.Errorf("读记录体: %w", err)
	}
	if len(body) < 4+34 || body[0] != 0x01 {
		return "", fmt.Errorf("不是 ClientHello: %#x", body[0])
	}
	p := 4 // 跳过 handshake 头（type + 3 字节长度）

	ver := binary.BigEndian.Uint16(body[p : p+2])
	p += 2
	p += 32 // random
	if p >= len(body) {
		return "", fmt.Errorf("截断: session_id 长度前")
	}
	sessLen := int(body[p])
	p++
	p += sessLen

	if p+2 > len(body) {
		return "", fmt.Errorf("截断: cipher_suites 长度")
	}
	csLen := int(binary.BigEndian.Uint16(body[p : p+2]))
	p += 2
	if p+csLen > len(body) {
		return "", fmt.Errorf("截断: cipher_suites 内容")
	}
	var ciphers []string
	for i := 0; i < csLen; i += 2 {
		ciphers = append(ciphers, strconv.Itoa(int(binary.BigEndian.Uint16(body[p+i:p+i+2]))))
	}
	p += csLen

	if p >= len(body) {
		return "", fmt.Errorf("截断: 压缩方法")
	}
	compLen := int(body[p])
	p += 1 + compLen

	var exts, curves, formats []string
	if p+2 <= len(body) {
		extLen := int(binary.BigEndian.Uint16(body[p : p+2]))
		p += 2
		for i := 0; i < extLen && p+4 <= len(body); {
			et := binary.BigEndian.Uint16(body[p : p+2])
			el := int(binary.BigEndian.Uint16(body[p+2 : p+4]))
			data := body[p+4 : p+4+el]
			exts = append(exts, strconv.Itoa(int(et)))
			switch et {
			case 10: // supported_groups
				if len(data) >= 2 {
					n := int(binary.BigEndian.Uint16(data[:2]))
					for j := 2; j+2 <= 2+n && j+2 <= len(data); j += 2 {
						curves = append(curves, strconv.Itoa(int(binary.BigEndian.Uint16(data[j:j+2]))))
					}
				}
			case 11: // ec_point_formats
				if len(data) >= 1 {
					n := int(data[0])
					for j := 1; j <= n && j < len(data); j++ {
						formats = append(formats, strconv.Itoa(int(data[j])))
					}
				}
			}
			p += 4 + el
			i += 4 + el
		}
	}
	return fmt.Sprintf("%d,%s,%s,%s,%s",
		ver, strings.Join(ciphers, "-"), strings.Join(exts, "-"),
		strings.Join(curves, "-"), strings.Join(formats, "-")), nil
}

// TestOfficialClientHelloJA3 最直接的一条：把我们的握手接到 net.Pipe 上，
// 解析真正发出的 ClientHello，按 JA3 规范拼出来的原文必须与官方抓包逐字节相等。
//
// 它同时钉住四件事：cipher 列表与顺序、扩展集合与**出现顺序**、曲线顺序、
// 点格式。任何一项漂移（例如误加 ALPN、误加 padding、把 cipher 排序）都会让
// 字符串不等。
func TestOfficialClientHelloJA3(t *testing.T) {
	cli, srv := net.Pipe()
	defer srv.Close()

	ja3c := make(chan string, 1)
	errc := make(chan error, 1)
	go func() {
		ja3, err := readJA3(srv)
		if err != nil {
			errc <- err
			return
		}
		ja3c <- ja3
	}()

	// 对面不是真 TLS 服务器（读完 ClientHello 就关），握手必然失败——
	// 这里只关心发出去的字节。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		_, _ = handshakeOfficialHello(ctx, cli, "www.codebuddy.cn")
	}()

	select {
	case got := <-ja3c:
		if got != officialJA3 {
			t.Fatalf("JA3 与官方不一致\n got=%s\nwant=%s", got, officialJA3)
		}
		// 再用社区通用的 md5 兜一层：三份独立实现的常量都是这个值。
		sum := fmt.Sprintf("%x", md5.Sum([]byte(got)))
		if sum != officialJA3MD5 {
			t.Fatalf("JA3 md5 不一致: got=%s want=%s", sum, officialJA3MD5)
		}
	case err := <-errc:
		t.Fatalf("读取 ClientHello 失败: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("超时：没有读到 ClientHello")
	}
}

// TestOfficialClientHelloHasNoALPN 单独钉住「不发 ALPN」：这是官方 Node 主进程
// 形态与渲染器 Chromium 形态（ALPN=h2）的分界，也是最容易被「顺手加上更现代」
// 改坏的一处。
func TestOfficialClientHelloHasNoALPN(t *testing.T) {
	spec := officialClientHelloSpec()
	// 用类型名判定：ALPN 与 padding 两个扩展都不允许出现。
	for i, ext := range spec.Extensions {
		name := fmt.Sprintf("%T", ext)
		if strings.Contains(name, "ALPN") {
			t.Fatalf("第 %d 个扩展是 %s：官方 ClientHello 不带 ALPN（禁 h2 后声明 ALPN 只会制造形态差异）", i, name)
		}
		if strings.Contains(name, "Padding") {
			t.Fatalf("第 %d 个扩展是 %s：官方没有 padding 扩展", i, name)
		}
	}
	if n := len(spec.Extensions); n != 10 {
		t.Fatalf("扩展数量 %d，官方是 10 项", n)
	}
	if n := len(spec.CipherSuites); n != 18 {
		t.Fatalf("cipher 数量 %d，官方是 18 项", n)
	}
}

// TestTLSDialerSkippedWhenProxyConfigured 有代理时必须不装自定义拨号：
// net/http 一旦看到 DialTLSContext 就会跳过代理直连目标，静默绕过用户配置的出口。
// 用可替换的判定函数覆盖两条分支（本机有系统级代理，改环境变量撤不掉）。
func TestTLSDialerSkippedWhenProxyConfigured(t *testing.T) {
	prev := proxyIsConfigured
	defer func() { proxyIsConfigured = prev }()

	proxyIsConfigured = func() bool { return true }
	if tr := newTransport(); tr.DialTLSContext != nil {
		t.Fatal("配了代理仍装自定义 DialTLSContext：net/http 会绕过代理直连")
	}

	proxyIsConfigured = func() bool { return false }
	tr := newTransport()
	if tr.DialTLSContext == nil {
		t.Fatal("无代理时未装自定义 DialTLSContext：TLS 指纹不会生效")
	}

	// 运行期切到有代理（SetDialer 会重挂拨号）：必须把已装的那份清掉。
	proxyIsConfigured = func() bool { return true }
	attachTLSDialer(tr, nil)
	if tr.DialTLSContext != nil {
		t.Fatal("切到有代理后未清空 DialTLSContext")
	}
}

// TestOfficialHelloHandshakesWithRealServer 真握手：uTLS 用的是官方 ClientHello，
// 但状态机与证书校验派生自标准库，必须证明它真的能完成握手并被 http.Transport
// 正常使用（含连接复用）。这条挡的是「指纹对了但连不上」这类全站级故障。
func TestOfficialHelloHandshakesWithRealServer(t *testing.T) {
	var protos []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protos = append(protos, r.Proto)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	prevHook, prevProxy := testTLSConfigHook, proxyIsConfigured
	testTLSConfigHook = func(host string) *utls.Config {
		return &utls.Config{ServerName: host, RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	proxyIsConfigured = func() bool { return false }
	defer func() { testTLSConfigHook, proxyIsConfigured = prevHook, prevProxy }()

	tr := newTransport()
	tr.Proxy = nil // 确保走 DialTLSContext 而不是系统代理
	client := &http.Client{Transport: tr}
	defer tr.CloseIdleConnections()

	for i := 0; i < 2; i++ {
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("第 %d 次请求失败（握手/证书校验）: %v", i+1, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("读响应体: %v", err)
		}
		if string(body) != "ok" {
			t.Fatalf("响应体=%q", body)
		}
	}
	for _, p := range protos {
		if p != "HTTP/1.1" {
			t.Fatalf("协商到了 %s：官方业务端点固定 HTTP/1.1（不发 ALPN）", p)
		}
	}
}

// TestOfficialHelloSSEStreamWorks 流式（SSE）走自定义 TLS 拨号是否正常：
// 首帧到末帧逐帧 flush，客户端必须能一行行读出来。聊天的全部流量都在这条路径上，
// 「握手能过但流读不出来」是最危险的失败形态。
func TestOfficialHelloSSEStreamWorks(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: {\"i\":%d}\n\n", i)
			w.(http.Flusher).Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	client := fingerprintTestClient(t, srv)
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	var frames []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			frames = append(frames, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("读流失败: %v", err)
	}
	if len(frames) != 4 {
		t.Fatalf("应收到 4 个 data 帧（3 内容 + DONE），实际 %d: %v", len(frames), frames)
	}
	if frames[3] != "data: [DONE]" {
		t.Fatalf("末帧应为 [DONE]，实际 %q", frames[3])
	}
}

// fingerprintTestClient 构造一个走「官方 ClientHello」且信任 srv 自签证书的客户端。
func fingerprintTestClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	prevHook, prevProxy := testTLSConfigHook, proxyIsConfigured
	testTLSConfigHook = func(host string) *utls.Config {
		return &utls.Config{ServerName: host, RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	proxyIsConfigured = func() bool { return false }
	t.Cleanup(func() { testTLSConfigHook, proxyIsConfigured = prevHook, prevProxy })

	tr := newTransport()
	tr.Proxy = nil
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// officialJA3IPTarget 一手抓包（桌面端 Electron/BoringSSL 运行时，目标为 IP）：
// 没有 SNI，所以扩展段以 23 开头、共 9 项。用来证明「同一个 ClientHello 在
// 有无 SNI 两种目标下都与官方一致」，而不是只在一种场景下凑对了。
const officialJA3IPTarget = "771,4865-4866-4867-49199-49195-49200-49196-49191-52393-52392-49161-49171-49162-49172-156-157-47-53,23-65281-10-11-35-13-51-45-43,29-23-24,0"

// TestOfficialClientHelloAgainstIPTargetMatchesCapture 对 IP 目标拨号时，我们的
// 握手必须与一手抓包逐字节相同：SNI 扩展被省略，其余 9 项顺序不变。
func TestOfficialClientHelloAgainstIPTargetMatchesCapture(t *testing.T) {
	cli, srv := net.Pipe()
	defer srv.Close()

	ja3c := make(chan string, 1)
	go func() {
		if ja3, err := readJA3(srv); err == nil {
			ja3c <- ja3
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _, _ = handshakeOfficialHello(ctx, cli, "127.0.0.1") }()

	select {
	case got := <-ja3c:
		if got != officialJA3IPTarget {
			t.Fatalf("对 IP 目标的握手与一手抓包不一致\n got=%s\nwant=%s", got, officialJA3IPTarget)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("超时：没有读到 ClientHello")
	}
}
