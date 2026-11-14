package scheduler

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// sessionTestScheduler 与 enterpriseTestScheduler 同构：switched=true 时账号的
// AccessToken 为带 token_source=enterprise_switch 的 JWT（企业**切换会话**，
// enterpriseId 为空——网关无法凭 enterpriseId 识别的形态）；false 为个人号对照组。
func sessionTestScheduler(t *testing.T, switched bool) (*Scheduler, *recorderUpstream) {
	t.Helper()
	r := &recorderUpstream{}
	srv := r.server()
	t.Cleanup(srv.Close)

	token := "at"
	if switched {
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"u1","token_source":"enterprise_switch"}`))
		token = "hdr." + payload + ".sig"
	}
	a := &auth.Auth{UID: "u1", AccessToken: token, RefreshToken: "rt", ExpiresAt: 9999999999}
	p := pool.New("")
	p.Add(a)

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
		WebBaseCN:     srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	return s, r
}

// TestEnterpriseSwitchOnlyAllowsTasks 未绑定企业租户的 enterprise_switch 个人账号
// 必须照常执行全部任务，不能仅凭 token_source 门控；普通个人号作为对照。
func TestEnterpriseSwitchOnlyAllowsTasks(t *testing.T) {
	restoreClock := nowForNightWindow
	nowForNightWindow = func() time.Time { return time.Date(2026, 10, 12, 23, 30, 0, 0, time.Local) }
	t.Cleanup(func() { nowForNightWindow = restoreClock })

	cases := []struct {
		name     string
		run      func(*Scheduler)
		endpoint string
	}{
		{"签到", func(s *Scheduler) { s.RunCheckinNow() }, "/daily-checkin"},
		{"活跃上报", func(s *Scheduler) { s.RunActivityNow() }, "/v2/report"},
		{"猫猫旅行", func(s *Scheduler) { s.RunTravelNow() }, "/activity/growth/buddy/info"},
		{"夜猫子", func(s *Scheduler) { s.RunBlackcatNow() }, "/v2/activity/growth/tasks"},
		{"连登管家", func(s *Scheduler) { s.RunStreakBonusNow() }, "/activity/growth/streak"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, switched := range []bool{true, false} {
				s, r := sessionTestScheduler(t, switched)
				c.run(s)
				if !r.has(c.endpoint) {
					t.Errorf("switched=%v 应照常发起 %s，实际请求=%v", switched, c.endpoint, r.snapshot())
				}
			}
		})
	}
}

// TestEnterpriseSessionKeepsKeepalive 切换会话照常保活（token 会过期，会话
// 形态不影响选号/刷新能力）。
func TestEnterpriseSessionKeepsKeepalive(t *testing.T) {
	s, r := sessionTestScheduler(t, true)
	s.RunKeepaliveNow()
	if !r.has("/token/refresh") {
		t.Errorf("企业切换会话必须照常保活，实际请求=%v", r.snapshot())
	}
}
