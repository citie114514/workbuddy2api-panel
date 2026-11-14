// humanize.go 让周期性任务的行为节拍不像机器。
//
// 背景：同一出口 IP 上全部账号以完全一致的机械节拍活动，是最容易被识别的特征。
// 上游复盘过一次集中封禁（池内多个账号在数分钟内同时被判「存在异常行为」），日志
// 显示余额刷新精确每 5 分钟整点全账号齐射、补领清扫精确每 15 分钟整点、夜猫子
// 23 点全池同时发起真实对话。真人不会这样活动。
//
// 这里只提供两个纯函数与两个薄封装，默认值全部为「不改变现有行为」：
//   - schedule.balance_refresh_jitter_permill / task_sweep_jitter_permill（0–500，缺省 0）
//     让每轮间隔在 ±(值/1000)×基准 内随机偏移，不再精确打卡；
//   - schedule.night_account_cap（缺省 0 = 不限）把夜间真实对话收窄到积分最高的前 N 个
//     账号，其余账号夜里静默。白天任务（签到/旅行/成长）仍全池保号，不受影响。
package scheduler

import (
	"math/rand"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// jitterPermillMax 抖动幅度的上限（500 = ±50%）。再大就不是"错峰"而是改变任务节奏，
// 低频任务的间隔会漂移到不可预期。
const jitterPermillMax = 500

// jitteredInterval 给基准间隔施加 ±(permill/1000) 的随机偏移。
//
// permill<=0 原样返回（缺省关闭，老行为逐字不变）；越界钳到上限；结果不小于 1 秒，
// 避免配置极高抖动时把 Ticker 类逻辑压到 0 导致忙等。
//
// randInt63n 由调用方注入，测试可给确定性源；生产传 rand.Int63n 即可（全局随机源
// 在 Go 1.20+ 自动播种且并发安全）。
func jitteredInterval(base time.Duration, permill int, randInt63n func(int64) int64) time.Duration {
	if base <= 0 || permill <= 0 || randInt63n == nil {
		return base
	}
	if permill > jitterPermillMax {
		permill = jitterPermillMax
	}
	// span = base × permill / 1000，取值区间 [base-span, base+span)。
	span := int64(base) * int64(permill) / 1000
	if span <= 0 {
		return base
	}
	delta := randInt63n(2*span) - span
	d := base + time.Duration(delta)
	if d < time.Second {
		d = time.Second
	}
	return d
}

// capByCredits 按积分从高到低取前 n 个账号，n<=0 或覆盖全部时原样返回。
//
// 用途是夜猫子这类「整任务都是真实模型对话」的活动收窄参与面：夜间只有少数账号
// 在活动，与真人的作息一致。排序不改动入参切片，同分保持原有顺序（稳定排序），
// 避免同一批账号每次都被重排成不同集合。
func capByCredits(list []pool.Status, n int) []pool.Status {
	if n <= 0 || len(list) <= n {
		return list
	}
	idx := make([]int, len(list))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return list[idx[a]].Credits > list[idx[b]].Credits
	})
	out := make([]pool.Status, 0, n)
	for _, i := range idx[:n] {
		out = append(out, list[i])
	}
	return out
}

// scheduleRand 生产用随机源（全局源：自动播种、并发安全）。
func scheduleRand(n int64) int64 { return rand.Int63n(n) }
