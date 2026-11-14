package scheduler

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func keepaliveWindowScheduler(t *testing.T, window time.Duration) (*Scheduler, *pool.Pool, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":3600}}`))
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up, KeepaliveRefreshWindow: window}), p, &calls
}

func TestScheduledKeepaliveOnlyRefreshesExpiringTokens(t *testing.T) {
	s, p, calls := keepaliveWindowScheduler(t, 7*24*time.Hour)
	p.Add(&auth.Auth{UID: "fresh", AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(30 * 24 * time.Hour).Unix()})
	p.Add(&auth.Auth{UID: "soon", AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(3 * 24 * time.Hour).Unix()})

	s.runKeepalive(true)
	if got := calls.Load(); got != 1 {
		t.Fatalf("scheduled keepalive calls=%d want 1 (only expiring token)", got)
	}

	// 手动入口保持全量语义：两个账号都应刷新。
	s.RunKeepaliveNow()
	if got := calls.Load(); got != 3 {
		t.Fatalf("manual keepalive total calls=%d want 3", got)
	}
}

func TestScheduledKeepaliveRefreshesExpiredAndUnknownExpiry(t *testing.T) {
	s, p, calls := keepaliveWindowScheduler(t, 7*24*time.Hour)
	p.Add(&auth.Auth{UID: "expired", AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(-time.Hour).Unix()})
	p.Add(&auth.Auth{UID: "unknown", AccessToken: "a", RefreshToken: "r", ExpiresAt: 0})

	s.runKeepalive(true)
	if got := calls.Load(); got != 2 {
		t.Fatalf("expired/unknown expiry must refresh, calls=%d want 2", got)
	}
}

func TestKeepaliveWindowDefaultAndHotUpdate(t *testing.T) {
	s, p, calls := keepaliveWindowScheduler(t, 0)
	if got := s.keepaliveRefreshWindow(); got != 7*24*time.Hour {
		t.Fatalf("default window=%v want 168h", got)
	}
	p.Add(&auth.Auth{UID: "fresh", AccessToken: "a", RefreshToken: "r", ExpiresAt: time.Now().Add(3 * 24 * time.Hour).Unix()})

	s.SetKeepaliveRefreshWindow(24 * time.Hour)
	if got := s.keepaliveRefreshWindow(); got != 24*time.Hour {
		t.Fatalf("hot window=%v want 24h", got)
	}
	s.runKeepalive(true)
	if got := calls.Load(); got != 0 {
		t.Fatalf("3-day token outside 24h window should not refresh, calls=%d", got)
	}
}
