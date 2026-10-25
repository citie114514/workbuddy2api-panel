package pool

// modelfallback_test.go v1.15 模型付费回退的目标解析单测。
// 端到端（选号改写/只回退一次/禁用开关）在 internal/server 侧。

import (
	"testing"
)

func TestModelFallbackDisabled(t *testing.T) {
	p := New("")
	p.SetModelFallback(false, map[string]string{"global:m": "m-sg"})
	if fb, ok := p.ModelFallbackTarget("global", "m"); ok {
		t.Errorf("disabled must never fall back, got %q", fb)
	}
}

func TestModelFallbackBuiltinSgRule(t *testing.T) {
	p := New("")
	p.SetModelFallback(true, nil)
	// 内置规则要求目录里确实存在 "-sg" 型号（modelRateOf 有倍率条目），
	// 不存在的型号不瞎撞。
	p.SetModelRateOf(func(realm, model string) string {
		if realm == "global" && model == "deepseek-v4.1-flash-sg" {
			return "x0.03"
		}
		return ""
	})
	fb, ok := p.ModelFallbackTarget("global", "deepseek-v4.1-flash")
	if !ok || fb != "deepseek-v4.1-flash-sg" {
		t.Errorf("builtin rule: fb=%q ok=%v, want deepseek-v4.1-flash-sg/true", fb, ok)
	}
	// 目录里没有 "-sg" → 不回退。
	if _, ok := p.ModelFallbackTarget("global", "other-model"); ok {
		t.Error("unknown sibling must not fall back")
	}
	// 已是 -sg 型号 → 不再派生 "-sg-sg"。
	if _, ok := p.ModelFallbackTarget("global", "deepseek-v4.1-flash-sg"); ok {
		t.Error("-sg model must not fall back again")
	}
	// 不同 realm 的目录条目不算数。
	p.SetModelRateOf(func(realm, model string) string {
		if realm == "cn" && model == "m-sg" {
			return "x0.01"
		}
		return ""
	})
	if _, ok := p.ModelFallbackTarget("global", "m"); ok {
		t.Error("sibling in another realm must not be used")
	}
}

func TestModelFallbackCustomMap(t *testing.T) {
	p := New("")
	p.SetModelFallback(true, map[string]string{
		"global:a": "a-paid",
		"global:b": "",  // 显式禁用
		"global:c": "c", // 指向自身 = 禁用
	})
	p.SetModelRateOf(func(realm, model string) string { return "x0.01" }) // 全目录存在
	if fb, ok := p.ModelFallbackTarget("global", "a"); !ok || fb != "a-paid" {
		t.Errorf("custom map: fb=%q ok=%v, want a-paid/true", fb, ok)
	}
	if _, ok := p.ModelFallbackTarget("global", "b"); ok {
		t.Error("empty custom value must disable fallback")
	}
	if _, ok := p.ModelFallbackTarget("global", "c"); ok {
		t.Error("self-pointing custom value must disable fallback")
	}
	// 自定义映射优先于内置规则：b 即使有 "-sg" 目录条目也被显式禁用拦截。
	if _, ok := p.ModelFallbackTarget("global", "b"); ok {
		t.Error("custom disable must override builtin rule")
	}
	// 未命中的模型仍走内置规则。
	fb, ok := p.ModelFallbackTarget("global", "deepseek-v4.1-flash")
	if !ok || fb != "deepseek-v4.1-flash-sg" {
		t.Errorf("unmapped model must use builtin rule, got %q ok=%v", fb, ok)
	}
}
