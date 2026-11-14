// 选号负载均衡测试：窗口内被选次数降权、过期清理、容量封顶，以及确定性随机源下
// 连续选号向少用账号摊开。粘性会话不经过 pick，因此不在本组测试范围。
package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestFairnessPenaltyWindowAndCap(t *testing.T) {
	p := New("")
	now := time.Now()
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.byUID["u1"]

	if got := loadFairnessLocked(e, now); got != 1 {
		t.Fatalf("never picked should be x1, got %v", got)
	}
	for i := 0; i < 10; i++ {
		notePickLocked(e, now)
	}
	if got, want := loadFairnessLocked(e, now), 1.0/(1.0+fairnessDecay*10); got != want {
		t.Fatalf("10 picks factor = %v, want %v", got, want)
	}
	if got := loadFairnessLocked(e, now.Add(fairnessWindow+time.Second)); got != 1 {
		t.Fatalf("expired window should restore x1, got %v", got)
	}
	if n := len(e.pickStamps); n != 0 {
		t.Fatalf("expired stamps should be pruned, got %d", n)
	}
	for i := 0; i < fairnessCap+10; i++ {
		notePickLocked(e, now)
	}
	if n := len(e.pickStamps); n != fairnessCap {
		t.Fatalf("stamp ring should cap at %d, got %d", fairnessCap, n)
	}
}

func TestFairnessSpreadsSequentialPicks(t *testing.T) {
	withNoPickGap(t)

	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	for _, u := range []string{"acct-a", "acct-b", "acct-c"} {
		p.Add(&auth.Auth{UID: u})
		p.SetCredits(u, 100, 0)
	}

	const picks = 24
	counts := map[string]int{}
	for i := 0; i < picks; i++ {
		a := p.Pick()
		if a == nil {
			t.Fatalf("pick %d returned nil", i)
		}
		counts[a.UID]++
	}
	lo, hi := picks, 0
	for _, c := range counts {
		if c < lo {
			lo = c
		}
		if c > hi {
			hi = c
		}
	}
	if hi-lo > 1 {
		t.Fatalf("fairness should spread %d picks evenly, counts=%v", picks, counts)
	}
}

func TestFairnessDoesNotBreakExpiryPreference(t *testing.T) {
	withNoPickGap(t)

	p := New("")
	p.SetPreferExpiring(true)
	p.Add(&auth.Auth{UID: "expiring"})
	p.Add(&auth.Auth{UID: "normal"})
	p.SetCredits("expiring", 100, 0)
	p.SetCredits("normal", 100, 0)

	now := time.Now()
	p.mu.Lock()
	p.byUID["expiring"].creditsExpiring = 100
	p.byUID["expiring"].creditsEarliestRemaining = 100
	p.byUID["expiring"].creditsEarliestExpiry = now.Add(24 * time.Hour)
	p.mu.Unlock()

	p.mu.Lock()
	rank := p.expiringRankLocked(now, p.expiringWindowLocked(now, ""), "")
	p.mu.Unlock()

	got := p.routingWeightOf(p.byUID["expiring"], 100, now, rank["expiring"])
	base := p.weightOf(p.byUID["expiring"], 100, now)
	if got <= base {
		t.Fatalf("expiry preference must remain a multiplier on load fairness: weight=%v base=%v", got, base)
	}
}
