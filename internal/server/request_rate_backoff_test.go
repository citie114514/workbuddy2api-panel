package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestChatModelBusyRotationSkipsBackoff 14003 换号不得插入轮转退避。
//
// 依据：线上 6 条 14003 的 attempts 2~3、duration_ms 1762/4060/7589/8023——每换
// 一次号就叠一次 500ms 起步的退避。14003 是**模型级速率窗口**，下一次尝试打的是
// 另一个账号，等待不减少任何单账号的信令，只把「换号即成功」的救路径拖成秒级。
//
// 断言方式：临时把退避基数恢复成生产的 500ms（TestMain 会置 0 加速其他用例），
// 三个号连续返回 14003。若仍退避，两次换号至少叠加 500ms×0.75 + 1000ms×0.75
// ≈ 1.1s；不退避则只有本地往返开销。
func TestChatModelBusyRotationSkipsBackoff(t *testing.T) {
	prev := rotateBackoffBase
	rotateBackoffBase = 500 * time.Millisecond
	defer func() { rotateBackoffBase = prev }()

	var calls int32
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			return &http.Response{StatusCode: http.StatusTooManyRequests,
				Header: http.Header{"Content-Type": {"application/json"}},
				Body:   io.NopCloser(strings.NewReader(`{"code":14003,"msg":"model capacity busy"}`))}, nil
		})},
		ChatBaseCN: "https://fake.example", BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "mb1", AccessToken: "at-mb1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "mb2", AccessToken: "at-mb2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "mb3", AccessToken: "at-mb3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	elapsed := time.Since(start)

	if n := atomic.LoadInt32(&calls); n < 2 {
		t.Fatalf("三个号都命中 14003 时必须换号重试，calls=%d", n)
	}
	if elapsed > 400*time.Millisecond {
		t.Fatalf("14003 换号仍在退避：%d 次尝试耗时 %v（退避基数为 %v）",
			atomic.LoadInt32(&calls), elapsed, rotateBackoffBase)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("全池 14003 应为 429，got %d body=%s", rec.Code, rec.Body)
	}
}
