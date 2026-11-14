// upstream_timeout_test.go 钉住 isUpstreamTimeout 的三态判定口径：超时和
// "网络抖动"此前共用同一条换号路径——注定超时的请求会轮转 MaxRotate 次，
// 期间还给一串健康号喂连败计数。移植自 OkRoromori 分支（七例对照）。
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type timeoutErr struct{ timeout bool }

func (e timeoutErr) Error() string   { return "stub" }
func (e timeoutErr) Timeout() bool   { return e.timeout }
func (e timeoutErr) Temporary() bool { return e.timeout }

func TestIsUpstreamTimeout(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		clientGone bool
		want       bool
	}{
		{"net.Error 超时", timeoutErr{true}, false, true},
		{"net.Error 非超时（连接被拒）", timeoutErr{false}, false, false},
		{"DNS 超时不算上游停滞", &net.DNSError{Err: "i/o timeout", Name: "fake.example", IsTimeout: true}, false, false},
		{"dial 超时不算上游停滞", &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{true}}, false, false},
		{"dial deadline 不算上游停滞", fmt.Errorf("request: %w", &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}), false, false},
		{"TLS 握手超时不算上游停滞", errors.New("net/http: TLS handshake timeout"), false, false},
		{"已建连响应超时仍算上游超时", &net.OpError{Op: "read", Net: "tcp", Err: timeoutErr{true}}, false, true},
		{"显式 deadline", fmt.Errorf("read: %w", context.DeadlineExceeded), false, true},
		{"客户端断连（ctx 已取消）", context.Canceled, true, false},
		{"空闲看门狗掐流（客户端仍在）", context.Canceled, false, true},
		{"普通错误", errors.New("boom"), false, false},
		{"nil", nil, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isUpstreamTimeout(c.err, c.clientGone); got != c.want {
				t.Fatalf("isUpstreamTimeout(%v, clientGone=%v) = %v, want %v", c.err, c.clientGone, got, c.want)
			}
		})
	}

	// 保证 stub 真的实现了 net.Error（否则上面的用例会静默走不到 Timeout 分支）
	var ne net.Error = timeoutErr{true}
	_ = ne
}

func TestRetryAfterHeaderRoundsUp(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "1"},
		{time.Nanosecond, "1"},
		{time.Second, "1"},
		{1001 * time.Millisecond, "2"},
		{7 * time.Second, "7"},
	}
	for _, tc := range cases {
		if got := retryAfterHeader(tc.d); got != tc.want {
			t.Errorf("retryAfterHeader(%v)=%q want %q", tc.d, got, tc.want)
		}
	}
}

func testTimeoutUpstream(hit map[string]int, timeoutAuths map[string]bool) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			hit[authz]++
			if timeoutAuths[authz] {
				return nil, timeoutErr{true}
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

func runTimeoutChat(h *Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	return rec
}

func TestUpstreamTimeoutExtraRotationSecondLuck(t *testing.T) {
	hit := map[string]int{}
	up := testTimeoutUpstream(hit, map[string]bool{"Bearer at-bad": true})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "ok", AccessToken: "at-ok", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, TimeoutRotateExtra: 1})
	if rec := runTimeoutChat(h); rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if hit["Bearer at-bad"] != 1 || hit["Bearer at-ok"] != 1 {
		t.Fatalf("calls=%v, want one attempt per account", hit)
	}
	for _, uid := range []string{"bad", "ok"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.BreakerFails != 0 || st.ConsecutiveFails != 0 || st.ErrTotal != 0 || st.InFlight != 0 {
			t.Fatalf("timeout must leave no penalty or lease on %s: %+v", uid, st)
		}
	}
}

func TestUpstreamTimeoutExtraBudgetExhausted(t *testing.T) {
	hit := map[string]int{}
	up := testTimeoutUpstream(hit, map[string]bool{"Bearer at-a": true, "Bearer at-b": true, "Bearer at-c": true})
	p := testPoolWith(
		&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
		&auth.Auth{UID: "c", AccessToken: "at-c", ExpiresAt: 9999999999},
	)
	p.SetCredits("a", 3000, 0)
	p.SetCredits("b", 2000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, TimeoutRotateExtra: 1})
	rec := runTimeoutChat(h)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "upstream timed out") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if hit["Bearer at-a"] != 1 || hit["Bearer at-b"] != 1 || hit["Bearer at-c"] != 0 {
		t.Fatalf("calls=%v, want pure-timeout budget of two attempts", hit)
	}
	for _, uid := range []string{"a", "b"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.BreakerFails != 0 || st.ConsecutiveFails != 0 || st.ErrTotal != 0 || st.InFlight != 0 {
			t.Fatalf("timeout must leave no penalty or lease on %s: %+v", uid, st)
		}
	}
}

func TestUpstreamTimeoutWithoutExtraReleasesSticky(t *testing.T) {
	hit := map[string]int{}
	up := testTimeoutUpstream(hit, map[string]bool{"Bearer at-bad": true})
	p := testPoolWith(&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999})
	sess := session.New(session.Config{Available: func() []string { return []string{"bad"} }})
	sess.Bind("conv-timeout", "bad")
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, TimeoutRotateExtra: 0})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-timeout"}}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if _, ok := sess.LookupForModel("conv-timeout", "glm-5.2"); ok {
		t.Fatal("terminal timeout must release sticky binding")
	}
}

func TestUpstreamTimeoutExtraDisabledZero(t *testing.T) {
	hit := map[string]int{}
	up := testTimeoutUpstream(hit, map[string]bool{"Bearer at-bad": true})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "ok", AccessToken: "at-ok", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, TimeoutRotateExtra: 0})
	if rec := runTimeoutChat(h); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if hit["Bearer at-bad"] != 1 || hit["Bearer at-ok"] != 0 {
		t.Fatalf("calls=%v, want rotation disabled", hit)
	}
	st, _ := p.Status("bad")
	if st.InFlight != 0 || st.ConsecutiveFails != 0 || st.Cooling {
		t.Fatalf("disabled extra rotation must still release and avoid penalty: %+v", st)
	}
}
