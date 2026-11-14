package pool

import (
	"reflect"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestSessionDeadCandidates 自动复核只针对「会话失效」这一类禁用，且按窗口节流。
//
// 11140（上游显式限制，需申诉）与手动禁用不自动探测：前者反复撞会加深风控印象，
// 后者会推翻人的决定。
func TestSessionDeadCandidates(t *testing.T) {
	p := New("")
	for _, uid := range []string{"dead", "banned", "manual"} {
		p.Add(&auth.Auth{UID: uid})
	}
	p.Disable("dead", "12153 session dead")
	p.Disable("banned", "account restricted by upstream (11140 request illegal)")
	p.Disable("manual", "manual disable (panel)")

	if got := p.SessionDeadCandidates(time.Now(), 6*time.Hour); len(got) != 0 {
		t.Fatalf("刚禁用不应立即复核，got %v", got)
	}
	got := p.SessionDeadCandidates(time.Now().Add(6*time.Hour+time.Minute), 6*time.Hour)
	if !reflect.DeepEqual(got, []string{"dead"}) {
		t.Fatalf("候选=%v want [dead]（11140 与手动禁用必须排除）", got)
	}

	// 复核失败后推后一个窗口：立刻再查必须为空。
	p.TouchDisabledAt("dead")
	if got := p.SessionDeadCandidates(time.Now(), 6*time.Hour); len(got) != 0 {
		t.Fatalf("TouchDisabledAt 后不应立即再次成为候选，got %v", got)
	}
}

// TestSessionDeadCandidatesZeroDisabledAt 旧状态文件没有 disabled_at 时，从此刻开始计时，
// 不能在升级瞬间把历史禁用号一起打一遍上游。
func TestSessionDeadCandidatesZeroDisabledAt(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "old"})
	p.Disable("old", "12153 session dead")

	p.mu.Lock()
	p.byUID["old"].disabledAt = time.Time{}
	p.mu.Unlock()

	if got := p.SessionDeadCandidates(time.Now(), time.Second); len(got) != 0 {
		t.Fatalf("零值 disabledAt 首次不应成为候选，got %v", got)
	}
	p.mu.Lock()
	stamped := !p.byUID["old"].disabledAt.IsZero()
	p.mu.Unlock()
	if !stamped {
		t.Fatal("首次判定应补写 disabledAt，否则永远进不了候选")
	}
	got := p.SessionDeadCandidates(time.Now().Add(2*time.Second), time.Second)
	if !reflect.DeepEqual(got, []string{"old"}) {
		t.Fatalf("补写后到期应为候选，got %v", got)
	}
}
