package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// reachedFixture 造一个「chat_5 已达标待领」的假上游，并统计两类调用：
//   - reports：/v2/report 的行为事件上报次数（重复执行的动作证据）
//   - claims ：领奖次数
//
// readCurrent 指定任务列表回读时下发的 current——用它可以模拟上游计分异步
// 导致的「执行前回读偏低」。
type reachedFixture struct {
	reports int32
	claims  int32
	tasks   int32
}

func newReachedFixture(t *testing.T, readCurrent int64) (*reachedFixture, *Panel) {
	t.Helper()
	f := &reachedFixture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			atomic.AddInt32(&f.tasks, 1)
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"tasks": []map[string]any{
					{"task_code": "chat_5", "title": "和 AI 聊天 5 次", "target": 5,
						"current": readCurrent, "accept_status": "accepted",
						"reward_credit": 100, "reward_energy": 5},
				}},
			})
		case r.URL.Path == "/v2/report":
			atomic.AddInt32(&f.reports, 1)
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK"})
		case strings.HasPrefix(r.URL.Path, "/activity/growth/tasks/") && strings.HasSuffix(r.URL.Path, "/claim"):
			atomic.AddInt32(&f.claims, 1)
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"already_claimed": false, "credit": 100, "energy": 5},
			})
		default:
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
		}
	}))
	t.Cleanup(srv.Close)
	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
	return f, p
}

// TestGrowthQueuedAtTargetSkipsAction 队列里「扫描时已达标」的任务只领奖、不重跑动作。
//
// 复现线上事故：134****8115 的 chat_5 在 19:42 已补报 5 条并达标，19:54 再扫时
// 以「达标未领」入队，执行期又把 5 条事件重打一遍，撞上游限流只剩
// `upstream soft_rate (http 429)`。达标任务的动作没有任何收益，必须先挡掉。
func TestGrowthQueuedAtTargetSkipsAction(t *testing.T) {
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })

	// 回读给满分：动作本来就不该被调用。
	f, p := newReachedFixture(t, 5)
	msg, err := p.runGrowthQueued(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5", true)
	if err != nil {
		t.Fatalf("runGrowthQueued err = %v", err)
	}
	if got := atomic.LoadInt32(&f.reports); got != 0 {
		t.Errorf("/v2/report 上报 %d 次，want 0（达标任务不得重跑动作）", got)
	}
	if got := atomic.LoadInt32(&f.claims); got != 1 {
		t.Errorf("claim %d 次，want 1（达标未领必须补领）", got)
	}
	if !strings.Contains(msg, "已达标") {
		t.Errorf("msg = %q，want 含「已达标」", msg)
	}
}

// TestGrowthQueuedAtTargetSurvivesStaleRead 扫描判达标后，即使执行前回读因上游
// 计分异步而翻回 0/5，也不得重跑动作。
//
// 这是本次事故的直接成因：只信执行前回读 → 把「已达标待领」重新判成待办 →
// 又打一批事件 → 429。扫描期的判定必须能压住抖动的回读。
func TestGrowthQueuedAtTargetSurvivesStaleRead(t *testing.T) {
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })

	f, p := newReachedFixture(t, 0) // 回读抖动成 0/5
	if _, err := p.runGrowthQueued(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5", true); err != nil {
		t.Fatalf("runGrowthQueued err = %v", err)
	}
	if got := atomic.LoadInt32(&f.reports); got != 0 {
		t.Errorf("/v2/report 上报 %d 次，want 0（扫描已判达标，不该被抖动回读拉回执行）", got)
	}
}

// TestGrowthQueuedUnfinishedStillRuns 反向验证：未达标的任务照常执行动作，
// 修复不能把「真该补进度」的路径一起挡掉。
func TestGrowthQueuedUnfinishedStillRuns(t *testing.T) {
	old := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = old })

	f, p := newReachedFixture(t, 0)
	msg, err := p.runGrowthQueued(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5", false)
	if err != nil {
		t.Fatalf("runGrowthQueued err = %v", err)
	}
	if got := atomic.LoadInt32(&f.reports); got != 5 {
		t.Errorf("/v2/report 上报 %d 次，want 5（0/5 需按差额补满）", got)
	}
	if !strings.Contains(msg, "补报") {
		t.Errorf("msg = %q，want 含「补报」", msg)
	}
}

// TestTaskReached 判据口径：Claimable 或 target>0 && current>=target。
func TestTaskReached(t *testing.T) {
	cases := []struct {
		name string
		t    *upstream.Task
		want bool
	}{
		{"nil", nil, false},
		{"已达标", &upstream.Task{Target: 5, Current: 5}, true},
		{"超额", &upstream.Task{Target: 5, Current: 7}, true},
		{"未达标", &upstream.Task{Target: 5, Current: 4}, false},
		{"上游标可领", &upstream.Task{Target: 5, Current: 0, Claimable: true}, true},
		{"无目标", &upstream.Task{Target: 0, Current: 0}, false},
	}
	for _, c := range cases {
		if got := taskReached(c.t); got != c.want {
			t.Errorf("%s: taskReached = %v, want %v", c.name, got, c.want)
		}
	}
}
