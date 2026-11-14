package panel

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestTaskFromSnapshotAvoidsUpstream 逐项执行必须优先用阶段 0 的快照。
//
// 这里用「零值 Panel」当探针：它的 cfg.Upstream 为 nil，一旦函数回源就会空指针 panic。
// 能正常返回就证明「没有为这个任务再拉一次列表」——这正是被省掉的那几十次请求。
func TestTaskFromSnapshotAvoidsUpstream(t *testing.T) {
	p := &Panel{}
	tasks := []upstream.Task{{TaskCode: "chat_5"}, {TaskCode: "Sequential_Tasks_1"}}
	snap := map[string]*upstream.Task{}
	mpSnap := map[string]*upstream.Task{}
	for i := range tasks {
		if tasks[i].TaskCode == "Sequential_Tasks_1" {
			mpSnap[tasks[i].TaskCode] = &tasks[i]
			continue
		}
		snap[tasks[i].TaskCode] = &tasks[i]
	}

	got, err := p.taskFromSnapshotOrFetch(nil, "chat_5", snap, mpSnap, true, true)
	if err != nil || got == nil || got.TaskCode != "chat_5" {
		t.Fatalf("快照命中失败: got=%+v err=%v", got, err)
	}

	// mp 码只出现在 mp 列表里：应从 mp 快照取，而不是回源。
	got, err = p.taskFromSnapshotOrFetch(nil, "Sequential_Tasks_1", snap, mpSnap, true, true)
	if err != nil || got == nil || got.TaskCode != "Sequential_Tasks_1" {
		t.Fatalf("mp 快照命中失败: got=%+v err=%v", got, err)
	}

	// 快照里没有 = 该账号确实没有这个任务：返回 nil 且不回源。
	got, err = p.taskFromSnapshotOrFetch(nil, "not_exists", snap, mpSnap, true, true)
	if err != nil || got != nil {
		t.Fatalf("不存在的任务不应回源: got=%+v err=%v", got, err)
	}
}
