package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// snapshotWithExpiry 给账号写入余额与一个到期批次（batchAt 之后全归零）。
func snapshotWithExpiry(p *Pool, uid string, credits, batchRemain int64, batchAt time.Time) {
	p.SetCreditsSnapshot(uid, CreditsSnapshot{
		Remain:            credits,
		Total:             credits,
		Expiring:          batchRemain,
		EarliestAt:        batchAt,
		EarliestRemaining: batchRemain,
		ExpiryBuckets: []ExpiryBucket{{
			Date:     batchAt.Format("2006-01-02"),
			ExpireAt: batchAt,
			Remain:   batchRemain,
		}},
	})
}

// TestExpiringLadderValues 钉住阶梯数值本身：第 1..5 名 8/6/5/4/3，第 6 名及以后 3
// （与改造前的统一 ×3 一致，即「排名靠后的号不被降权」）。
func TestExpiringLadderValues(t *testing.T) {
	want := []float64{8, 6, 5, 4, 3}
	if len(expiringLadder) != len(want) {
		t.Fatalf("expiringLadder 长度=%d want %d", len(expiringLadder), len(want))
	}
	for i, w := range want {
		if expiringLadder[i] != w {
			t.Errorf("第 %d 名权重=%v want %v", i+1, expiringLadder[i], w)
		}
	}
	if expiringTailSlots != 3 {
		t.Errorf("第 6 名及以后权重=%v want 3（与改造前一致，不降权）", expiringTailSlots)
	}
	// 名次 0 表示不在窗口内 → 不加权。
	if got := expiringSlots(0); got != 1 {
		t.Errorf("expiringSlots(0)=%v want 1", got)
	}
	if got := expiringSlots(6); got != expiringTailSlots {
		t.Errorf("expiringSlots(6)=%v want %v", got, expiringTailSlots)
	}
}

// TestExpiringRankByUrgency 排名按「最早到期升序 → 该批剩余降序 → uid」，且权重倍数
// 严格递减；第 6 名落到 tail（3）。
//
// 这是改造的核心：以前窗口内一律 ×3，绝对权重仍由余额决定，号一多最急的号反而进不了
// top5 短名单。阶梯让「谁最急」直接决定谁拿最多流量。
func TestExpiringRankByUrgency(t *testing.T) {
	p := New("")
	now := time.Now()
	uids := []string{"u1", "u2", "u3", "u4", "u5", "u6"}
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid})
	}
	// 余额全相同（隔离余额项），只让到期时间不同：u1 最急。
	for i, uid := range uids {
		snapshotWithExpiry(p, uid, 1000, 100, now.Add(time.Duration(i+1)*24*time.Hour))
	}

	p.mu.Lock()
	win := p.expiringWindowLocked(now, "")
	rank := p.expiringRankLocked(now, win, "")
	p.mu.Unlock()

	if win != 7*24*time.Hour {
		t.Fatalf("主窗口内有批次时应用 7 天窗口，got %v", win)
	}
	wantRank := map[string]int{"u1": 1, "u2": 2, "u3": 3, "u4": 4, "u5": 5, "u6": 6}
	for uid, want := range wantRank {
		if rank[uid] != want {
			t.Errorf("%s 名次=%d want %d", uid, rank[uid], want)
		}
	}

	wantMult := map[string]float64{"u1": 8, "u2": 6, "u3": 5, "u4": 4, "u5": 3, "u6": 3}
	for uid, want := range wantMult {
		p.mu.Lock()
		e := p.byUID[uid]
		got := p.routingWeightOf(e, 1000, now, rank[uid])
		base := p.weightOf(e, 1000, now)
		p.mu.Unlock()
		if diff := got - base*want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s 权重=%v want %v（base=%v ×%v）", uid, got, base*want, base, want)
		}
	}
}

// TestExpiringTieBreaksByRemainingThenUID 同一到期时刻按该批剩余量降序（浪费得多先花），
// 剩余量也相同时用 uid 兜底，保证多进程/多次调用排名一致。
func TestExpiringTieBreaksByRemainingThenUID(t *testing.T) {
	p := New("")
	now := time.Now()
	at := now.Add(24 * time.Hour)
	for _, uid := range []string{"a-small", "b-large", "c-small"} {
		p.Add(&auth.Auth{UID: uid})
	}
	snapshotWithExpiry(p, "a-small", 1000, 10, at)
	snapshotWithExpiry(p, "b-large", 1000, 900, at)
	snapshotWithExpiry(p, "c-small", 1000, 10, at)

	p.mu.Lock()
	rank := p.expiringRankLocked(now, p.expiringWindowLocked(now, ""), "")
	p.mu.Unlock()

	if rank["b-large"] != 1 {
		t.Errorf("剩余量大的应排第一，got b-large=%d", rank["b-large"])
	}
	if rank["a-small"] != 2 || rank["c-small"] != 3 {
		t.Errorf("剩余量相同时应按 uid 升序, got a=%d c=%d", rank["a-small"], rank["c-small"])
	}
}

// TestExpiringWindowFallsBackTo14d 主窗口（7 天）内全池都没有到期积分时放宽到 14 天；
// 一旦有账号进入主窗口，立刻切回 7 天（只给真正更急的那批加权）。
func TestExpiringWindowFallsBackTo14d(t *testing.T) {
	p := New("")
	now := time.Now()
	p.Add(&auth.Auth{UID: "far1"})
	p.Add(&auth.Auth{UID: "far2"})
	// 两个号都在 10 天后到期：主窗口空 → 回退 14 天。
	snapshotWithExpiry(p, "far1", 1000, 300, now.Add(10*24*time.Hour))
	snapshotWithExpiry(p, "far2", 1000, 200, now.Add(11*24*time.Hour))

	p.mu.Lock()
	win := p.expiringWindowLocked(now, "")
	rank := p.expiringRankLocked(now, win, "")
	p.mu.Unlock()

	if win != 14*24*time.Hour {
		t.Fatalf("主窗口无积分时应回退 14 天，got %v", win)
	}
	if len(rank) != 2 || rank["far1"] != 1 || rank["far2"] != 2 {
		t.Fatalf("回退窗口内应按到期先后排名, got %v", rank)
	}

	// 加入一个 3 天后到期的号：主窗口非空 → 切回 7 天，10 天外的号不再参与排名。
	p.Add(&auth.Auth{UID: "near"})
	snapshotWithExpiry(p, "near", 1000, 50, now.Add(3*24*time.Hour))

	p.mu.Lock()
	win = p.expiringWindowLocked(now, "")
	rank = p.expiringRankLocked(now, win, "")
	p.mu.Unlock()

	if win != 7*24*time.Hour {
		t.Fatalf("主窗口有积分时应回到 7 天，got %v", win)
	}
	if len(rank) != 1 || rank["near"] != 1 {
		t.Fatalf("7 天窗口内只应剩 near, got %v", rank)
	}
}

// TestExpiringWindowFallbackIsPerRealm 回退判定按域独立：cn 域在主窗口内有积分时，
// 不得把 global 域也锁在 7 天窗口里（两域候选集互不相干）。
func TestExpiringWindowFallbackIsPerRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	now := time.Now()
	p.Add(&auth.Auth{UID: "cn-soon", Domain: "www.codebuddy.cn"}) // Realm()=="cn"
	p.Add(&auth.Auth{UID: "g-later", Domain: "www.workbuddy.ai"}) // Realm()=="global"
	snapshotWithExpiry(p, "cn-soon", 1000, 100, now.Add(2*24*time.Hour))
	snapshotWithExpiry(p, "g-later", 1000, 100, now.Add(10*24*time.Hour))

	p.mu.Lock()
	cnWin := p.expiringWindowLocked(now, "cn")
	gWin := p.expiringWindowLocked(now, "global")
	p.mu.Unlock()

	if cnWin != 7*24*time.Hour {
		t.Errorf("cn 域名次窗口=%v want 7 天（该域主窗口内已有积分）", cnWin)
	}
	if gWin != 14*24*time.Hour {
		t.Errorf("global 域名次窗口=%v want 14 天（该域主窗口内为空）", gWin)
	}
}

// TestPreferExpiringOffIgnoresWindow prefer_expiring=false 时窗口与排名都不生效，
// 权重退化为纯 weightOf（零回归）。
func TestPreferExpiringOffIgnoresWindow(t *testing.T) {
	p := New("")
	now := time.Now()
	p.Add(&auth.Auth{UID: "soon"})
	snapshotWithExpiry(p, "soon", 1000, 500, now.Add(time.Hour))
	p.SetPreferExpiring(false)

	p.mu.Lock()
	rank := p.expiringRankLocked(now, p.expiringWindowLocked(now, ""), "")
	e := p.byUID["soon"]
	got := p.routingWeightOf(e, 1000, now, rank["soon"])
	base := p.weightOf(e, 1000, now)
	p.mu.Unlock()

	if got != base {
		t.Fatalf("关闭快过期偏好后权重必须等于 weightOf: got %v base %v", got, base)
	}
}
