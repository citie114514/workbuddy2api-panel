package pool

import (
	"testing"
	"time"
)

// TestModelHangBlocksAtThreshold 观察窗内累计到阈值即熔断：第一次停滞只记录，
// 第二次触发（同一域同一模型），换个模型不受影响。
func TestModelHangBlocksAtThreshold(t *testing.T) {
	p := New("")

	if _, blocked := p.ModelHangBlocked("cn", "m1"); blocked {
		t.Fatal("未观测到停滞时不应熔断")
	}
	p.NoteModelHang("cn", "m1")
	if _, blocked := p.ModelHangBlocked("cn", "m1"); blocked {
		t.Fatal("第 1 次停滞不应熔断（单次可能是偶发抖动）")
	}
	p.NoteModelHang("cn", "m1")
	until, blocked := p.ModelHangBlocked("cn", "m1")
	if !blocked {
		t.Fatal("第 2 次停滞应熔断")
	}
	if time.Until(until) <= 0 || time.Until(until) > hangBlock+time.Second {
		t.Fatalf("熔断截止时刻异常: %v", until)
	}
	// 域/模型维度独立：同模型的另一域、同域的另一模型都不受影响。
	if _, blocked := p.ModelHangBlocked("global", "m1"); blocked {
		t.Fatal("另一域不应被连带熔断")
	}
	if _, blocked := p.ModelHangBlocked("cn", "m2"); blocked {
		t.Fatal("另一模型不应被连带熔断")
	}
	if got := p.HangTotal(); got != 1 {
		t.Fatalf("HangTotal=%d want 1", got)
	}
}

// TestModelHangClearOnSuccess 一次成功即清零观测与熔断（成功是最强的"没挂"证据）。
func TestModelHangClearOnSuccess(t *testing.T) {
	p := New("")
	p.NoteModelHang("cn", "m")
	p.NoteModelHang("cn", "m")
	if _, blocked := p.ModelHangBlocked("cn", "m"); !blocked {
		t.Fatal("precondition: 应已熔断")
	}
	p.ClearModelHang("cn", "m")
	if _, blocked := p.ModelHangBlocked("cn", "m"); blocked {
		t.Fatal("成功后应解除熔断")
	}
	// 清零后要重新累计两次才再熔断（不是一次就跳闸）。
	p.NoteModelHang("cn", "m")
	if _, blocked := p.ModelHangBlocked("cn", "m"); blocked {
		t.Fatal("解除后第 1 次停滞不应立即复熔断")
	}
}

// TestModelHangExpires 熔断到期自动解除，且过期条目会被清理（表不无限增长）。
func TestModelHangExpires(t *testing.T) {
	p := New("")
	p.NoteModelHang("cn", "m")
	p.NoteModelHang("cn", "m")

	p.mu.Lock()
	p.hangUntil[hangKey("cn", "m")] = time.Now().Add(-time.Second)
	p.mu.Unlock()

	if _, blocked := p.ModelHangBlocked("cn", "m"); blocked {
		t.Fatal("已过期的熔断不应继续拦截")
	}
	p.mu.Lock()
	_, still := p.hangUntil[hangKey("cn", "m")]
	p.mu.Unlock()
	if still {
		t.Fatal("过期条目应被顺手清理")
	}
}

// TestModelHangWindowPrunesOldObservations 观察窗外的旧停滞不参与累计。
func TestModelHangWindowPrunesOldObservations(t *testing.T) {
	p := New("")
	p.NoteModelHang("cn", "m")

	p.mu.Lock()
	p.hangSeen[hangKey("cn", "m")] = []time.Time{time.Now().Add(-hangWindow - time.Minute)}
	p.mu.Unlock()

	// 旧记录被裁掉，本次只算第 1 次 → 不熔断。
	p.NoteModelHang("cn", "m")
	if _, blocked := p.ModelHangBlocked("cn", "m"); blocked {
		t.Fatal("窗口外的旧停滞不应继续累计")
	}
}

// TestModelHangEmptyModelIgnored 无模型名不做任何统计（没有维度可归于）。
func TestModelHangEmptyModelIgnored(t *testing.T) {
	p := New("")
	p.NoteModelHang("cn", "")
	p.NoteModelHang("cn", "")
	if _, blocked := p.ModelHangBlocked("cn", ""); blocked {
		t.Fatal("空模型名不应熔断")
	}
	if got := p.HangTotal(); got != 0 {
		t.Fatalf("HangTotal=%d want 0", got)
	}
}

// TestHangStatuses 面板观测：只列仍在熔断期的行，按模型名排序。
func TestHangStatuses(t *testing.T) {
	p := New("")
	p.NoteModelHang("cn", "zeta")
	p.NoteModelHang("cn", "zeta")
	p.NoteModelHang("cn", "alpha")
	p.NoteModelHang("cn", "alpha")

	rows := p.HangStatuses()
	if len(rows) != 2 {
		t.Fatalf("熔断行数=%d want 2", len(rows))
	}
	if rows[0].Model != "alpha" || rows[1].Model != "zeta" {
		t.Fatalf("排序错误: %+v", rows)
	}
	for _, r := range rows {
		if r.BlockedUntil.IsZero() {
			t.Fatalf("%s 缺截止时刻", r.Model)
		}
	}
}
