package pool

import "testing"

// TestIsQuotaReasonCodeFamily 锁住「哪些模型级冷却码算日配额」这条口径。
//
// 为什么值得单独测：reason 是 applyErrorPolicy 用实际命中的码拼出来的
// （"6001 model rate limit" 这类字符串），而 isQuotaReason 只看前缀。扩族
// （6001–6008）之后如果这里还只认 6004/6000，6008（RPD，日请求数）会被当成
// 可自愈的瞬时窗口反复重试到次日，而 6001–6007（秒/分/小时级速率）反过来会被
// 当成日配额锁到次日——两个方向都是负优化。
func TestIsQuotaReasonCodeFamily(t *testing.T) {
	quota := []string{
		"6000 model rate limit",
		"6004 model rate limit",
		"6008 model rate limit",
	}
	for _, r := range quota {
		if !isQuotaReason(r) {
			t.Errorf("isQuotaReason(%q)=false want true（日级配额应锁到重置）", r)
		}
	}
	transient := []string{
		"6001 model rate limit", // TPS：秒级
		"6002 model rate limit", // TPM：分钟级
		"6003 model rate limit", // TPH：小时级
		"6005 model rate limit", // RPS
		"6006 model rate limit", // RPM
		"6007 model rate limit", // RPH
		"14003 model busy",
		"",
	}
	for _, r := range transient {
		if isQuotaReason(r) {
			t.Errorf("isQuotaReason(%q)=true want false（瞬时速率窗口不该按日配额处理）", r)
		}
	}
}
