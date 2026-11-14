package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// resetDynamicModels 清动态模型目录缓存（包级状态），避免用例间互相继承失败退避。
func resetDynamicModels(t *testing.T) {
	t.Helper()
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

// TestModelsDualAuth /v1/models 同时认 Bearer 与 x-api-key。
//
// 背景：/v1/messages 挂 withAnthropicAuth（认 x-api-key），而 /v1/models 原本只挂
// withAuth（只认 Bearer）。Anthropic 官方 SDK 的模型发现默认带 x-api-key，模型发现
// 又是它的标准启动动作——同一把 key 能调通 messages 却列不出模型，只能收 401。
//
// 同时钉住 401 的错误体分形：带 x-api-key 的按 Anthropic 口径回，用 Bearer 的仍回
// OpenAI 口径。若图省事统一换成 Anthropic 口径，OpenAI 客户端会把「密钥不对」误报
// 成「网关坏了」。
func TestModelsDualAuth(t *testing.T) {
	const key = "dual-auth-test-key"
	h := NewHandler(Config{
		APIKey:   key,
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
	})

	cases := []struct {
		name    string
		headers map[string]string
		want    int
		// wantErr 是 401 时该出现的错误码/类型；空表示期望 200。
		wantErr string
	}{
		{"Bearer 有效", map[string]string{"Authorization": "Bearer " + key}, 200, ""},
		{"x-api-key 有效", map[string]string{"x-api-key": key}, 200, ""},
		{"Bearer 无效", map[string]string{"Authorization": "Bearer wrong"}, 401, "invalid_api_key"},
		{"x-api-key 无效", map[string]string{"x-api-key": "wrong"}, 401, "authentication_error"},
		{"缺凭据", map[string]string{}, 401, "invalid_api_key"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetDynamicModels(t)
			req := httptest.NewRequest("GET", "/v1/models", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("code=%d want=%d body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != 401 {
				return
			}
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if tc.wantErr == "invalid_api_key" {
				e, _ := resp["error"].(map[string]any)
				if e == nil || e["code"] != "invalid_api_key" {
					t.Fatalf("期望 OpenAI 口径 code=invalid_api_key，实际 %v", resp)
				}
				if resp["type"] == "error" {
					t.Fatalf("Bearer/无凭据场景不应回 Anthropic 信封：%v", resp)
				}
				return
			}
			if resp["type"] != "error" {
				t.Fatalf("期望 Anthropic 信封 type=error，实际 %v", resp)
			}
			e, _ := resp["error"].(map[string]any)
			if e == nil || e["type"] != "authentication_error" {
				t.Fatalf("期望 error.type=authentication_error，实际 %v", resp)
			}
		})
	}
}
