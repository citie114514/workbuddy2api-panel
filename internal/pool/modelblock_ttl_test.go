package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestModelBlockTTLShortEnoughToSelfHeal 11102/11158 负缓存的 TTL 必须短到能自愈，
// 同时不能短到把「真的没有这个模型」的号变成探测风暴。
//
// 回归背景：11102「service info not found」不总是「这个号真没有这个模型」——路径级差异、
// 上游瞬时抖动、分类误判都会给同一个码，社区实测同一账号两分钟就恢复。旧参数是 6h 基数 /
// 24h 上限，一次误判就把 (账号,模型) 锁死半天，几个号先后误判就是该模型整池不可用。
//
// 上限 15m / 2h 的取舍：15m 让误判自愈只比 2 分钟慢 13 分钟，而把「每次探测都白撞一轮
// 400」的稳态成本从 2m 方案的 144 次/天压到 12 次/天；2h 既跟得上模型权限变化，又不至于
// 一天只探一次。
func TestModelBlockTTLShortEnoughToSelfHeal(t *testing.T) {
	if modelBlockBaseTTL > 15*time.Minute {
		t.Errorf("modelBlockBaseTTL=%v 太长（上限 15m：误判要能在一刻钟内自愈）", modelBlockBaseTTL)
	}
	if modelBlockMaxTTL > 2*time.Hour {
		t.Errorf("modelBlockMaxTTL=%v 太长（上限 2h：上游放开权限后最多等这么久）", modelBlockMaxTTL)
	}
	if modelBlockBaseTTL < 10*time.Minute {
		t.Errorf("modelBlockBaseTTL=%v 太短（下限 10m：再短就是每小时多次白撞探测）", modelBlockBaseTTL)
	}
	if modelBlockBaseTTL >= modelBlockMaxTTL {
		t.Errorf("基数 %v 必须小于封顶 %v", modelBlockBaseTTL, modelBlockMaxTTL)
	}
	if got := modelBlockBaseTTL * (1 << modelBlockShift); got <= modelBlockMaxTTL {
		t.Errorf("阶梯末级 %v 未超过封顶 %v（shift=%d 失效）", got, modelBlockMaxTTL, modelBlockShift)
	}

	// 首次命中写 15 分钟左右的 Until，而不是 6 小时。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.BlockModelBackoff("u1", "m1", "11102 model [m1] service info not found")
	p.mu.Lock()
	until := p.byUID["u1"].modelCooldowns["m1"].Until
	p.mu.Unlock()
	if d := time.Until(until); d > modelBlockBaseTTL+time.Minute {
		t.Fatalf("首次命中 TTL=%v want ≤ %v", d, modelBlockBaseTTL)
	}
}

// TestModelCooldownsRestoreClampsStaleBackoff 存量条目钳制：升级前的 11102 条目可能带着
// 6h/24h 的 Until，不改的话「上线即收窄」要等它们自然到期（最长一天）。6004 是上游重置
// 墙钟语义，绝不能被钳。
func TestModelCooldownsRestoreClampsStaleBackoff(t *testing.T) {
	now := time.Now()
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 11102 退避条目（本次会写 ~15m，稍后篡改成旧版本的 6h）。
	p.BlockModelBackoff("u1", "m-backoff", "11102 model [m-backoff] service info not found")
	// 6004 条目：上游墙钟 6 小时后。
	p.CooldownSoftForModel("u1", time.Minute, now.Add(6*time.Hour), "m-quota", "6004 model rate limit")

	p.mu.Lock()
	sf := p.stateOverviewLocked()
	stale := sf.Accounts["u1"].ModelCooldowns["m-backoff"]
	stale.Until = now.Add(6 * time.Hour) // 模拟旧版本落盘的 6h 基数
	sf.Accounts["u1"].ModelCooldowns["m-backoff"] = stale
	p.mu.Unlock()

	p2 := New("")
	p2.Add(&auth.Auth{UID: "u1"})
	p2.mu.Lock()
	p2.applyAccountsLocked(sf.Accounts)
	restored := make(map[string]time.Time, 2)
	for m, mc := range p2.byUID["u1"].modelCooldowns {
		restored[m] = mc.Until
	}
	p2.mu.Unlock()

	if d := restored["m-backoff"].Sub(now); d > modelBlockMaxTTL+time.Minute {
		t.Fatalf("存量 11102 未被钳制：还剩 %v（上限 %v）", d, modelBlockMaxTTL)
	}
	if d := restored["m-quota"].Sub(now); d < 5*time.Hour {
		t.Fatalf("6004 墙钟被误钳：只剩 %v（应保持 ~6h）", d)
	}
}
