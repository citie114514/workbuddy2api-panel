// Package safefile 提供"掉电安全"的原子文件写入与带回退的读取。
//
// 为什么需要：单纯的「写临时文件 + rename」只保证读者看到完整文件，**不保证内容
// 已经落盘**。掉电时 ext4/XFS 上 rename 的日志可能先提交、数据块后刷盘，留下长度
// 正确但内容全 0 的文件；被读成「零状态」后，下一次写入就把真实状态彻底抹掉
// （冷却、停牌、成本观测、用量全丢），而且没有任何报错。
//
// 落盘顺序（每一步失败都不会破坏已有文件）：
//  1. 写 <path>.tmp 并 fsync —— 数据先到盘；
//  2. 把当前文件转成 <path>.bak（rename，原子的元数据操作）；
//  3. <path>.tmp rename 成 <path>；
//  4. fsync 目录（尽力而为：Windows 不支持目录 fsync，忽略其错误）。
//
// 第 2、3 步之间掉电的窗口里 <path> 缺失、<path>.bak 是上一份完整内容，
// 读取侧 ReadWithBackup 会回退到备份，不会把"文件不见了"当成"零状态"。
package safefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WriteAtomic 原子且抗掉电地写文件。perm 用于新文件（既有文件保留自身权限）。
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	if path == "" {
		return errors.New("safefile: empty path")
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := writeSync(tmp, data, perm); err != nil {
		return err
	}
	// 旧文件转备份：失败（例如首次写入没有旧文件）不阻断主流程。
	bak := path + ".bak"
	if _, statErr := os.Stat(path); statErr == nil {
		_ = os.Remove(bak)
		if err := os.Rename(path, bak); err != nil {
			// 备份失败不阻断：主文件仍在，rename(tmp, path) 仍是原子覆盖。
			_ = err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// writeSync 写文件并 fsync 到盘（O_TRUNC 覆盖写）。
func writeSync(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// syncDir 落目录项（rename 本身的持久化）。Windows 不支持对目录 fsync，
// 统一忽略错误——该平台的文件系统没有这一层语义。
func syncDir(dir string) {
	if dir == "" {
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// ReadWithBackup 读取文件；主文件不存在、为空或校验失败时回退到 .bak。
// validate 为 nil 时只校验非空。usedBackup 为 true 表示内容来自备份。
//
// 为什么"空文件"要回退：掉电留下的零长度/全 0 文件是最常见的损坏形态，
// 把它当合法状态继续用就是把真实状态抹掉。
func ReadWithBackup(path string, validate func([]byte) error) (data []byte, usedBackup bool, err error) {
	raw, rerr := os.ReadFile(path)
	if rerr == nil && len(raw) > 0 && (validate == nil || validate(raw) == nil) {
		return raw, false, nil
	}
	bak, berr := os.ReadFile(path + ".bak")
	if berr != nil || len(bak) == 0 {
		if rerr != nil {
			return nil, false, rerr
		}
		if validate != nil {
			if verr := validate(raw); verr != nil {
				return nil, false, fmt.Errorf("safefile: %s 校验失败且无可用备份: %w", path, verr)
			}
		}
		return raw, false, nil
	}
	if validate != nil {
		if verr := validate(bak); verr != nil {
			if rerr != nil {
				return nil, false, rerr
			}
			return raw, false, nil
		}
	}
	return bak, true, nil
}

// JSONValidator 返回一个只校验「是合法 JSON」的 validate 函数，供 ReadWithBackup 使用。
func JSONValidator() func([]byte) error {
	return func(b []byte) error {
		var v any
		return json.Unmarshal(b, &v)
	}
}
