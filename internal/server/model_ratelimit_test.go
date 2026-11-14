package server

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestChat6000ModelCooldownForms(t *testing.T) {
	reset := time.Now().Add(3 * time.Hour)
	resetText := reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	tests := []struct {
		name       string
		body       string
		retryAfter time.Duration
		wantUntil  time.Time
		wantMax    time.Duration
	}{
		{"明确重置时间", `{"code":6000,"msg":"将在 ` + resetText + ` UTC+8 重置，请切换其他模型"}`, 0, reset, 0},
		{"RetryAfter", `{"code":6000,"msg":"已达上限，请切换其他模型"}`, 90 * time.Second, time.Time{}, 90 * time.Second},
		{"无墙钟退避", `{"code":6000,"msg":"已达上限，请切换其他模型"}`, 0, time.Time{}, time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p, Upstream: &upstream.Client{}, SoftCooldown: time.Minute})
			h.applyErrorPolicy("u1", upstream.ErrSoftRate, tt.body, "glm-5.3", &upstream.Error{
				Kind: upstream.ErrSoftRate, Status: 429, RetryAfter: tt.retryAfter,
			})
			st, _ := p.Status("u1")
			if st.Cooling || !st.Until.IsZero() {
				t.Fatalf("model limit must not set account cooldown: %+v", st)
			}
			if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
				t.Fatalf("model rows=%+v", st.RateLimitedModels)
			}
			gotUntil := st.RateLimitedModels[0].Until
			if !tt.wantUntil.IsZero() {
				if d := gotUntil.Sub(tt.wantUntil); d < -time.Second || d > time.Second {
					t.Fatalf("until=%v want %v", gotUntil, tt.wantUntil)
				}
			} else if rem := time.Until(gotUntil); rem <= 0 || rem > tt.wantMax {
				t.Fatalf("remaining=%v want (0,%v]", rem, tt.wantMax)
			}
			if got := p.PickExcludingForModel(nil, "hy3-x"); got == nil || got.UID != "u1" {
				t.Fatalf("other model must remain selectable, got %+v", got)
			}
		})
	}
}

func TestChat6000WithoutSwitchHintUsesAccountCooldown(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: &upstream.Client{}, SoftCooldown: time.Minute})
	body := `{"code":6000,"msg":"model usage limit exceeded"}`
	h.applyErrorPolicy("u1", upstream.ErrSoftRate, body, "glm-5.3", &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429})
	st, _ := p.Status("u1")
	if !st.Cooling || st.CoolKind != "soft_rate" || len(st.RateLimitedModels) != 0 {
		t.Fatalf("6000 without switch hint must remain account-level: %+v", st)
	}
}

func TestModelLimitEmptyModelFallsBackWithoutEmptyKey(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: &upstream.Client{}, SoftCooldown: time.Minute})
	body := `{"code":6000,"msg":"已达上限，请切换其他模型"}`
	h.applyErrorPolicy("u1", upstream.ErrSoftRate, body, "", &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429})
	st, _ := p.Status("u1")
	if !st.Cooling || len(st.RateLimitedModels) != 0 {
		t.Fatalf("empty model must use account cooldown without an empty model key: %+v", st)
	}
}
