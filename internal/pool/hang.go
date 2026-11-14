// hang.go 模型级「挂死」熔断：上游长时间不产出（表头超时 / 传输层停滞 / 流内掐流）
// 连续命中时，短暂把所有账号的该模型入口关掉，避免每个请求都白等一整个超时。
//
// 与账号级熔断的区别：账号级熔断记的是「这个号连续 5xx」，挂死往往是**模型/渠道侧**
// 的问题（同一个模型换个号也一样卡），逐号轮转只会把超时成本乘以账号数。因此按
// (域, 模型) 维度统计，命中阈值后入口直接拒绝并带 Retry-After，让客户端等待重试。
//
// 与模型级冷却（6004/11102/14003）的区别：那三种是**上游明确告知**的不可用，
// 这里是**观测到**的停滞，且按时长自动解除；成功一次即清零（避免长期误伤）。
package pool

import (
	"sort"
	"time"
)

const (
	// hangWindow 观察窗：只在最近这段时间内的停滞才累计。
	hangWindow = 5 * time.Minute
	// hangThreshold 观察窗内累计多少次停滞即熔断。
	hangThreshold = 2
	// hangBlock 熔断时长（成功一次或到期即解除）。
	hangBlock = 5 * time.Minute
)

// HangStatus 模型挂死熔断的对外观测行（面板/状态接口）。
type HangStatus struct {
	Model string `json:"model"`
	// Observations 观察窗内累计的停滞次数。
	Observations int `json:"observations"`
	// BlockedUntil 熔断截止时刻（零值 = 未熔断）。
	BlockedUntil time.Time `json:"blocked_until,omitempty"`
}

// hangKey 熔断键：同模型名可跨域（cn/global 是不同渠道），按域分开统计。
func hangKey(realm, model string) string { return realm + "\x1f" + model }

// NoteModelHang 记录一次模型停滞（表头超时/传输层停滞/流内掐流），并在观察窗内
// 累计达到阈值时置熔断。realm/model 为空时不做任何事（无维度可统计）。
func (p *Pool) NoteModelHang(realm, model string) {
	if model == "" {
		return
	}
	key := hangKey(realm, model)
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hangSeen == nil {
		p.hangSeen = make(map[string][]time.Time)
	}
	// 只保留观察窗内的记录（切片按时间升序，从头部裁剪）。
	seen := p.hangSeen[key]
	cut := 0
	for cut < len(seen) && now.Sub(seen[cut]) > hangWindow {
		cut++
	}
	if cut > 0 {
		seen = append([]time.Time(nil), seen[cut:]...)
	}
	seen = append(seen, now)
	p.hangSeen[key] = seen
	if len(seen) >= hangThreshold {
		if p.hangUntil == nil {
			p.hangUntil = make(map[string]time.Time)
		}
		p.hangUntil[key] = now.Add(hangBlock)
		// 触发后清空观察记录：下一轮要重新累计（避免刚解封就因旧记录复熔断）。
		delete(p.hangSeen, key)
		p.hangTotal.Add(1)
		p.dirty.Store(true)
	}
}

// ClearModelHang 一次成功即清零该模型的停滞观测与熔断：成功是最强的"没挂"证据。
func (p *Pool) ClearModelHang(realm, model string) {
	if model == "" {
		return
	}
	key := hangKey(realm, model)
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.hangSeen[key]; ok {
		delete(p.hangSeen, key)
	}
	if _, ok := p.hangUntil[key]; ok {
		delete(p.hangUntil, key)
		p.dirty.Store(true)
	}
}

// ModelHangBlocked 查询该 (域, 模型) 是否处于熔断期，返回截止时刻。
func (p *Pool) ModelHangBlocked(realm, model string) (time.Time, bool) {
	if model == "" {
		return time.Time{}, false
	}
	key := hangKey(realm, model)
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.hangUntil[key]
	if !ok {
		return time.Time{}, false
	}
	if !now.Before(until) {
		delete(p.hangUntil, key) // 到期：顺手清掉，避免表无限增长
		return time.Time{}, false
	}
	return until, true
}

// HangStatuses 当前仍在熔断期的 (域, 模型) 观测行，按模型名稳定排序。
func (p *Pool) HangStatuses() []HangStatus {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.hangUntil) == 0 {
		return nil
	}
	out := make([]HangStatus, 0, len(p.hangUntil))
	for key, until := range p.hangUntil {
		if !now.Before(until) {
			continue
		}
		model := key
		if i := indexSep(key); i >= 0 {
			model = key[i+1:]
		}
		out = append(out, HangStatus{Model: model, BlockedUntil: until})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// HangTotal 进程生命周期内触发过的熔断次数（观测用）。
func (p *Pool) HangTotal() int64 { return p.hangTotal.Load() }

// indexSep 返回熔断键里分隔符的位置（键用 \x1f 拼接域与模型）。
func indexSep(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1f {
			return i
		}
	}
	return -1
}
