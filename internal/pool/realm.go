// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"math"
	"sort"
	"time"
)

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// ServableForModelRealm 返回 realm 内此刻**真正能服务该模型**的账号数。
//
// 与 pick 的候选口径一致（healthyForModel 的 6004/11102 模型级冷却豁免、在途租约、
// 积分保底都计入），但**不含 pick 的全冷却兜底**——跨域回退（issue #135）要的正是
// 「这个域此刻有没有健康号」，兜底把冷却号捞回来的乐观语义会掩盖这一点。
//
// 只读：持 RLock 遍历，不改任何状态。realm=="" 时统计全池（与既有 realm 语义一致）。
func (p *Pool) ServableForModelRealm(model, realm string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	n := 0
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		if p.floorBlockedForRealmModel(e, model, realm, now) {
			continue
		}
		n++
	}
	return n
}

// WeightedAvailableUIDsForModelRealm 返回带虚拟实例权重的可用账号列表。
//
// 普通账号出现 1 次；窗口内有快过期积分的账号按其紧迫度名次出现 8/6/5/4/3 次
// （第 6 名及以后 3 次）。调用方继续按原有序列表哈希，即可让新会话把流量优先分给
// 最急的几个号。重复项按 UID 排序后展开，保证同一账号拓扑下不同进程得到一致列表。
//
// 该方法是现有 AvailableUIDsForModelRealm 的增量入口，不改变旧方法语义，也不
// 修改配置、状态或 Redis schema。prefer_expiring=false 时退化为逐账号一次。
func (p *Pool) WeightedAvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	// 与选号侧共用同一份名次（按域算、只看池内状态），使「新会话分给谁」和
	// 「老会话换号给谁」对同一批快过期账号给出同样的先后——两套口径各算一套排名
	// 会让新会话集中到第 1 名、换号却抽到第 5 名。
	expRank := map[string]int(nil)
	if p.preferExpiring {
		expRank = p.expiringRankLocked(now, p.expiringWindowLocked(now, realm), realm)
	}
	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		slots := int(math.Round(expiringSlots(expRank[uid])))
		if slots < 1 {
			slots = 1
		}
		for i := 0; i < slots; i++ {
			out = append(out, uid)
		}
	}
	return out
}

// StickyEligibleForModelRealm 判断已有绑定能否保留：只看账号与模型健康，
// 不看在途名额。新会话分配仍使用 AvailableUIDsForModelRealm。
func (p *Pool) StickyEligibleForModelRealm(uid, model, realm string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok || (realm != "" && e.a.Realm() != realm) {
		return false
	}
	return e.stickyHealthyForModel(time.Now(), model)
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}
