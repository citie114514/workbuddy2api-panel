package upstream

import (
	"errors"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// VerifyVerdict 是裸网关 401 经 refresh 求证后的结论。
type VerifyVerdict int

const (
	VerifyAlive VerifyVerdict = iota
	VerifyDead
	VerifyInconclusive
)

func (v VerifyVerdict) String() string {
	switch v {
	case VerifyAlive:
		return "alive"
	case VerifyDead:
		return "dead"
	default:
		return "inconclusive"
	}
}

// VerifySessionByRefresh 复用现有刷新及 singleflight 求证会话。
// 只有应用层明确的 4xx 拒绝才判死；网络、限流和边缘拦截均无明确判词。
// 成功后的凭证落盘与账号状态处理由调用方负责。
func (c *Client) VerifySessionByRefresh(a *auth.Auth) VerifyVerdict {
	if c == nil || a == nil || strings.TrimSpace(a.RefreshTokenValue()) == "" {
		return VerifyInconclusive
	}
	err := c.RefreshToken(a)
	if err == nil {
		return VerifyAlive
	}
	var re *Error
	if !errors.As(err, &re) {
		return VerifyInconclusive
	}
	if re.Status >= 500 || re.Status == http.StatusTooManyRequests ||
		IsGatewayUnauthorized(re.Status, re.Msg) || IsWafBlocked(re.Status, re.Msg) {
		return VerifyInconclusive
	}
	if re.Status >= 400 && re.Status < 500 {
		return VerifyDead
	}
	return VerifyInconclusive
}
