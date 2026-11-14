package server

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// streamKeepAlive 在流式请求「首个字节写出之前」的窗口里周期性写 SSE 注释帧。
//
// 为什么需要这一层：流式路径在拿到上游响应头之前不向客户端写任何字节——这是
// 首帧否决设计的前提（上游首帧就报错时，尚未写出的响应头还能用来静默换号）。
// 而长上下文的 TTFB 常有 1–5 秒，这段空窗里客户端一字节都收不到。
//
// 线上证据：被标记为 interrupted 的请求里，绝大多数「总耗时」与「首字节耗时」
// 几乎相等（1264/1265、1114/1115、1410/1410…），说明断开发生在首字节到达之前，
// 而不是用户主动停止——客户端等不下去了。
//
// 为什么不能在流内解决：流内保活（upstream.StreamHint）的计时起点是「上游响应头
// 已到达」，恰好越过了客户端最没耐心的那一段；它只能覆盖「已开流、首帧未到」。
//
// 代价必须写清楚：注释帧一旦写出，响应头即提交为 200，此后即便轮转全败也改不回
// 4xx/5xx，只能写流内 error 帧收尾。所以第一帧只在「已经整整等了一个间隔」之后
// 才写——快请求完全不触发，行为与改动前逐字节一致。
type streamKeepAlive struct {
	http.ResponseWriter

	mu        sync.Mutex
	committed bool // 响应头是否已提交（注释帧或真实内容都算）
	real      bool // 是否已写出真实内容（非注释帧）
	pinged    bool // 提交是不是我们自己的注释帧造成的

	quit chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

func newStreamKeepAlive(w http.ResponseWriter) *streamKeepAlive {
	return &streamKeepAlive{ResponseWriter: w, quit: make(chan struct{})}
}

func (k *streamKeepAlive) WriteHeader(code int) {
	k.mu.Lock()
	k.committed = true
	k.mu.Unlock()
	k.ResponseWriter.WriteHeader(code)
}

func (k *streamKeepAlive) Write(p []byte) (int, error) {
	// 持锁完成「置标志 + 写字节」：注释帧 goroutine 在同一把锁下检查 committed，
	// 因此它不可能插进一次真实写的中途（先提交状态、后写出内容之间的缝隙）。
	k.mu.Lock()
	defer k.mu.Unlock()
	k.committed = true
	k.real = true
	return k.ResponseWriter.Write(p)
}

func (k *streamKeepAlive) Flush() {
	if f, ok := k.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 之类可向下取到原始 writer。
func (k *streamKeepAlive) Unwrap() http.ResponseWriter { return k.ResponseWriter }

// Committed 报告响应头是否已提交。写错误响应的代码据此决定走 JSON 还是流内 error 帧。
func (k *streamKeepAlive) Committed() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.committed
}

// start 启动保活：每 every 写一个 `: ping`（SSE 注释帧，客户端不解析成内容）。
func (k *streamKeepAlive) start(every time.Duration) {
	if every <= 0 {
		every = time.Second
	}
	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-k.quit:
				return
			case <-t.C:
				if !k.ping() {
					return
				}
			}
		}
	}()
}

// ping 写一帧注释帧；返回 false 表示应当停止（已开始写真实内容，或写失败）。
func (k *streamKeepAlive) ping() bool {
	k.mu.Lock()
	if k.real {
		// 已开始写真实内容：保活交给流内，自己退出。
		k.mu.Unlock()
		return false
	}
	if k.committed && !k.pinged {
		// 响应头已被别的路径提交（例如错误响应的 WriteHeader），但还没写正文：
		// 不能再往别人的响应体里插注释帧。若提交是我们自己的注释帧造成的，
		// pinged 为真，则继续按间隔续帧。
		k.mu.Unlock()
		return false
	}
	// 与 upstream.StreamHint 开头设置的四个头保持一致：注释帧可能先于它写出，
	// 而响应头一旦提交，后来再 Set 就不再生效——必须自己补齐。
	h := k.ResponseWriter.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	_, err := io.WriteString(k.ResponseWriter, ": ping\n\n")
	k.committed = true
	k.pinged = true
	k.mu.Unlock()

	if f, ok := k.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	return err == nil
}

// stop 停止保活并等 goroutine 退出；可重复调用（defer 与交棒处都会调）。
func (k *streamKeepAlive) stop() {
	k.once.Do(func() { close(k.quit) })
	k.wg.Wait()
}
