package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestModelCostTierFlipsBothWays 判层以「最近一次原始观测」为准，免费↔收费可双向翻转。
//
// 回归的是单号垄断的根因：EMA（CostPer1k = prev*0.7 + per1k*0.3）是正数时只无限
// 逼近 0、永不等于 0。按 EMA 判免费，一个号只要付过一次费就永久停在收费层，而收费层
// 被硬过滤 → 它拿不到新观测 → 数学上再也回不到免费层（只能等 6h TTL），该模型流量
// 于是长期压在少数号上。
func TestModelCostTierFlipsBothWays(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 首次观测即收费 → tier 2。
	p.NoteModelCost("u1", "m", 1.0, 1000)
	if got := costTierOf(t, p, "u1", "m"); got != 2 {
		t.Fatalf("付费后 tier=%d want 2", got)
	}

	// 之后观测到免费 → 必须立刻回到 tier 0（EMA 口径下永远不会）。
	p.NoteModelCost("u1", "m", 0, 1000)
	if got := costTierOf(t, p, "u1", "m"); got != 0 {
		t.Fatalf("重新观测到免费后 tier=%d want 0（EMA 残值不得参与判层）", got)
	}

	// 再回到收费 → tier 2（反向也能翻）。
	p.NoteModelCost("u1", "m", 2.0, 1000)
	if got := costTierOf(t, p, "u1", "m"); got != 2 {
		t.Fatalf("再次付费后 tier=%d want 2", got)
	}

	// 台账对外透出 last_per_1k（判层依据），EMA 只作旁证。
	rows := modelCostRows(t, p, "u1")
	if len(rows) != 1 {
		t.Fatalf("model_costs 行数=%d want 1", len(rows))
	}
	if rows[0].LastPer1k == nil || *rows[0].LastPer1k != 2.0 {
		t.Fatalf("last_per_1k=%v want 2.0", rows[0].LastPer1k)
	}
	if rows[0].CostPer1k == 0 {
		t.Fatal("EMA 残值应保留作旁证（非零）")
	}
}

// TestModelCostLegacyLedgerDropped 无 last_per_1k 的旧台账在恢复时一次性丢弃：
// 旧口径里既有「付过费就永久收费」的单向污染，也有历史记账错误留下的不可信值。
// 保留它们会让单号垄断在升级后继续存在。
func TestModelCostLegacyLedgerDropped(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")

	// 先让程序自己写出一份合法文件，再抹掉 last_per_1k 模拟旧版本台账。
	seed := New(fp)
	seed.Add(&auth.Auth{UID: "u1"})
	seed.NoteModelCost("u1", "m", 1.0, 1000)
	seed.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse state: %v", err)
	}
	accts, _ := doc["accounts"].(map[string]any)
	u1, _ := accts["u1"].(map[string]any)
	if u1 == nil {
		t.Fatalf("state.json 缺 u1 账号: %s", string(raw))
	}
	costs, _ := u1["model_costs"].(map[string]any)
	if costs == nil {
		t.Fatalf("state.json 缺 model_costs: %s", string(raw))
	}
	row, _ := costs["m"].(map[string]any)
	if row == nil {
		t.Fatalf("state.json 缺 model_costs.m: %s", string(raw))
	}
	delete(row, "last_per_1k")
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(fp, out, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	// 恢复侧应丢弃该条目 → 无观测（tier 1），由下一次真实请求重新学习。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if rows := modelCostRows(t, p2, "u1"); len(rows) != 0 {
		t.Fatalf("旧口径条目应被丢弃，实际保留 %d 行", len(rows))
	}
}

// TestModelCostLastPer1kRoundTrip last_per_1k 落盘/恢复往返无损（重启后判层不变）。
func TestModelCostLastPer1kRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "m", 0, 1000) // 免费观测
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if got := costTierOf(t, p2, "u1", "m"); got != 0 {
		t.Fatalf("恢复后 tier=%d want 0（last_per_1k 未往返）", got)
	}
}

// TestSessionDeadCountPersistsArmsDirty 12153 计数与清零都要置脏。
//
// 不置脏时：进程在 5s flusher 之前退出（或刷新成功清零后马上重启），磁盘残留旧计数，
// 重启后一次失败即达阈值——一个刚被成功 refresh 证明活着的号被误禁。
func TestSessionDeadCountPersistsArmsDirty(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.dirty.Store(false) // 忽略 Add 造成的脏标记，只看计数本身

	p.NoteSessionDead("u1")
	if !p.dirty.Load() {
		t.Fatal("12153 计数 +1 应置脏（否则重启后残留旧计数）")
	}

	p.dirty.Store(false)
	p.ClearSessionDead("u1")
	if !p.dirty.Load() {
		t.Fatal("非零计数被清零应置脏")
	}

	// 已是零值时清零不再置脏（避免每次成功都无谓写盘）。
	p.dirty.Store(false)
	p.ClearSessionDead("u1")
	if p.dirty.Load() {
		t.Fatal("零值清零不应置脏")
	}
}

// TestSessionDeadCountSurvivesRestore 计数值本身上是持久化状态：重启后从磁盘接着算。
func TestSessionDeadCountSurvivesRestore(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	// 磁盘上是 2 次，这一步就是第 3 次 → 必须禁用。计数若没往返，这里只会算第 1 次。
	if !p2.NoteSessionDead("u1") {
		t.Fatal("恢复 2 次计数后第 3 次 12153 应禁用（计数未持久化会被当成第 1 次）")
	}
	if st, _ := p2.Status("u1"); !st.Disabled {
		t.Fatalf("第 3 次 12153 后应 disabled: %+v", st)
	}
}

// costTierOf 取某 (账号, 模型) 当前的判层结果（无观测返回 1）。
func costTierOf(t *testing.T, p *Pool, uid, model string) int {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return 1
	}
	mc, ok := e.modelCostOf(model, time.Now())
	if !ok {
		return 1
	}
	return mc.tier()
}

// modelCostRows 取对外台账行（Status.ModelCosts）。
func modelCostRows(t *testing.T, p *Pool, uid string) []ModelCostStatus {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("no status for %s", uid)
	}
	return st.ModelCosts
}

// TestModelCostTTLStillApplies TTL 过期后回到「无观测」层（时段性优惠不跨时段生效）。
func TestModelCostTTLStillApplies(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "m", 0, 1000)

	p.mu.Lock()
	mc := p.byUID["u1"].modelCost["m"]
	mc.LastSeen = time.Now().Add(-modelCostTTL - time.Minute)
	p.byUID["u1"].modelCost["m"] = mc
	p.mu.Unlock()

	if got := costTierOf(t, p, "u1", "m"); got != 1 {
		t.Fatalf("过期观测 tier=%d want 1", got)
	}
}
