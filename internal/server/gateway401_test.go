package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func gatewayVerifyClient(status int, body string) *upstream.Client {
	return &upstream.Client{
		ChatBaseCN: "https://fake.example",
		HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
	}
}

func applyGateway401(h *Handler, uid string) {
	const body = "<html>401 Authorization Required</html>"
	h.applyErrorPolicy(uid, upstream.ErrSessionDead, body, "glm-5.2", &upstream.Error{
		Kind: upstream.ErrSessionDead, Status: http.StatusUnauthorized, Msg: body,
	})
}

func TestGateway401AliveSavesTokensAndClearsDeadCount(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "workbuddy.json")
	a := &auth.Auth{UID: "u1", AccessToken: "old-at", RefreshToken: "old-rt", FilePath: fp, ExpiresAt: 9999999999}
	p := testPoolWith(a)
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	h := NewHandler(Config{Pool: p, Upstream: gatewayVerifyClient(200,
		`{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt","expiresIn":7200}}`)})
	applyGateway401(h, "u1")

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read saved auth: %v", err)
	}
	saved, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("parse saved auth: %v", err)
	}
	if saved.AccessToken != "new-at" || saved.RefreshToken != "new-rt" {
		t.Fatalf("saved tokens = %q/%q", saved.AccessToken, saved.RefreshToken)
	}
	if p.NoteSessionDead("u1") || p.NoteSessionDead("u1") {
		t.Fatal("refresh alive must clear the previous consecutive-dead count")
	}
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("alive account must not be disabled: %+v", st)
	}
}

func TestGateway401AliveSaveFailureDoesNotDisable(t *testing.T) {
	a := &auth.Auth{UID: "u1", AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: 9999999999}
	p := testPoolWith(a)
	h := NewHandler(Config{Pool: p, Upstream: gatewayVerifyClient(200,
		`{"code":0,"data":{"accessToken":"new-at","refreshToken":"new-rt"}}`)})
	applyGateway401(h, "u1")
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("successful refresh with save failure must not disable: %+v", st)
	}
}

func TestGateway401InconclusiveDoesNotAccumulateDead(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: gatewayVerifyClient(503, "temporarily unavailable")})
	for i := 0; i < 4; i++ {
		applyGateway401(h, "u1")
	}
	st, _ := p.Status("u1")
	if st.Disabled || !st.Cooling {
		t.Fatalf("inconclusive result should only soft-cool, got %+v", st)
	}
}

func TestGateway401DeadDisablesAtThird(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: gatewayVerifyClient(400, `{"code":12153,"msg":"refresh token invalid"}`)})
	for i := 0; i < 2; i++ {
		applyGateway401(h, "u1")
		if st, _ := p.Status("u1"); st.Disabled {
			t.Fatalf("dead result %d must not disable yet", i+1)
		}
	}
	applyGateway401(h, "u1")
	st, _ := p.Status("u1")
	if !st.Disabled || st.DisabledReason != "gateway 401 refresh rejected" {
		t.Fatalf("third explicit rejection must disable: %+v", st)
	}
}
