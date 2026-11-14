package pool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestStateRestoreFallsBackToBackup 主文件被写坏（空/半截）时从 .bak 恢复。
//
// 掉电时 rename 的日志可能先提交、数据块后刷盘，留下长度正确但内容为空的
// state.json。没有回退时它会被读成"零状态"，随后一次写入就把冷却/成本/禁用
// 状态彻底抹掉，且没有任何报错。
func TestStateRestoreFallsBackToBackup(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")

	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.Flush()
	// Flush 幂等（无变更不写盘），要有一次真实变更才会产生第二次写入，
	// 从而把上一份完整状态转成 .bak。
	p.NoteError("u1")
	p.Flush()

	if _, err := os.Stat(fp + ".bak"); err != nil {
		t.Fatalf("应有备份文件: %v", err)
	}
	// 模拟掉电损坏：主文件变成空文件。
	if err := os.WriteFile(fp, nil, 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("应能从备份恢复账号状态")
	}
	if !st.Disabled {
		t.Fatalf("应从 .bak 恢复 disabled 状态，实际 %+v", st)
	}
}

// TestStateRestoreWithoutBackupIsEmpty 主文件损坏且无备份时不 panic、按无状态启动。
func TestStateRestoreWithoutBackupIsEmpty(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应存在（来自 auth 目录，无历史状态）")
	}
	if st.Disabled {
		t.Fatalf("无备份时不应凭空恢复出禁用状态: %+v", st)
	}
}
