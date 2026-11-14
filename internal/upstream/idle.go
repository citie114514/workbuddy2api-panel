// idle.go 聊天 SSE 流中空闲监控：活跃吐数据续命不掐，静默超过阈值才断流（释放租约）。
package upstream

import (
	"context"
	"io"
	"sync"
	"time"
)

// idleMonitoringBody 包在聊天 SSE body 外层：
// 每次读到底层数据（n>0）就刷新 lastRead；后台 goroutine 周期检查，
// 静默超过 idle 就 cancel 请求 context，中断阻塞中的 Read。
//
// 除空闲监控外它还负责关闭前的**流尾回收**（drainTail）：两者都必须发生在
// body 关闭的那一刻，合成一层包装可以保证任何调用路径都不会只做一半。
type idleMonitoringBody struct {
	rc        io.ReadCloser
	mu        sync.Mutex
	lastRead  time.Time
	stopOnce  sync.Once
	closeOnce sync.Once
	closeErr  error
	stopCh    chan struct{}
	cancel    context.CancelFunc
}

func (b *idleMonitoringBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.mu.Lock()
		b.lastRead = time.Now()
		b.mu.Unlock()
	}
	return n, err
}

const (
	// 关闭前读掉流尾的预算。上游在 [DONE] 之后还会补 chunked 结束块，读不到它
	// Transport 就只能丢弃连接——每个流式请求都要重做一次 TCP+TLS 握手。
	// 4KB 是「结束块、零散心跳、少量尾帧」的量级；真在持续输出的流不会被
	// 这两个上限拖住，超预算即放弃（此时连接本来就复用不了）。
	drainMaxBytes = 4 << 10
	drainTimeout  = 100 * time.Millisecond
)

// Close 停掉后台 goroutine、**限时读掉流尾**、取消请求 context、关闭底流。
//
// 顺序不能换：先 cancel 再关闭会让在途读取立刻失败，Transport 判定 body 未读完
// 而丢弃连接（net/http 只在读到 EOF 后才把连接放回空闲池）。流式响应在
// `data: [DONE]` 处就停止读取，而结束块在它之后，所以每条流都欠一截尾巴。
//
// Close 幂等：续写层与处理函数结束时各关一次是常态，第二次不再读底流。
func (b *idleMonitoringBody) Close() error {
	b.closeOnce.Do(func() {
		// 先停空闲监控：drain 期间不能让它把连接 cancel 掉。
		b.stopOnce.Do(func() { close(b.stopCh) })
		b.drainTail()
		b.cancel()
		b.closeErr = b.rc.Close()
	})
	return b.closeErr
}

// drainTail 限时读掉流尾，让连接能回到空闲池。最多 drainMaxBytes。
//
// 超时后**不等**读 goroutine 退出：先 cancel 中断请求 context，随后的 rc.Close()
// 会唤醒任何阻塞中的 Read（net/http body 的 Close 语义），goroutine 自行结束。
// 在这里 `<-done` 反而是死锁面——cancel 未必能打断底层读（自定义 body、无 ctx 的
// 读实现），而 Close 必须是无条件前进的。
func (b *idleMonitoringBody) drainTail() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.CopyN(io.Discard, b.rc, drainMaxBytes)
	}()
	select {
	case <-done:
	case <-time.After(drainTimeout):
		b.cancel()
	}
}

func (b *idleMonitoringBody) idleFor() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Since(b.lastRead)
}

// monitorBody 包装聊天流：空闲监控（idle>0 时启用）+ 关闭前流尾回收。
// idle<=0 只关掉空闲监控那一半，**仍然保留流尾回收**——否则禁用空闲监控就等于
// 顺带放弃连接复用（两条策略本该独立）。计时从返回 body 之后开始——首字节阶段
// 由 Transport.ResponseHeaderTimeout 管，这里不抢跑。
func monitorBody(rc io.ReadCloser, idle time.Duration, cancel context.CancelFunc) io.ReadCloser {
	b := &idleMonitoringBody{
		rc:       rc,
		lastRead: time.Now(),
		stopCh:   make(chan struct{}),
		cancel:   cancel,
	}
	if idle <= 0 {
		return b
	}
	go func() {
		t := time.NewTicker(idleTick(idle))
		defer t.Stop()
		for {
			select {
			case <-b.stopCh:
				return
			case <-t.C:
				if b.idleFor() > idle {
					cancel()
					return
				}
			}
		}
	}()
	return b
}

// idleTick 返回监控周期：idle/4，钳在 [10ms, 1s]。小 idle 也能快速发现，大小值避免空转。
func idleTick(idle time.Duration) time.Duration {
	d := idle / 4
	if d > time.Second {
		d = time.Second
	}
	if d < 10*time.Millisecond {
		d = 10 * time.Millisecond
	}
	return d
}
