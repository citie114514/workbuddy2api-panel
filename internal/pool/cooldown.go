// 冷却与熔断：Cooldown（固定时长账号级冷却）、CooldownSoftRate（账号级软冷却，对齐
// 上游重置时间或有界退避）、CooldownSoftForModel（模型级软冷却，对齐重置墙钟）、
// BlockModelBackoff/Clear（11102 负缓存）、软冷却封顶、熔断失败累计、签到解冻。
package pool

import (
	"log"
	"sort"
	"strings"
	"time"
)

func (p *Pool) SetCredits(uid string, credits, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
		e.creditsTotal = total
		p.dirty.Store(true)
	}
}

// SetNickname 更新账号昵称并回写 auths 凭证文件（issue #94：上游改名后同步）。
// 昵称未变化时不写盘；uid 不存在 / 昵称为空返回 false。与 token 刷新共用
// auth 自身的锁与 SaveAtomic 原子写，无半更新窗口。
func (p *Pool) SetNickname(uid, nickname string) bool {
	if uid == "" || nickname == "" {
		return false
	}
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok {
		return false
	}
	a := e.a
	a.Lock()
	changed := a.Nickname != nickname
	if changed {
		a.Nickname = nickname
	}
	a.Unlock()
	if !changed {
		return false
	}
	if err := a.SaveAtomic(); err != nil {
		log.Printf("WARN: [pool] nickname save %s: %v", uid, err)
		return false
	}
	return true
}

// NoteCheckinDone 标记账号今日已签到（签到成功与上游"今天已签到"幂等拒绝均算）。
// 记录本地日期，跨零点自然过期；不触碰冷却/禁用状态（签到与冷却域正交）。
func (p *Pool) NoteCheckinDone(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		day := time.Now().Format("2006-01-02")
		if e.lastCheckinDay != day {
			e.lastCheckinDay = day
			p.dirty.Store(true)
		}
	}
}

// CreditsSnapshot 一次余额查询的完整结果（含按日到期批次）。
// 调度器/面板把 upstream.UserResourceSnapshot 的输出转换成本结构后写入池。
type CreditsSnapshot struct {
	Remain            int64
	Total             int64
	Expiring          int64
	EarliestAt        time.Time
	EarliestRemaining int64
	ExpiryBuckets     []ExpiryBucket
}

// SetCreditsSnapshot 更新账号余额/总额、窗口内快过期子集、最早到期批次与按日到期批次。
// 有 buckets 时以 buckets 为准重算最早批次（同一天多个包合计）；无 buckets 时沿用
// 入参 earliestAt/earliestRemaining（兼容旧调用与测试）。所有数值钳到 [0, credits]，
// 避免上游脏数据污染选号。
func (p *Pool) SetCreditsSnapshot(uid string, s CreditsSnapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	credits := s.Remain
	if credits < 0 {
		credits = 0
	}
	expiring := s.Expiring
	if expiring < 0 {
		expiring = 0
	}
	if expiring > credits {
		expiring = credits
	}
	now := time.Now()
	buckets := sanitizeExpiryBuckets(s.ExpiryBuckets, now)
	earliestAt, earliestRemaining := s.EarliestAt, s.EarliestRemaining
	if len(buckets) > 0 {
		earliestAt = buckets[0].ExpireAt
		earliestRemaining = buckets[0].Remain
	}
	if earliestRemaining < 0 {
		earliestRemaining = 0
	}
	if earliestRemaining > credits {
		earliestRemaining = credits
	}
	if earliestAt.IsZero() || !earliestAt.After(now) || earliestRemaining == 0 {
		earliestAt = time.Time{}
		earliestRemaining = 0
	}
	e.credits = credits
	e.creditsTotal = s.Total
	e.creditsExpiring = expiring
	e.creditsEarliestExpiry = earliestAt
	e.creditsEarliestRemaining = earliestRemaining
	e.expiryBuckets = buckets
	p.dirty.Store(true)
}

// SetCreditsDetailed 旧入口：只有聚合余额与最早一批，没有按日批次。
// 保留给测试与只关心最早批次的调用方；生产路径统一走 SetCreditsSnapshot。
func (p *Pool) SetCreditsDetailed(uid string, credits, total, expiring int64, earliestAt time.Time, earliestRemaining int64) {
	p.SetCreditsSnapshot(uid, CreditsSnapshot{
		Remain:            credits,
		Total:             total,
		Expiring:          expiring,
		EarliestAt:        earliestAt,
		EarliestRemaining: earliestRemaining,
	})
}

// sanitizeExpiryBuckets 深拷贝并清洗：只保留有日期、未过期、正剩余的桶，按日期升序。
// ExpireAt 缺失时按日期补当天 00:00（本地时区），保证排序与最早批次仍可用。
func sanitizeExpiryBuckets(in []ExpiryBucket, now time.Time) []ExpiryBucket {
	if len(in) == 0 {
		return nil
	}
	today := now.Format("2006-01-02")
	out := make([]ExpiryBucket, 0, len(in))
	for _, b := range in {
		if b.Remain <= 0 || b.Date == "" || b.Date < today {
			continue
		}
		if b.ExpireAt.IsZero() {
			if t, err := time.ParseInLocation("2006-01-02", b.Date, time.Local); err == nil {
				b.ExpireAt = t
			}
		}
		if b.ExpireAt.IsZero() || !b.ExpireAt.After(now) {
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].ExpireAt.Before(out[j].ExpireAt)
	})
	return out
}

// ClearExpiringSnapshots 清空所有账号的快过期/最早到期缓存。配置窗口改变时调用，
// 避免在新快照写入前继续使用旧窗口得到的路由数据；下一次签到或余额刷新会重建。
func (p *Pool) ClearExpiringSnapshots() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.byUID {
		e.creditsExpiring = 0
		e.creditsEarliestExpiry = time.Time{}
		e.creditsEarliestRemaining = 0
	}
	p.dirty.Store(true)
}

// Cooldown 冷却账号至 now+d（即时冷却：CoolHard 余额耗尽 / CoolSoft 固定短冷却）。
//
// 重构后本入口是「固定时长的账号级冷却」，不再做两件旧事：
//   - 不再喂熔断器失败计数：熔断器只对「反复失败」（NoteError，5xx）退避。
//     软限流/余额耗尽各有权威恢复时刻（重置墙钟 / 04:00 签到），再并入"连续失败"
//     会让用户正常重试越堆越厚。熔断语义由 NoteError 唯一驱动（与 until 正交保持）。
//   - 不再做 softStreak 指数堆加：固定 d 即最终时长。CoolSoft 的精确对齐请用
//     CooldownSoftRate（有界、对齐上游重置时间）。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.coolKind = kind
		e.reason = reason
		// 不清 modelCooldowns：账号级冷却与模型级冷冻是**正交**维度。
		//   - 账号级冷却期间 healthyForModel 末尾的 e.healthy(now) 已保证整号不可选，
		//     modelExempt 也显式判了 !e.until.IsZero() → 不存在「模型豁免泄漏」，
		//     当初加清空的理由已不成立；
		//   - 反之清掉会让「锁到上游重置墙钟」的承诺提前失效：账号在别的模型上吃一次
		//     429，本模型的 6004/6000 冷冻就被一起抹掉，账号级冷却到期后立刻再撞同一
		//     模型的限流墙（多号池里表现为「提示 15:22 恢复，2h 后又撞一次」）。
		p.dirty.Store(true)
	}
}

// CooldownSoftForModel 429 的模型级软冷却入口。
// 非空 model 只写 modelCooldowns：明确 resetAt 完整保留，不受 softRateMax 截断；
// 无墙钟时按该模型 Hits 做有界退避。空模型无法建立路由键，退回账号级软冷却。
func (p *Pool) CooldownSoftForModel(uid string, base time.Duration, resetAt time.Time, model, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		now := time.Now()
		if model != "" {
			if e.modelCooldowns == nil {
				e.modelCooldowns = map[string]modelCooldown{}
			}
			if !resetAt.IsZero() {
				until := resetAt
				if !until.After(now) {
					until = now.Add(time.Millisecond)
				}
				e.modelCooldowns[model] = modelCooldown{Until: until, ResetAt: resetAt, Reason: reason}
			} else {
				hits := e.modelCooldowns[model].Hits + 1
				e.modelCooldowns[model] = modelCooldown{
					Until:  now.Add(p.softDurationLocked(base, hits)),
					Reason: reason,
					Hits:   hits,
				}
			}
		} else {
			// 空模型无法建立独立路由键，退回账号级冷却。
			if !resetAt.IsZero() {
				e.until = p.cappedSoftUntilLocked(now, resetAt)
			} else if e.coolKind != CoolSoft || !now.Before(e.until) {
				d := p.softDurationLocked(base, e.softStreak+1)
				e.softStreak++
				e.until = now.Add(d)
			}
			e.coolKind = CoolSoft
			e.reason = reason
			// 与 Cooldown 同口径：账号级冷却不清模型级冷冻（正交维度）。
		}
		p.dirty.Store(true)
	}
}

const (
	modelBusyBase = 10 * time.Second
	modelBusyMax  = time.Minute
)

// CooldownModelBusy 记录 14003 的 (账号,模型) 暂时性容量避让。
// 默认 10 秒起、最多 1 分钟；有效窗口内重复命中不持续延长。显式 Retry-After
// 也在 1 分钟短窗口内截断。已有更晚的模型冷却不被覆盖或降级成容量类型。
func (p *Pool) CooldownModelBusy(uid, model string, retryAfter time.Duration, reason string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	old, exists := e.modelCooldowns[model]
	if exists && !old.AuditOnly && old.Until.After(now) {
		return
	}
	if retryAfter > 0 {
		if retryAfter > modelBusyMax {
			retryAfter = modelBusyMax
		}
	} else {
		hits := old.Hits + 1
		retryAfter = modelBusyBase
		if hits > 1 {
			shift := hits - 1
			if shift > 3 {
				shift = 3
			}
			retryAfter <<= uint(shift)
			if retryAfter > modelBusyMax {
				retryAfter = modelBusyMax
			}
		}
		old.Hits++
	}
	if e.modelCooldowns == nil {
		e.modelCooldowns = map[string]modelCooldown{}
	}
	e.modelCooldowns[model] = modelCooldown{
		Until: now.Add(retryAfter), Reason: reason, Hits: old.Hits, Capacity: true,
	}
	p.dirty.Store(true)
}

// RecordModelRateLimitAudit 记录无法参与模型路由的 6004 展示项。
// 典型场景是 6004 没有可解析重置时间：账号仍按原有有界退避冷却，
// 本方法只把模型名挂到 e.until 上供账号页展示，不影响 healthyForModel。
func (p *Pool) RecordModelRateLimitAudit(uid, model, reason string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	if old, exists := e.modelCooldowns[model]; exists && !old.AuditOnly && old.Until.After(now) {
		return // 已有真实模型冷却，审计记录不得覆盖路由截止
	}
	until := e.until
	if until.IsZero() || !until.After(now) {
		until = now.Add(p.softRateMaxOr())
	}
	if e.modelCooldowns == nil {
		e.modelCooldowns = make(map[string]modelCooldown)
	}
	e.modelCooldowns[model] = modelCooldown{
		Until:     until,
		Reason:    reason,
		AuditOnly: true,
	}
	p.dirty.Store(true)
}

// modelBlock TTL 常量（11102/11158 负缓存退避）：首次 15m，之后 base × 2^min(hits-1, 4)，
// 封顶 2h。该模型请求成功即由 BlockModelClear 清除。
//
// 为什么从 6h/24h 收到 15m/2h：11102「service info not found」**不总是**「这个号真没有
// 这个模型」——路径级差异、上游瞬时抖动、分类误判都会给同一个码，社区实测同一账号两分钟
// 就恢复。旧参数把一次误判放大成 (账号,模型) 锁死 6 小时：多号池里几个号先后误判，就是
// 该模型整池 400/503 好几个小时，且 ModelBlockedForRealm 会让客户端看到
// model_unavailable（"换个模型"）——这个错误结论比等十五分钟贵得多。
//
// 为什么不更短（曾试 2m/10m）：对**真的没有这个模型**的号，每次探测都是一轮白撞的 400。
// 2m/10m 等于稳态每小时探 6 次（一天 144 次），几个号一起就是几百次噪音轮转；15m 把
// 这项成本压到 1/12（一天 12 次），而误判自愈只比 2 分钟慢 13 分钟。上限 2h 同理：
// 既要跟上模型目录/权限变化（上游放开权限后最多 2 小时就能用上），又不至于一天只探一次。
//
// shift=4 保留：15m × 16 = 4h > 2h，阶梯仍能走到封顶。
const (
	modelBlockBaseTTL = 15 * time.Minute
	modelBlockShift   = 4
	modelBlockMaxTTL  = 2 * time.Hour

	// modelBlockAuthTTL 11158「该账号类型无此模型权限」的固定 TTL。
	//
	// 与 11102 分开：11102 可能是路径差异/上游抖动/分类误判，几分钟就能自愈，所以
	// 用短退避；11158 是**授权属性**（账号套餐不含该模型），短时间内不会变，按退避
	// 反复探测只是每 2 小时往上游打一次注定失败的请求——既占轮转名额，也是一条可
	// 观测的重复失败。
	//
	// 取 24h 而不是永久：套餐/权限会变（续费、活动放开），永久负缓存会让账号在权限
	// 恢复后再也用不上这个模型，只能人工干预。一天一次探测是成本与自愈速度的折中。
	modelBlockAuthTTL = 24 * time.Hour
)

// BlockModelBackoff 11102「该后端无此模型」的 (账号, 模型) 负缓存入口
// （handler.applyErrorPolicy 调用）。复用 modelCooldowns 机制（不新建平行状态）：
// 写 modelCooldowns[model]，Until 为指数退避 TTL，选号侧 healthyForModel 自动对该
// 账号避开该模型。
//
// 语义与 6004 正交：6004 是「模型被限流、对齐重置墙钟」，本入口是「官方确定该后端
// 无此模型、重试无意义，只能换模型/换账号」。resetAt 无需传（11102 无重置文案），
// ResetAt 保持零值，与 6004 台账共用 Until 判定——11102 条目会以 11102 reason 出现在
// /status 台账，运维可见。
func (p *Pool) BlockModelBackoff(uid, model, reason string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	hits := 0
	if e.modelCooldowns != nil {
		hits = e.modelCooldowns[model].Hits
	}
	hits++
	ttl := modelBlockTTL(reason, hits)
	if e.modelCooldowns == nil {
		e.modelCooldowns = map[string]modelCooldown{}
	}
	e.modelCooldowns[model] = modelCooldown{
		Until:  now.Add(ttl),
		Reason: reason,
		Hits:   hits,
	}
	p.dirty.Store(true)
}

// modelBlockTTL 按码给负缓存时长：11158 是授权属性（固定 24h），11102 按退避阶梯
// （首次 15m，翻倍到 2h 封顶）。
func modelBlockTTL(reason string, hits int) time.Duration {
	if strings.HasPrefix(reason, "11158") {
		return modelBlockAuthTTL
	}
	ttl := modelBlockBaseTTL
	if d := ttl * (1 << uint(min(hits-1, modelBlockShift))); d < modelBlockMaxTTL {
		return d
	}
	return modelBlockMaxTTL
}

// modelBlockMaxTTLFor 该码的负缓存上限（持久化恢复时的存量钳制用）。
func modelBlockMaxTTLFor(reason string) time.Duration {
	if strings.HasPrefix(reason, "11158") {
		return modelBlockAuthTTL
	}
	return modelBlockMaxTTL
}

// modelBlockReason 报告该模型冷却是否属于「确定性拒绝」负缓存（11102/11158）。
// BlockModelClear（成功即清）与持久化恢复（存量 TTL 钳制）共用同一口径，
// 避免两处各判一套前缀、改一个漏一个。
func modelBlockReason(reason string) bool {
	return strings.HasPrefix(reason, "11102") || strings.HasPrefix(reason, "11158")
}

// BlockModelClear 清除 (账号, 模型) 的确定性模型拒绝负缓存（11102/11158；该模型
// 实测又通了）。半开探测或正常请求对该模型成功后调用（handler 成功路径）。只清
// 这两类条目、不碰 6004 独立冷却表——6004 有自身上游重置墙钟语义，成功不该抹掉。
func (p *Pool) BlockModelClear(uid, model string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || len(e.modelCooldowns) == 0 {
		return
	}
	mc, exists := e.modelCooldowns[model]
	if !exists || !modelBlockReason(mc.Reason) {
		return
	}
	delete(e.modelCooldowns, model)
	if len(e.modelCooldowns) == 0 {
		e.modelCooldowns = nil
	}
	p.dirty.Store(true)
}

// ModelBlockStatus 描述某模型在全池范围内因模型级冷却而不可选的情况。
type ModelBlockStatus struct {
	Blocked  bool      // true = 每个非禁用账号都对该模型处于冷却中
	Reason   string    // 冷却原因（通常为上游原文，如 "11102 model ... not found"）
	Until    time.Time // 最早解封时间（零值 = 上游未给重置时刻）
	Count    int       // 因此被挡的账号数
	Capacity bool      // true = 所有阻塞均为 14003 暂时性容量窗口，可 503 重试
	// QuotaOnly true = 所有阻塞原因都是用量配额（6004/6000）。配额锁在重置前重试
	// 必然同样失败，是"限流"语义而不是"参数/模型不可用"，故按 429 返回且**不带**
	// Retry-After——带上会被某些客户端当成"睡多久"的指令，把 20 分钟锁变成 20 分钟
	// 静默挂起，比报错更难排查。
	QuotaOnly bool
}

// isQuotaReason 报告模型级冷却原因是否属于**日级用量配额**（6000 / 6004 TPD /
// 6008 RPD）。原因字符串由 applyErrorPolicy 写入（"6004 model rate limit" 等）。
//
// 只收日级码：6001/6002/6003/6005/6006/6007 是秒/分/小时级瞬时速率窗口，
// 几分钟就能自愈，按日配额处理会把模型锁到次日，属于过度冷却。
func isQuotaReason(reason string) bool {
	reason = strings.TrimSpace(reason)
	return strings.HasPrefix(reason, "6004") ||
		strings.HasPrefix(reason, "6008") ||
		strings.HasPrefix(reason, "6000")
}

// ModelBlocked 报告该模型是否在全池范围内被模型级冷却挡住。
//
// 为什么需要它：选号失败时客户端只会拿到 no_healthy_account（"没有可用账号"），
// 但真实原因常常是「号都在、只是都对这个模型关闭」。两者对调用方的处置完全不同
// ——前者该等，后者换个模型才有用——此前却无法区分：首次请求还能看到上游原文
// （lastErr 非空），一旦负缓存写入，后续请求 lastErr 为空，就只剩"池子没号"
// （issue #102 附带发现 1）。上游原文与解封时间在那里被丢掉。
//
// 跨 realm 判定：调用方失败前已依次尝试过各域，所以只要有**任意**账号还能服务该
// 模型，就不能算全池阻塞 —— 此时返回 Blocked=false，让上层继续用原有的
// no_healthy_account 文案（选号失败另有原因：在途占满/积分保底/账号级冷却）。
//
// 口径必须与选号一致：用 modelCooled 而非直接查 map，这样 AuditOnly 条目
// （只审计不拦截）不会被误报成阻塞。
func (p *Pool) ModelBlocked(model string) ModelBlockStatus {
	return p.ModelBlockedForRealm(model, "")
}

// ModelBlockedForRealm 与 ModelBlocked 同口径，但只统计指定 realm 的可选账号。
// 模型请求已按 realm 分池时（如 global:gpt-5.6-sol 只走 global 账号），另一域的
// 未冷却账号不构成“还能服务”的证明，否则会把本域全锁误报成 no_healthy_account。
func (p *Pool) ModelBlockedForRealm(model, realm string) ModelBlockStatus {
	if model == "" {
		return ModelBlockStatus{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	st := ModelBlockStatus{Capacity: true, QuotaOnly: true}
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		// 暂停选号（paused）号与禁用号同口径跳过：它此刻不可选，「能服务」的
		// 证明不成立——否则一个暂停的健康号会掩盖「其余号全被模型级冷却挡住」。
		if e.disabled || e.paused {
			continue // 禁用/暂停号不参与：它们的不可用与模型无关
		}
		if !e.modelCooled(now, model) {
			// 还有账号能服务这个模型 → 不是模型级阻塞。
			return ModelBlockStatus{}
		}
		st.Count++
		if mc, ok := e.modelCooldowns[model]; ok {
			if !mc.Capacity {
				st.Capacity = false
			}
			if !isQuotaReason(mc.Reason) {
				st.QuotaOnly = false
			}
			if st.Reason == "" {
				st.Reason = mc.Reason
			}
			// 取最早解封：那才是"再等多久值得重试"的答案。
			if !mc.Until.IsZero() && (st.Until.IsZero() || mc.Until.Before(st.Until)) {
				st.Until = mc.Until
			}
		}
	}
	if st.Count == 0 {
		// 池里压根没有非禁用账号：这是"真的没号"，不是模型问题。
		return ModelBlockStatus{}
	}
	st.Blocked = true
	return st
}

// CooldownSoftRate 429/限流文案的**账号级**软冷却入口（handler.applyErrorPolicy 调用）。
//
// 语义：
//   - resetAt 非零（上游带权威重置时间，无论 6004 还是 11140 rate-limiting）→
//     账号级直到该墙钟（截断到 softRateMax，绝不指数堆加）；**不**在
//     modelCooldowns 记模型（账号级语义，不产生切模型豁免——普通账号级限流不该
//     因切模型绕过）。
//   - resetAt 零值且**不在冷却中**（首次/恢复后的新限流）→ 有界退避：按 softStreak
//     指数退避并封顶 softRateMax。softStreak 只在真正进入一次新冷却时计数，由
//     NoteSuccess/reviveCoolingLocked 清零（既有恢复语义）。
//   - resetAt 零值且**已在软冷却中**（兜底探测再次撞 429）→ 不推进 streak、不延长
//     until：用户重试/并发兜底探测不得把冷却越堆越厚——这正是旧实现「越重试越冷、
//     全池被推到 2h 封顶」的元凶（每次探测都 softStreak++ 指数翻倍）。
func (p *Pool) CooldownSoftRate(uid string, base time.Duration, resetAt time.Time, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		now := time.Now()
		if !resetAt.IsZero() {
			e.until = p.cappedSoftUntilLocked(now, resetAt)
		} else if e.coolKind != CoolSoft || !now.Before(e.until) {
			// 新限流（不在有效软冷却中）：推进有界退避；兜底探测（仍在软冷却中）不翻倍。
			d := p.softDurationLocked(base, e.softStreak+1)
			e.softStreak++
			e.until = now.Add(d)
		}
		e.coolKind = CoolSoft
		e.reason = reason
		// 与 Cooldown 同口径：账号级软冷却不清模型级冷冻（正交维度）。
		p.dirty.Store(true)
	}
}

// cappedSoftUntilLocked 把上游重置墙钟截断到 softRateMax（now+softRateMax 与 resetAt
// 取较早者）。resetAt 已过期（时钟偏移/文案过期）时时长钳到时间零点附近，立即恢复。
// 调用方必须已持有 p.mu。
func (p *Pool) cappedSoftUntilLocked(now, resetAt time.Time) time.Time {
	cap := now.Add(p.softRateMaxOr())
	if resetAt.After(cap) {
		return cap
	}
	if resetAt.After(now) {
		return resetAt
	}
	return now.Add(time.Millisecond)
}

// softRateMaxOr 返回生效的 softRateMax（未注入时按默认 2h），供封顶计算。
// 调用方必须已持有 p.mu。
func (p *Pool) softRateMaxOr() time.Duration {
	if p.softRateMax > 0 {
		return p.softRateMax
	}
	return defaultSoftRateMax
}

// softDurationLocked 按连续软冷却次数把基数 d 指数放大：d << (streak-1)，封顶 softRateMax。
// softRateMax 未注入（<=0）时按 defaultSoftRateMax 算。streak<=1 时原样返回 d。
// 左移位数受 softStreakShiftMax 限制，避免 streak 极大时移位溢出。
// 调用方必须已持有 p.mu。
func (p *Pool) softDurationLocked(d time.Duration, streak int) time.Duration {
	if streak <= 1 {
		return d
	}
	shift := streak - 1
	if shift > softStreakShiftMax {
		shift = softStreakShiftMax
	}
	d <<= shift
	max := p.softRateMax
	if max <= 0 {
		max = defaultSoftRateMax
	}
	if d > max || d <= 0 { // d<=0：左移溢出成负数/零，同样按封顶兜底
		d = max
	}
	return d
}

// recordBreakerFailureLocked 累计一次熔断失败；达到阈值则按指数退避熔断。
// 熔断与冷却（until）解耦：冷却按错误类别给固定时长，熔断则对"反复失败"逐次加长封禁。
// 调用方必须已持有 p.mu。
func (p *Pool) recordBreakerFailureLocked(e *entry) {
	e.fails++
	if e.fails < p.breakerThreshold {
		return
	}
	d := p.breakerCooldown
	for i := 0; i < e.retryCount; i++ {
		d *= 2
		if d >= p.breakerCooldownMax {
			d = p.breakerCooldownMax
			break
		}
	}
	// 触发熔断：重置失败计数供下一轮重新累计；retryCount 递增放大退避指数。
	e.fails = 0
	e.retryCount++
	e.breakerUntil = time.Now().Add(d)
}

// CooldownUntilTomorrow4AM 冷却到下一个 04:00（本地时区）。
// 用于 ErrHardCredit 场景：积分耗尽账号等签到任务（09:00/21:00）恢复。
func (p *Pool) CooldownUntilTomorrow4AM(uid string, reason string) {
	now := time.Now()
	p.Cooldown(uid, CoolHard, nextDay4AM(now).Sub(now), reason)
}

// nextDay4AM 返回 now 之后最近的一个 04:00（与 now 同一时区）。
// now 在当天 04:00 之前（凌晨 00:00~04:00）时返回当天 04:00——此时签到尚未执行，
// 该窗内触发的硬冷却等当天签到即可恢复；返回次日会白冷约一天。
// 04:00 整及之后返回次日 04:00。
// time.Date 对日溢出自动进位（月末→下月 1 号、年末→下年 1 号），天然覆盖跨日/跨月/跨年。
func nextDay4AM(now time.Time) time.Time {
	if now.Hour() < 4 {
		return time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	}
	return time.Date(now.Year(), now.Month(), now.Day()+1, 4, 0, 0, 0, now.Location())
}

// ReenableIfCredits 签到/余额刷新后解冻：仅当 remain > 0 且账号非禁用时，解冻
// **余额耗尽冷却**（CoolHard）。软限流（CoolSoft）与模型级台账（modelCooldowns）
// 不在此清除——它们的恢复证据是上游重置墙钟到期，不是余额恢复（余额刷新周期
// 任务每 5 分钟到达这里，全清会把限流冷却实际寿命压到一个刷新周期内）。
// 注意：不碰熔断器——熔断到期（breakerUntil 过期）或下次 chat 成功（NoteSuccess）才恢复。
// reviveCoolingLocked 已迁至 transition.go（状态机迁移唯一权威实现）。
