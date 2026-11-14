package upstream

import (
	"errors"
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func verifyAuth() *auth.Auth {
	return &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
}

func verifyClient(status int, body string) *Client {
	return testClient(func(*http.Request) (*http.Response, error) {
		return jsonResp(status, body), nil
	})
}

func TestVerifySessionByRefresh(t *testing.T) {
	tests := []struct {
		name   string
		client *Client
		auth   *auth.Auth
		want   VerifyVerdict
	}{
		{"刷新成功", verifyClient(200, `{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":7200}}`), verifyAuth(), VerifyAlive},
		{"应用明确拒绝", verifyClient(400, `{"code":12153,"msg":"refresh token invalid"}`), verifyAuth(), VerifyDead},
		{"应用其他四xx拒绝", verifyClient(403, `{"code":9999,"msg":"forbidden"}`), verifyAuth(), VerifyDead},
		{"上游五xx", verifyClient(503, "service unavailable"), verifyAuth(), VerifyInconclusive},
		{"限流", verifyClient(429, `{"msg":"rate limit"}`), verifyAuth(), VerifyInconclusive},
		{"网关401", verifyClient(401, "<html>401 Authorization Required</html>"), verifyAuth(), VerifyInconclusive},
		{"WAF403", verifyClient(403, "<html>blocked</html>"), verifyAuth(), VerifyInconclusive},
		{"无刷新令牌", verifyClient(200, `{}`), &auth.Auth{UID: "u1"}, VerifyInconclusive},
		{"空账号", verifyClient(200, `{}`), nil, VerifyInconclusive},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.client.VerifySessionByRefresh(tt.auth); got != tt.want {
				t.Fatalf("VerifySessionByRefresh() = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("网络错误", func(t *testing.T) {
		c := testClient(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("network unavailable")
		})
		if got := c.VerifySessionByRefresh(verifyAuth()); got != VerifyInconclusive {
			t.Fatalf("got %v, want VerifyInconclusive", got)
		}
	})
	var nilClient *Client
	if got := nilClient.VerifySessionByRefresh(verifyAuth()); got != VerifyInconclusive {
		t.Fatalf("nil client got %v, want VerifyInconclusive", got)
	}
}

func TestGatewayUnauthorizedClassification(t *testing.T) {
	for _, body := range []string{"", "openresty", "<html>401 Authorization Required</html>"} {
		if !IsGatewayUnauthorized(401, body) {
			t.Fatalf("body %q should be gateway unauthorized", body)
		}
		if got := Classify(401, body); got != ErrSessionDead {
			t.Fatalf("Classify(401, %q) = %v, want ErrSessionDead", body, got)
		}
	}
	body := `{"code":9999,"msg":"bad token"}`
	if IsGatewayUnauthorized(401, body) || Classify(401, body) != ErrClient {
		t.Fatal("business 401 must not be treated as a bare gateway response")
	}
}
