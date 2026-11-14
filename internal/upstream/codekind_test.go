package upstream

// 官方业务码表的分类回归。
//
// 码表来自内置 CLI 的 classifyErrorDetail（codebuddy-lite-wb.mjs）：
//
//	[10105, quota/quota_active_session] [15001, quota/quota_web_search]
//	[14003, quota/quota_request_limit]
//	[14001,14002,14012,14013,14014,14018, quota/quota_balance_exhausted]
//	[14015, auth/auth_expired] [14016,14017, quota/quota_not_activated]
//	[11140, auth/auth_forbidden] [11141, model_service/model_behavior_error]
//	[11142, auth/auth_forbidden]
//
// 这里钉住的核心语义：**业务码优先于 HTTP 状态码**。上游把额度耗尽/授权过期/
// 未激活全套在 429 上返回，只看状态码会把三者一律当成可自愈的软限流。

import (
	"net/http"
	"testing"
)

// TestOfficialCodeKind 每个官方码的分类与状态码无关。
func TestOfficialCodeKind(t *testing.T) {
	cases := []struct {
		code int
		want ErrKind
		why  string
	}{
		// 额度耗尽 → 硬冷却（等签到/领包补充，冷却期间重试没意义）
		{14001, ErrHardCredit, "UsageLimitExceeded"},
		{14002, ErrHardCredit, "balance_exhausted 另一变体"},
		{14012, ErrHardCredit, "UsageLimitExceededEnterprise"},
		{14013, ErrHardCredit, "UsageLimitExceededTencent"},
		{14014, ErrHardCredit, "UsageLimitEnterpriseExhausted"},
		{14018, ErrHardCredit, "UsageLimitUserExhausted"},
		{14019, ErrHardCredit, "UsageLimitNoTokenBudget"},
		// 账号状态 → 账号级故障（重试等不来自愈）
		{14015, ErrAccountFault, "LicenseExpired（官方归 auth/auth_expired）"},
		{14016, ErrAccountFault, "EnterpriseNotActivated"},
		{14017, ErrAccountFault, "UserNotActivated"},
	}
	statuses := []int{http.StatusTooManyRequests, http.StatusBadRequest, http.StatusForbidden}
	for _, c := range cases {
		for _, st := range statuses {
			body := `{"code":` + itoa(c.code) + `,"msg":"x"}`
			if got := Classify(st, body); got != c.want {
				t.Errorf("Classify(%d, code=%d %s)=%s want %s", st, c.code, c.why, got, c.want)
			}
		}
	}
}

// TestOfficialCodeKindNested 取码必须能穿透嵌套信封（官方 extractStructuredBizCode
// 同样看 error / data / response.data）。
func TestOfficialCodeKindNested(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":14015,"message":"license expired"}}`,
		`{"data":{"code":14015}}`,
		`{"response":{"data":{"code":14015}}}`,
		`{"code":"14015"}`, // 字符串形式
	} {
		if got := Classify(http.StatusTooManyRequests, body); got != ErrAccountFault {
			t.Errorf("Classify(429, %s)=%s want account_fault", body, got)
		}
	}
}

// TestOfficialCodeKindDoesNotOverreach 码表不得把既有语义抢走：
// 6000-6008 仍是模型级限流、14003 仍是模型繁忙、6020/6021 仍是渠道容量、
// 未列出的码仍按状态码兜底。
func TestOfficialCodeKindDoesNotOverreach(t *testing.T) {
	// 6004 模型级限流：走 IsModelRateLimit 而不是被 officialCodeKind 接走
	if got := Classify(http.StatusTooManyRequests, `{"code":6004,"msg":"x"}`); got == ErrHardCredit || got == ErrAccountFault {
		t.Errorf("6004 不该被判成额度/账号故障，得到 %s", got)
	}
	// 14003 模型繁忙
	if got := Classify(http.StatusTooManyRequests, `{"code":14003,"msg":"模型繁忙"}`); got != ErrModelBusy {
		t.Errorf("14003=%s want model_busy", got)
	}
	// 6020/6021 渠道容量
	for _, c := range []string{"6020", "6021"} {
		if got := Classify(http.StatusTooManyRequests, `{"code":`+c+`}`); got != ErrUpstreamBusy {
			t.Errorf("%s=%s want upstream_busy", c, got)
		}
	}
	// 不在表内的码：429 仍落软限流
	if got := Classify(http.StatusTooManyRequests, `{"code":19999,"msg":"x"}`); got != ErrSoftRate {
		t.Errorf("未知码 429=%s want soft_rate", got)
	}
	// 10105（活跃会话过多）官方归 quota_active_session，但它是**可自愈**的软限流：
	// 换号或稍等即可，不该弃号。保持现状（不列入额度耗尽表）。
	if got := Classify(http.StatusTooManyRequests, `{"code":10105,"msg":"too many active sessions"}`); got != ErrSoftRate {
		t.Errorf("10105=%s want soft_rate（可自愈，不该弃号）", got)
	}
}

// TestAccountRestrictedCodes 11142 与 11140 同属 auth_forbidden，
// 但仍要求 code + 文案双条件（11140 曾因只按 code 判定误伤模型级限流）。
func TestAccountRestrictedCodes(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"code":11140,"msg":"request illegal"}`, true},
		{`{"code":11142,"msg":"request illegal"}`, true},
		// 有码无文案：可能是别的语义，不判受限
		{`{"code":11142,"msg":"model provider is rate-limiting requests"}`, false},
		{`{"code":11140,"msg":"model provider is rate-limiting requests"}`, false},
		// 有文案无码：11128 通道校验失败，不是账号限制
		{`{"code":11128,"msg":"request illegal"}`, false},
		{`{"msg":"request illegal"}`, false},
	}
	for _, c := range cases {
		if got := IsAccountRestricted(c.body); got != c.want {
			t.Errorf("IsAccountRestricted(%s)=%v want %v", c.body, got, c.want)
		}
	}
}

// TestAccountFaultReason 原因文案必须带**实际**码，否则扩表后 14015 会被误标成
// trial not activated，排查方向直接跑偏。
func TestAccountFaultReason(t *testing.T) {
	cases := map[string]string{
		`{"code":14015}`: "account fault (14015)",
		`{"code":14016}`: "account fault (14016)",
		`{"code":14017}`: "account fault (14017)",
		`{"msg":"something else"}`: "account fault",
	}
	for body, want := range cases {
		if got := AccountFaultReason(body); got != want {
			t.Errorf("AccountFaultReason(%s)=%q want %q", body, got, want)
		}
	}
}

// itoa 小工具，避免为几个用例引入 strconv。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
