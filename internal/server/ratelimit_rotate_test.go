package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChat429DoesNotConsumeRotateBudget 429 软限流不消耗 MaxRotate 预算。
//
// 场景：MaxRotate=1、两个账号、两个都返回 429。
//   - 旧行为（429 占预算）：只试 1 个号就把 429 交给客户端，另一个号从未被冷却；
//   - 新行为（429 豁免）：两个号都会被尝试并各自进入 soft_rate 冷却，预算耗不耗
//     不应该由「被限流」这件事决定——换到未冷却的号才是正解。
//
// 断言只依赖「两个号都被冷却过」，不依赖选号顺序（加权随机下先选谁都可以）。
func TestChat429DoesNotConsumeRotateBudget(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, `{"code":1,"msg":"rate limit"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "r1", AccessToken: "at-r1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "r2", AccessToken: "at-r2", ExpiresAt: 9999999999},
	)
	p.SetCredits("r1", 2000, 0)
	p.SetCredits("r2", 2000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != 429 {
		t.Fatalf("code=%d want 429, body=%s", rec.Code, rec.Body)
	}
	for _, uid := range []string{"r1", "r2"} {
		st, _ := p.Status(uid)
		if !st.Cooling {
			t.Errorf("%s 未被尝试/冷却 —— 429 消耗了轮换预算（应豁免）: %+v", uid, st)
			continue
		}
		if st.CoolKind != "soft_rate" {
			t.Errorf("%s 冷却类型=%q want soft_rate", uid, st.CoolKind)
		}
		if st.Disabled {
			t.Errorf("%s 不该因 429 被禁用", uid)
		}
		if st.ErrTotal != 0 {
			t.Errorf("%s 不该累计错误计数: err_total=%d", uid, st.ErrTotal)
		}
	}
}

// TestChat429ExemptionHasHardCap 豁免有硬上限，不会因并发把冷却号捞回来而原地打转。
//
// 做法：三个号全 429、MaxRotate=1，断言请求正常结束（不挂死）且响应仍是 429。
// 豁免上限 = 账号数 + MaxRotate = 4，所以最多 5 次尝试就必须收敛。
func TestChat429ExemptionHasHardCap(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 429, `{"code":1,"msg":"rate limit"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a", AccessToken: "at-a", ExpiresAt: 9999999999},
		&auth.Auth{UID: "b", AccessToken: "at-b", ExpiresAt: 9999999999},
		&auth.Auth{UID: "c", AccessToken: "at-c", ExpiresAt: 9999999999},
	)
	for _, uid := range []string{"a", "b", "c"} {
		p.SetCredits(uid, 2000, 0)
	}
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1})

	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
		if rec.Code != 429 {
			t.Errorf("code=%d want 429", rec.Code)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("请求未收敛（超过 10s）—— 429 豁免可能没有硬上限，正在原地打转")
	}
	// 上限 = 账号数 3 + MaxRotate 1 = 4 次豁免，加上首次共 5 次；留一点余量防并发重试。
	if calls > 8 {
		t.Errorf("上游被调用 %d 次，超过豁免硬上限预期（应 ≤5）", calls)
	}
}
