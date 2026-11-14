package server

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 11134「模型提供方暂时不可用」是 (账号,模型) 侧的窗口，不是账号故障：
// 同一个模型换任何账号都会撞上，账号本身健康。若按 5xx 处理会喂熔断计数，
// 而熔断阈值是 3 —— 上游某家模型抖一下就会把一批健康账号熔断出池。
func TestModelUnavailableIsModelLevel(t *testing.T) {
	const body = `{"code":11134,"msg":"model provider temporarily unavailable","requestId":"x"}`

	// 一、分类层：无论上游用 500 还是 503 承载，都必须归到模型级。
	for _, status := range []int{500, 503} {
		if got := upstream.Classify(status, body); got != upstream.ErrModelBusy {
			t.Errorf("Classify(%d, 11134) = %v，期望 ErrModelBusy（模型级）", status, got)
		}
	}
	// 对照：普通 5xx（无业务码）仍必须是账号级 ErrServer。
	if got := upstream.Classify(503, `{"code":500,"msg":"internal error"}`); got != upstream.ErrServer {
		t.Errorf("无关 5xx 应仍为 ErrServer，实际 %v", got)
	}

	// 二、策略层：连续 4 次（超过熔断阈值 3）也不得熔断账号、不累计账号错误。
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p})
	for i := 0; i < 4; i++ {
		h.applyErrorPolicy("u1", upstream.ErrModelBusy, body, "glm-5.3-flash", nil)
	}

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号状态取不到")
	}
	if st.BreakerFails != 0 || !st.BreakerUntil.IsZero() {
		t.Fatalf("模型级错误熔断了账号：breaker_fails=%d breaker_until=%v", st.BreakerFails, st.BreakerUntil)
	}
	if st.ErrTotal != 0 {
		t.Errorf("模型级错误不该累计账号错误：err_total=%d", st.ErrTotal)
	}
	if st.Cooling {
		t.Errorf("模型级错误不该让整个账号冷却：cool_kind=%q", st.CoolKind)
	}
}
