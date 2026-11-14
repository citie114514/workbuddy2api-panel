// transport.go 出站 Transport 构造的单一事实来源（连接层加固，吸收 kongjianguan
// 4 连击实测经验的前三件，见 .claude/reports/fork-scan-absorb.md T-2）：
// 真正禁 h2 / TLS 握手超时 / 短 keepalive 探测，全参数集中定义可测试可调整。
// 第四件 DisableKeepAlives 按报告 trade-off 不吸收（每请求 TLS 握手开销与连接
// 复用意图相反），分析见 .claude/reports/transport-hardening.md。
package upstream

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 连接层参数集中定义（与 server/backoff.go 同风格：一处定义，测试可回读断言）。
const (
	// dialTimeout TCP 连接建立上限。半死连接的第一道闸：连不上就快速失败
	// 轮转换号，不再干等系统 TCP 重传窗口（kongjianguan 实测半开 TCP 单次
	// TTFB 卡 936s——默认 Dialer 无超时上限）。
	dialTimeout = 10 * time.Second
	// dialKeepAlive TCP keepalive 探测周期。默认 Dialer 2h 才发首个探测——
	// NAT 黑洞里 2h 足够连接半死且被复用。15s 周期让死连接在 15~30s 内被
	// 内核掐掉（RST/ETIMEDOUT），复用侧立即感知而非卡到重传窗口。
	dialKeepAlive = 15 * time.Second
	// tlsHandshakeTimeout TLS 握手上限。现役此前完全缺失——握手挂起时无任何
	// 层兜底（ResponseHeaderTimeout 只在请求写完后才计时），只能干等到
	// HTTP.Client.Timeout(120s)。
	tlsHandshakeTimeout = 10 * time.Second
	// idleConnTimeout 空闲连接池保留时长。从 90s 收到 30s：WAF 风暴后上游
	// NGI 常态性掐闲置连接，90s 池里的连接多半已死（kongjianguan 同款取值）；
	// 复用侧仍有 15s keepalive 兜底识别。
	idleConnTimeout = 30 * time.Second
	// responseHeaderTimeout 聊天 SSE 首字节前（响应头）硬上限。从 120s 收到
	// 60s：kongjianguan 笔记实录成功请求 TTFB 曾到 16s（慢模型冷启动），
	// 60s ≈ 3.75× 观测最坏健康首包，留足慢冷启动余量；同时把半死连接场景的
	// 单请求卡死从 2 分钟压到 1 分钟（MaxRotate 默认 3 次的最坏轮转从 6 分钟
	// 压到 3 分钟）。不取 kongjianguan 的 20s：其 20s 是 DisableKeepAlives+
	// timedConn 20s 写超时组合的取值，我们保留连接复用，须按自身慢冷启动
	// 观测留余量。语义核对（任务书设计纪律）：ResponseHeaderTimeout 只计响应
	// 头到达前的时长，头到达后 SSE 长流不受影响（流中空闲由 IdleTimeout 监控，
	// 见 idle.go），不误杀长流——transport_test.go 有显式回归。
	//
	// 注意：本常量只是 newTransport 的构造默认，main.go 会按 config
	// upstream.header_timeout_seconds 无条件覆盖。因此生产生效值 = config
	// 解析值（未配置时 normalize 回落 timeout_seconds，默认 120），本 60s 仅作
	// 「Config 未接线/测试裸用」时的安全网——与 config.example.json 的取值
	// 对齐避免三处口径漂移（transport 60 / config 回落 120 / example 60）。
	responseHeaderTimeout = 60 * time.Second
)

// maxIdleConns / maxIdleConnsPerHost 连接池容量（既有值，一并集中定义）。
const (
	maxIdleConns        = 100
	maxIdleConnsPerHost = 20
)

// newDialer 构造出站拨号器（DialContext 的 Timeout/KeepAlive 参数集中于此，
// 供测试回读断言）。
func newDialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialKeepAlive,
	}
}

// newTransport 构造共享出站 Transport（HTTP 与 ChatHTTP 同一实例，连接池不重复）。
// 分两层防半死连接：
//   - TLS 层：空 TLSNextProto 真正禁 h2（kongjianguan 二次修正的实证：ForceAttemptHTTP2=false
//     只对自定义 Dial 生效，默认 TLS 经 ALPN 仍协商出 h2，半死 h2 流复用表现为
//     "http2: timeout awaiting response headers"——唯一正确写法是置空映射，让 ALPN
//     完成后无 h2 协议可用，连接退回 HTTP/1.1）。
//   - TCP 层：DialContext 10s 建连上限 + 15s keepalive 探测，半开连接在建立期
//     和复用期都能被快速识别（见 dialTimeout/dialKeepAlive 注释）。
func newTransport() *http.Transport {
	dialer := newDialer()
	tr := &http.Transport{
		DialContext: dialer.DialContext,
		// 空 TLSNextProto（非 nil）真正禁 h2：见函数注释。必须 make 而非 nil——
		// nil 表示「让标准库注入默认 h2 映射」（kongjianguan 实测：设
		// ForceAttemptHTTP2=false 后日志仍报 h2 timeout，正是这个陷阱）。
		TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		IdleConnTimeout:       idleConnTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		// 自动读取环境变量中的代理配置
		Proxy: http.ProxyFromEnvironment,
	}
	// TLS 层与官方客户端同形（见 tlsfingerprint.go）：有代理时不装——
	// 自定义 DialTLSContext 会让 net/http 跳过代理直连目标。
	attachTLSDialer(tr, dialer)
	return tr
}

// closeIdler 实现该接口的 RoundTripper 支持清空空闲连接池（*http.Transport、
// http2.Transport 等均满足；测试注入的自定义 RoundTripper 可选择性实现）。
type closeIdler interface {
	CloseIdleConnections()
}

// roundTripCloseIdle 在传输层请求失败后清掉 rt 所属 Transport 的空闲连接池
// （kongjianguan 第 4 件：失败连接可能仍留在空闲池里，等 IdleConnTimeout 才
// 过期，下一个请求会继续捡到它）。
//
// 挂载点（任务书「评估挂载点：错误分类处理处」的结论）：错误分类（Classify）
// 只见业务信封——传输层失败根本没有 body 可分类（见 doJSON/ChatStreamContext
// 对 read body 失败的处理：不进 Classify、不罚号）。这类失败的正确处理正是
// 连接层的池清理，故挂在与 Do 并列的传输层出口（ChatStreamContext 的 Do 错误
// 分支），而非 applyErrorPolicy。
//
// 关闭是 best-effort：rt 为 nil 或未实现 closeIdler（如测试注入的 rtFunc）时
// 静默跳过。CloseIdleConnections 只关空闲连接，不影响在途请求；瞬时代价是
// 下个请求多一次 TCP+TLS 握手，与半死连接被复用卡 60s 的风险完全不成比例。
func roundTripCloseIdle(rt http.RoundTripper) {
	if rt == nil {
		return
	}
	if ci, ok := rt.(closeIdler); ok {
		ci.CloseIdleConnections()
	}
}

// uidContextKey 请求上下文里的账号 UID（连接池分桶键）。
type uidContextKey struct{}

// attachAccountUID 把账号 UID 写进请求上下文（不可见，不影响任何出站字节）。
//
// 用上下文而不是新增请求头：头是发往上游的字节，多一个少一个都会改变指纹；
// 上下文只在进程内。调用点放在头构造里（CommonHeaders / BillingHeaders），
// 覆盖所有出站路径且不需要每个业务函数各改一遍。
func attachAccountUID(req *http.Request, a *auth.Auth) {
	if req == nil || a == nil || a.UID == "" {
		return
	}
	// WithContext 返回浅拷贝；写回原指针让调用方后续设置的头仍然生效。
	*req = *req.WithContext(context.WithValue(req.Context(), uidContextKey{}, a.UID))
}

// requestAccountUID 取请求的账号 UID：优先上下文，其次 X-User-Id 头
// （少数路径只设头没走 CommonHeaders 时兜底）。
func requestAccountUID(req *http.Request) string {
	if req == nil {
		return ""
	}
	if v, ok := req.Context().Value(uidContextKey{}).(string); ok && v != "" {
		return v
	}
	return req.Header.Get("X-User-Id")
}

// maxUIDTransports 分桶上限。账号数是有限的（当前 9 个），上限只为防御
// 异常输入（例如历史遗留的 per-session UID）导致 map 无界增长。
const maxUIDTransports = 128

// uidTransport 按账号分桶的连接池。
//
// 为什么必须分桶：net/http 的空闲连接按 (scheme, host) 复用，**不区分账号**。
// 共用一个 *http.Transport 时，账号 A 的请求结束后，账号 B 的请求会捡起同一条
// TCP/TLS 连接——上游按连接聚合就能看到「一条连接、多个 Authorization」。真实
// 桌面客户端是每人一台机器，一条连接只有一个身份，这是最容易被识别为网关的
// 结构性特征，而且与 TLS 指纹（JA3/JA4）无关，改头也不管用。
//
// 未绑定账号的请求（如匿名探测）走 "" 桶，行为与改造前完全一致。
type uidTransport struct {
	mu   sync.Mutex
	m    map[string]*http.Transport
	dial *net.Dialer
	// respHeaderTimeout 非 0 时覆盖新建 Transport 的 ResponseHeaderTimeout
	// （聊天首字节上限，由 main 从配置注入）。
	respHeaderTimeout time.Duration
}

// newUIDTransport 构造默认分桶传输层（桶内 Transport 由 newTransport 生成）。
func newUIDTransport() *uidTransport {
	return &uidTransport{m: make(map[string]*http.Transport)}
}

// forUID 取（或惰性创建）某个账号的 Transport。
func (t *uidTransport) forUID(uid string) *http.Transport {
	t.mu.Lock()
	defer t.mu.Unlock()
	if tr, ok := t.m[uid]; ok {
		return tr
	}
	if len(t.m) >= maxUIDTransports {
		t.evictOneLocked()
	}
	tr := newTransport()
	if t.dial != nil {
		tr.DialContext = t.dial.DialContext
		// TLS 拨号必须跟同一个 dialer 走，否则超时/keepalive 两套口径。
		attachTLSDialer(tr, t.dial)
	}
	if t.respHeaderTimeout > 0 {
		tr.ResponseHeaderTimeout = t.respHeaderTimeout
	}
	t.m[uid] = tr
	return tr
}

// evictOneLocked 淘汰一个空桶（优先非 "" 桶）。调用方须持锁。
// 只关闭空闲连接，不打断在途请求。
func (t *uidTransport) evictOneLocked() {
	for k, tr := range t.m {
		if k == "" {
			continue
		}
		tr.CloseIdleConnections()
		delete(t.m, k)
		return
	}
}

// RoundTrip 按请求所属账号选择连接池。
func (t *uidTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Accept-Encoding 统一在这里注入：所有出站上游请求（对话、签到、任务、上报、
	// 模型目录、搜索）都过本层，一处设定即全覆盖，不存在漏网的请求形态。
	// 调用方显式设过值时不覆盖（目前没有这样的调用方，留作扩展口）。
	// 设了这个头之后标准库不再自动解压，所以下面必须自己解（见 encoding.go）。
	if req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", acceptEncodingAxios)
	}
	resp, err := t.forUID(requestAccountUID(req)).RoundTrip(req)
	if err != nil {
		return nil, err
	}
	body, derr := decodeResponseBody(resp)
	if derr != nil {
		resp.Body.Close()
		return nil, derr
	}
	resp.Body = body
	return resp, nil
}

// CloseIdleConnections 关闭所有桶的空闲连接（失败清池路径使用）。
func (t *uidTransport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, tr := range t.m {
		tr.CloseIdleConnections()
	}
}

// SetDialer 注入拨号器：作用于已有桶与后续新建的桶。
func (t *uidTransport) SetDialer(d *net.Dialer) {
	if d == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dial = d
	for _, tr := range t.m {
		tr.DialContext = d.DialContext
		attachTLSDialer(tr, d)
	}
}

// SetResponseHeaderTimeout 设置聊天首字节（响应头）上限：作用于已有桶与后续新建的桶。
func (t *uidTransport) SetResponseHeaderTimeout(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.respHeaderTimeout = d
	for _, tr := range t.m {
		tr.ResponseHeaderTimeout = d
	}
}
