package auth

import (
	"encoding/base64"
	"testing"
)

// mustJWT 用给定 payload 拼一个最小 JWT（header.payload.signature）。
// 构造带指定声明的 JWT，用于确认 token_source 不再影响企业门控。
func mustJWT(t *testing.T, payload string) string {
	t.Helper()
	return "hdr." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

// TestIsEnterpriseSession 只按 enterpriseId 识别企业账号。
// enterprise_switch 可出现在未绑定企业租户的个人账号上，不应据此门控成长任务。
func TestIsEnterpriseSession(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  bool
	}{
		{"enterprise_switch 未绑定租户", mustJWT(t, `{"sub":"u1","token_source":"enterprise_switch"}`), false},
		{"enterprise_switch 的 JWT 租户声明不替代 auth 字段", mustJWT(t, `{"sub":"u1","token_source":"enterprise_switch","enterprise_id":"e9"}`), false},
		{"无 token_source 声明", mustJWT(t, `{"sub":"u1","scope":"openid profile"}`), false},
		{"其他 token_source", mustJWT(t, `{"sub":"u1","token_source":"password"}`), false},
		{"带填充 base64url", "hdr." + base64.URLEncoding.EncodeToString([]byte(`{"token_source":"enterprise_switch"}`)) + ".sig", false},
		{"payload 非 JSON", "hdr." + base64.RawURLEncoding.EncodeToString([]byte("not-json")) + ".sig", false},
		{"非 JWT 形态", "opaque-token", false},
		{"空 token", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &Auth{UID: "u1", AccessToken: c.token}
			if got := a.IsEnterpriseSession(); got != c.want {
				t.Errorf("IsEnterpriseSession()=%v want %v", got, c.want)
			}
		})
	}

	t.Run("enterpriseId 非空短路为真", func(t *testing.T) {
		a := &Auth{UID: "u1", EnterpriseID: "e1", AccessToken: mustJWT(t, `{"sub":"u1"}`)}
		if !a.IsEnterpriseSession() {
			t.Error("IsEnterprise()=true 时 IsEnterpriseSession() 应恒为 true")
		}
	})

	t.Run("企业切换会话不影响 IsEnterprise（计费口径隔离）", func(t *testing.T) {
		a := &Auth{UID: "u1", AccessToken: mustJWT(t, `{"token_source":"enterprise_switch"}`)}
		if a.IsEnterprise() {
			t.Error("仅 token_source=enterprise_switch 不应翻转 IsEnterprise（额度查询仍走个人口径）")
		}
	})
}
