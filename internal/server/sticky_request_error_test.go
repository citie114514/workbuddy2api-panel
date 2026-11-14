package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 请求级错误与账号无关：同一个 body 换任何账号都是同样的结果，账号本身仍健康、
// 它的上游前缀缓存也还热着。这类错误必须**只释放在途租约、不解绑会话粘性**，
// 否则客户端下一次重试会被分配到另一个号上，而前缀缓存按账号隔离——十万到
// 几十万 token 的前缀要整段重算，表现就是「报错一次之后缓存一直热不起来」。
//
// 这里直接断言绑定表本身没被清空。只看「内存里 Resolve 还能解析出号」不够：
// 账号还被本轮租用（in_flight）时，选号会因满载走溢出分支，测不出绑定是否已被删。
func TestRequestLevelErrorKeepsStickyBinding(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantKind upstream.ErrKind
	}{
		{
			name:     "prompt_too_long",
			status:   400,
			body:     `{"code":11115,"msg":"prompt is too long: 1051130 tokens > 1048576 maximum"}`,
			wantKind: upstream.ErrPromptTooLong,
		},
		{
			name:     "bad_params",
			status:   400,
			body:     `{"code":11101,"msg":"Unmarshal chat params failed"}`,
			wantKind: upstream.ErrBadParams,
		},
		{
			name:     "tool_sequence",
			status:   400,
			body:     `{"code":11148,"msg":"tool calls and tool results do not match"}`,
			wantKind: upstream.ErrToolSequence,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 先确认 body 真的归到这个分支——否则测的是别的东西，绿了也没意义。
			if got := upstream.Classify(tc.status, tc.body); got != tc.wantKind {
				t.Fatalf("分类不符：Classify=%v want %v（用例 body 需修正）", got, tc.wantKind)
			}

			st := newBindStore()
			sess := session.New(session.Config{
				TTL:       time.Minute,
				Store:     st,
				Available: func() []string { return []string{"u1"} },
			})
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return tc.status, tc.body, false
			})
			h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})
			sess.Bind("conv-1", "u1")

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`)))

			if uid, ok := st.lastUID("conv-1"); !ok || uid != "u1" {
				t.Fatalf("请求级错误不该解绑粘性：binding=%q ok=%v delCnt=%d", uid, ok, st.delCnt)
			}
			if st.delCnt != 0 {
				t.Fatalf("请求级错误不该调用 DelBind，实际 %d 次", st.delCnt)
			}
		})
	}
}

// 反向保护：账号级错误（429 软限流）仍必须解绑——那确实该换号，绑在坏号上
// 下一轮只会再撞一次。这条防止上面的改动被扩大到不该保留绑定的分支。
func TestAccountLevelErrorStillUnbindsSticky(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"u1"} },
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 2000, 0)
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 429, `{"code":1,"msg":"rate limit"}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-1", "u1")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`)))

	if st.delCnt == 0 {
		t.Fatalf("账号级错误必须解绑：delCnt=%d（软限流后还绑在坏号上会继续撞）", st.delCnt)
	}
	if uid, ok := st.lastUID("conv-1"); ok {
		t.Fatalf("账号级错误后仍绑在 %q，应已解绑", uid)
	}
}
