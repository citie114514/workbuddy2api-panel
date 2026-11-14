package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestModelBlockTTLByCode 11158（无该模型权限）与 11102（服务信息缺失）必须用不同时长。
//
// 11102 可能是路径差异/抖动/误判，几分钟自愈 → 短退避；11158 是授权属性（套餐不含该
// 模型），按 2 小时一次反复探测只是往上游打注定失败的请求。两者的证据链不同，时长也
// 必须不同。
func TestModelBlockTTLByCode(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.BlockModelBackoff("u1", "model-noent", "11158 model not allowed for this account")
	p.mu.Lock()
	until58 := p.byUID["u1"].modelCooldowns["model-noent"].Until
	p.mu.Unlock()
	if d := time.Until(until58); d < 23*time.Hour || d > 25*time.Hour {
		t.Errorf("11158 负缓存 %v，应约 24h", d)
	}

	// 二次命中不得把 11158 继续拉长（它不是退避型，是固定时长）。
	p.BlockModelBackoff("u1", "model-noent", "11158 model not allowed for this account")
	p.mu.Lock()
	until58b := p.byUID["u1"].modelCooldowns["model-noent"].Until
	p.mu.Unlock()
	if d := time.Until(until58b); d > 25*time.Hour {
		t.Errorf("11158 重复命中被拉长到 %v，应保持固定 24h", d)
	}

	p.BlockModelBackoff("u1", "model-flaky", "11102 service info not found")
	p.mu.Lock()
	until02 := p.byUID["u1"].modelCooldowns["model-flaky"].Until
	p.mu.Unlock()
	if d := time.Until(until02); d < 10*time.Minute || d > 20*time.Minute {
		t.Errorf("11102 首次负缓存 %v，应约 15m", d)
	}
}
