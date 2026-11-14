package safefile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestWriteAtomicCreatesBackup 第二次写入时旧内容转成 .bak，主文件是新内容。
func TestWriteAtomicCreatesBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteAtomic(path, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Fatal("首次写入没有旧文件，不应产生 .bak")
	}
	if err := WriteAtomic(path, []byte(`{"v":2}`), 0o600); err != nil {
		t.Fatalf("second write: %v", err)
	}
	cur, err := os.ReadFile(path)
	if err != nil || string(cur) != `{"v":2}` {
		t.Fatalf("主文件内容=%q err=%v", cur, err)
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil || string(bak) != `{"v":1}` {
		t.Fatalf("备份内容=%q err=%v", bak, err)
	}
	// tmp 不应残留（否则下次写入前会被覆盖，但脏文件会误导排查）。
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatal("不应残留 .tmp")
	}
}

// TestReadWithBackupFallsBack 主文件缺失/为空/非法时回退 .bak（零长度文件是掉电
// 最常见的损坏形态，把它当合法状态就会把真实状态抹掉）。
func TestReadWithBackupFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteAtomic(path, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(path, []byte(`{"v":2}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		mut   func() error
		want  string
		isBak bool
	}{
		{"主文件为空", func() error { return os.WriteFile(path, nil, 0o600) }, `{"v":1}`, true},
		{"主文件被截断", func() error { return os.WriteFile(path, []byte(`{"v":`), 0o600) }, `{"v":1}`, true},
		{"主文件缺失", func() error { return os.Remove(path) }, `{"v":1}`, true},
	}
	for _, c := range cases {
		if err := c.mut(); err != nil {
			t.Fatalf("%s: 准备失败 %v", c.name, err)
		}
		raw, usedBak, err := ReadWithBackup(path, JSONValidator())
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(raw) != c.want || usedBak != c.isBak {
			t.Fatalf("%s: raw=%q usedBak=%v want %q/%v", c.name, raw, usedBak, c.want, c.isBak)
		}
	}
}

// TestReadWithBackupHealthy 主文件正常时从不使用备份。
func TestReadWithBackupHealthy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WriteAtomic(path, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, usedBak, err := ReadWithBackup(path, JSONValidator())
	if err != nil || usedBak || string(raw) != `{"v":1}` {
		t.Fatalf("raw=%q usedBak=%v err=%v", raw, usedBak, err)
	}
}

// TestReadWithBackupNoBackupReturnsError 主文件与备份都不可用时返回错误（调用方据此
// 走"无状态"路径），而不是返回空内容。
func TestReadWithBackupNoBackupReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	if _, _, err := ReadWithBackup(path, JSONValidator()); err == nil {
		t.Fatal("主文件与备份都缺失时应返回错误")
	}
}

// TestWriteAtomicKeepsMode 权限按 perm 创建（凭证/状态文件不应因 umask 变成 world-readable）。
func TestWriteAtomicKeepsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 不按 POSIX 权限位工作（CreateFile 的 mode 参数被忽略），
		// 这里只验证类 Unix 平台。
		t.Skip("POSIX 权限位在 Windows 上不生效")
	}
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := WriteAtomic(path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perm=%v want 0600", st.Mode().Perm())
	}
}
