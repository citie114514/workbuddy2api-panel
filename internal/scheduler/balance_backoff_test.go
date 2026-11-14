package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type balanceBackoffFake struct {
	hits     atomic.Int32
	fail     atomic.Bool
	body     string
	requests chan time.Time
}

func newBalanceBackoffScheduler(t *testing.T, fake *balanceBackoffFake, uids ...string) (*Scheduler, *pool.Pool) {
	t.Helper()
	if fake.body == "" {
		fake.body = `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":555,"CycleCapacityUsed":0}]}}}}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.hits.Add(1)
		if fake.requests != nil {
			fake.requests <- time.Now()
		}
		if fake.fail.Load() {
			// 快速业务失败，避免 upstream 对瞬时错误执行 2s/4s 重试。
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("boom"))
			return
		}
		_, _ = w.Write([]byte(fake.body))
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at-" + uid, RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up}), p
}

func balanceStateForTest(s *Scheduler, uid string) balanceFailState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.balanceState[uid]
}

func TestBalanceStaggerSpreadAndStableOffset(t *testing.T) {
	if got := balanceStaggerSpread(0); got != 0 {
		t.Fatalf("interval=0 spread=%v want 0", got)
	}
	if got := balanceStaggerSpread(90 * time.Second); got != 30*time.Second {
		t.Fatalf("interval=90s spread=%v want 30s", got)
	}
	if got := balanceStaggerSpread(5 * time.Minute); got != 90*time.Second {
		t.Fatalf("interval=5m spread=%v want capped 90s", got)
	}
	for _, uid := range []string{"u1", "u2", "稳定账号", ""} {
		for _, spread := range []time.Duration{time.Nanosecond, time.Millisecond, 90 * time.Second} {
			first := balanceOffset(uid, spread)
			if first < 0 || first >= spread {
				t.Fatalf("uid=%q spread=%v offset=%v 超出 [0,spread)", uid, spread, first)
			}
			if again := balanceOffset(uid, spread); again != first {
				t.Fatalf("uid=%q 偏移不稳定：%v != %v", uid, again, first)
			}
		}
	}
	if got := balanceOffset("u1", 0); got != 0 {
		t.Fatalf("spread=0 offset=%v want 0", got)
	}
}

func TestBalanceBackoffSkipsExactlyOneThenTwoRounds(t *testing.T) {
	fake := &balanceBackoffFake{}
	fake.fail.Store(true)
	s, _ := newBalanceBackoffScheduler(t, fake, "u1")
	ctx := context.Background()

	for round := 1; round <= 3; round++ {
		s.RunBalanceRefreshStaggered(ctx, time.Nanosecond)
		if got := fake.hits.Load(); got != int32(round) {
			t.Fatalf("第 %d 次实际失败 hits=%d want %d", round, got, round)
		}
		want := balanceFailState{consecutive: round}
		if round == 3 {
			want.skipRounds = 1
		}
		if got := balanceStateForTest(s, "u1"); got != want {
			t.Fatalf("第 %d 次失败后 state=%+v want %+v", round, got, want)
		}
	}

	s.RunBalanceRefreshStaggered(ctx, time.Nanosecond)
	if got := fake.hits.Load(); got != 3 {
		t.Fatalf("第 3 次失败后应准确跳 1 轮，hits=%d want 3", got)
	}
	if got := balanceStateForTest(s, "u1"); got != (balanceFailState{consecutive: 3}) {
		t.Fatalf("跳过轮改变了连败或延长退避：%+v", got)
	}

	s.RunBalanceRefreshStaggered(ctx, time.Nanosecond)
	if got := fake.hits.Load(); got != 4 {
		t.Fatalf("跳 1 轮后应执行第 4 次实际请求，hits=%d want 4", got)
	}
	if got := balanceStateForTest(s, "u1"); got != (balanceFailState{consecutive: 4, skipRounds: 2}) {
		t.Fatalf("第 4 次失败后 state=%+v want consecutive=4 skipRounds=2", got)
	}
	for skipped := 1; skipped <= 2; skipped++ {
		s.RunBalanceRefreshStaggered(ctx, time.Nanosecond)
		if got := fake.hits.Load(); got != 4 {
			t.Fatalf("第 4 次失败后第 %d 个跳过轮发起了请求，hits=%d", skipped, got)
		}
		want := balanceFailState{consecutive: 4, skipRounds: 2 - skipped}
		if got := balanceStateForTest(s, "u1"); got != want {
			t.Fatalf("第 %d 个跳过轮 state=%+v want %+v", skipped, got, want)
		}
	}

	s.RunBalanceRefreshStaggered(ctx, time.Nanosecond)
	if got := fake.hits.Load(); got != 5 {
		t.Fatalf("跳 2 轮后应恢复实际请求，hits=%d want 5", got)
	}
	if got := balanceStateForTest(s, "u1"); got != (balanceFailState{consecutive: 5, skipRounds: 2}) {
		t.Fatalf("第 5 次及以后应最多跳 2 轮，state=%+v", got)
	}
}

func TestBalanceBackoffSuccessClearsState(t *testing.T) {
	fake := &balanceBackoffFake{}
	fake.fail.Store(true)
	s, p := newBalanceBackoffScheduler(t, fake, "u1")
	for i := 0; i < 3; i++ {
		s.RunBalanceRefreshStaggered(context.Background(), time.Nanosecond)
	}
	s.RunBalanceRefreshStaggered(context.Background(), time.Nanosecond) // 跳过 1 轮

	fake.fail.Store(false)
	s.RunBalanceRefreshStaggered(context.Background(), time.Nanosecond)
	if got := balanceStateForTest(s, "u1"); got != (balanceFailState{}) {
		t.Fatalf("成功一次应清零连败与退避，state=%+v", got)
	}
	if st, _ := p.Status("u1"); st.Credits != 555 {
		t.Fatalf("成功后 credits=%d want 555", st.Credits)
	}

	fake.fail.Store(true)
	for i := 1; i <= 2; i++ {
		s.RunBalanceRefreshStaggered(context.Background(), time.Nanosecond)
		if got := balanceStateForTest(s, "u1"); got != (balanceFailState{consecutive: i}) {
			t.Fatalf("清零后第 %d 次失败 state=%+v", i, got)
		}
	}
}

func TestManualBalanceRefreshNeverSkipsOrConsumesRounds(t *testing.T) {
	fake := &balanceBackoffFake{}
	fake.fail.Store(true)
	s, _ := newBalanceBackoffScheduler(t, fake, "u1")
	for i := 0; i < 3; i++ {
		s.RunBalanceRefreshStaggered(context.Background(), time.Nanosecond)
	}
	s.RunBalanceRefreshStaggered(context.Background(), time.Nanosecond) // 消费第 3 次后的 1 轮
	s.RunBalanceRefreshStaggered(context.Background(), time.Nanosecond) // 第 4 次失败，待跳 2 轮

	for i := 1; i <= 2; i++ {
		before := fake.hits.Load()
		s.RunBalanceRefreshNow()
		if got := fake.hits.Load(); got != before+1 {
			t.Fatalf("手动刷新 %d 被退避跳过，hits %d→%d", i, before, got)
		}
		if got := balanceStateForTest(s, "u1"); got.skipRounds != 2 {
			t.Fatalf("手动刷新 %d 消费了 skipRounds：%+v", i, got)
		}
	}
}

func TestStaggeredBalanceSuccessWritesCompleteSnapshot(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	first := time.Now().In(loc).Add(24 * time.Hour).Format("2006-01-02 15:04:05")
	second := time.Now().In(loc).Add(48 * time.Hour).Format("2006-01-02 15:04:05")
	fake := &balanceBackoffFake{body: fmt.Sprintf(`{"code":0,"data":{"Response":{"Data":{"Accounts":[`+
		`{"PackageName":"first","CycleCapacitySize":100,"CycleCapacityRemain":60,"CycleCapacityUsed":40,"CycleEndTime":%q},`+
		`{"PackageName":"second","CycleCapacitySize":200,"CycleCapacityRemain":40,"CycleCapacityUsed":160,"CycleEndTime":%q}`+
		`]}}}}`, first, second)}
	s, p := newBalanceBackoffScheduler(t, fake, "u1")
	s.schedMu.Lock()
	s.cfg.ExpiringSoonWindow = 72 * time.Hour
	s.schedMu.Unlock()

	s.RunBalanceRefreshStaggered(context.Background(), 3*time.Millisecond)
	st, _ := p.Status("u1")
	if st.Credits != 100 || st.CreditsTotal != 300 || st.CreditsExpiring != 100 {
		t.Fatalf("完整快照总额不符：%+v", st)
	}
	if st.CreditsEarliestExpiry.IsZero() || st.CreditsEarliestRemaining != 60 {
		t.Fatalf("最早到期快照不符：%+v", st)
	}
	if len(st.CreditsExpiryBuckets) != 2 || st.CreditsExpiryBuckets[0].Remain != 60 || st.CreditsExpiryBuckets[1].Remain != 40 {
		t.Fatalf("到期分桶不符：%+v", st.CreditsExpiryBuckets)
	}
	if s.BalanceRefreshedAt().IsZero() {
		t.Fatal("后台错峰成功后未更新 lastBalanceRefresh")
	}
}

func TestStartBalanceRefreshUsesBackoffOnStartupAndTicker(t *testing.T) {
	fake := &balanceBackoffFake{requests: make(chan time.Time, 4)}
	s, _ := newBalanceBackoffScheduler(t, fake, "u1")
	s.mu.Lock()
	s.balanceState["u1"] = balanceFailState{consecutive: 3, skipRounds: 2}
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const interval = 40 * time.Millisecond
	started := time.Now()
	s.StartBalanceRefresh(ctx, interval)

	// 启动首轮和第一个定时轮各消费一次退避，都不能直接走手动全量入口。
	select {
	case at := <-fake.requests:
		t.Fatalf("前两轮应退避，却在 %v 后发起请求", at.Sub(started))
	case <-time.After(55 * time.Millisecond):
	}
	select {
	case at := <-fake.requests:
		if elapsed := at.Sub(started); elapsed < 70*time.Millisecond {
			t.Fatalf("第二个定时轮才应恢复请求，实际仅过 %v", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("第二个定时轮后仍未恢复余额请求")
	}
}

func TestStaggeredBalanceContextCancelInterruptsWait(t *testing.T) {
	fake := &balanceBackoffFake{}
	uid := "u1"
	for balanceOffset(uid, 30*time.Second) < time.Second {
		uid += "x"
	}
	s, _ := newBalanceBackoffScheduler(t, fake, uid)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.RunBalanceRefreshStaggered(ctx, 90*time.Second)
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("context 取消未打断错峰等待")
	}
	if got := fake.hits.Load(); got != 0 {
		t.Fatalf("context 取消后仍发起错峰等待中的账号请求，hits=%d want 0", got)
	}
}
