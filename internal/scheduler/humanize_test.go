package scheduler

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestJitteredIntervalOffIsIdentity 缺省（permill=0）时行为必须与接入前逐字一致：
// 同样的基准间隔原样返回，不加任何随机。
func TestJitteredIntervalOffIsIdentity(t *testing.T) {
	base := 5 * time.Minute
	calls := 0
	rnd := func(n int64) int64 { calls++; return 0 }
	if got := jitteredInterval(base, 0, rnd); got != base {
		t.Fatalf("permill=0 → %v want %v（不得抖动）", got, base)
	}
	if calls != 0 {
		t.Fatalf("permill=0 不应消费随机源，calls=%d", calls)
	}
	// 负值同样按关闭处理。
	if got := jitteredInterval(base, -10, rnd); got != base {
		t.Fatalf("负 permill → %v want %v", got, base)
	}
}

// TestJitteredIntervalBounds 抖动幅度落在 ±(permill/1000)×基准 内，且不为零。
func TestJitteredIntervalBounds(t *testing.T) {
	base := 10 * time.Minute
	permill := 200 // ±20% → ±2 分钟
	cases := []struct {
		rnd  int64
		want time.Duration
	}{
		{0, base - 2*time.Minute},                              // 下界（rand 返回 0）
		{int64(2 * time.Minute), base},                         // 中点（delta=0）
		{2*int64(2*time.Minute) - 1, base + 2*time.Minute - 1}, // 上界附近
	}
	for _, c := range cases {
		got := jitteredInterval(base, permill, func(int64) int64 { return c.rnd })
		if got != c.want {
			t.Errorf("rnd=%d → %v want %v", c.rnd, got, c.want)
		}
	}
}

// TestJitteredIntervalClampAndFloor 幅度上限 500（±50%），且结果不小于 1 秒。
func TestJitteredIntervalClampAndFloor(t *testing.T) {
	// 超上限按 500 处理：base=10s → 区间 [5s, 15s)。
	base := 10 * time.Second
	got := jitteredInterval(base, 100000, func(n int64) int64 { return 0 })
	if got != 5*time.Second {
		t.Fatalf("幅度应钳到 500 → %v want 5s", got)
	}
	// 极小基准 + 大抖动也不得压到 1 秒以下（避免忙等）。
	if got := jitteredInterval(500*time.Millisecond, 500, func(n int64) int64 { return 0 }); got < time.Second {
		t.Fatalf("结果不得低于 1s，got %v", got)
	}
}

// TestJitterSpreadsAcrossRounds 连续多轮取到的间隔应分散（不是每轮同一个值）。
func TestJitterSpreadsAcrossRounds(t *testing.T) {
	base := 5 * time.Minute
	seen := map[time.Duration]bool{}
	for i := 0; i < 20; i++ {
		seen[jitteredInterval(base, 300, scheduleRand)] = true
	}
	if len(seen) < 5 {
		t.Fatalf("20 轮只出现 %d 种间隔，抖动没有生效", len(seen))
	}
}

// TestCapByCreditsTopN 夜猫子参与上限：按积分取前 N，且不改动入参顺序。
func TestCapByCreditsTopN(t *testing.T) {
	list := []pool.Status{
		{UID: "a", Credits: 10},
		{UID: "b", Credits: 90},
		{UID: "c", Credits: 50},
		{UID: "d", Credits: 70},
	}
	got := capByCredits(list, 2)
	if len(got) != 2 || got[0].UID != "b" || got[1].UID != "d" {
		t.Fatalf("top2=%v want [b d]", uids(got))
	}
	// 入参顺序不变（调用方可能还持有同一切片）。
	if list[0].UID != "a" || list[1].UID != "b" || list[2].UID != "c" || list[3].UID != "d" {
		t.Fatalf("入参顺序被改写: %v", uids(list))
	}
	// 0 或超过池容量 → 原样返回（不限）。
	if got := capByCredits(list, 0); len(got) != 4 {
		t.Fatalf("cap=0 应不限，got %d", len(got))
	}
	if got := capByCredits(list, 9); len(got) != 4 {
		t.Fatalf("cap>池容量 应不限，got %d", len(got))
	}
}

// TestCapByCreditsStableOnTie 同分保持原有顺序（否则同一批账号每次被重排成不同集合）。
func TestCapByCreditsStableOnTie(t *testing.T) {
	list := []pool.Status{
		{UID: "x", Credits: 50},
		{UID: "y", Credits: 50},
		{UID: "z", Credits: 50},
	}
	first := uids(capByCredits(list, 2))
	for i := 0; i < 5; i++ {
		if again := uids(capByCredits(list, 2)); again[0] != first[0] || again[1] != first[1] {
			t.Fatalf("同分结果不稳定: %v vs %v", first, again)
		}
	}
	if first[0] != "x" || first[1] != "y" {
		t.Fatalf("同分应保持原顺序，got %v", first)
	}
}

// TestSetJitterClamps 热更新同样钳到 [0,500]；SetNightAccountCap 负值按 0。
func TestSetJitterClamps(t *testing.T) {
	s := New(Config{})
	s.SetJitter(-5, 900)
	if got := s.balanceJitter.Load(); got != 0 {
		t.Fatalf("balanceJitter=%d want 0", got)
	}
	if got := s.sweepJitter.Load(); got != int32(jitterPermillMax) {
		t.Fatalf("sweepJitter=%d want %d", got, jitterPermillMax)
	}
	s.SetNightAccountCap(-3)
	if got := s.NightAccountCap(); got != 0 {
		t.Fatalf("nightCap=%d want 0", got)
	}
}

func uids(list []pool.Status) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.UID)
	}
	return out
}
