package server

// proto_reasoning_test.go v1.16.0：reasoning / 缓存计量在两条协议路径上的透出。
//
// 背景：chat 路径上游什么都给（reasoning_content、completion_thinking_tokens、
// prompt_cache_*），适配层此前把它们吞了 —— 三路对照实测定位（计量失真）：
//   - /v1/messages：thinking 块（thinking_delta + signature 占位）+ usage 缓存字段
//   - /v1/responses：reasoning item（受请求 reasoning.summary 控制）+ usage details
//   - /v1/chat/completions 不动（已经是全的）
//
// 断言一律走解析后的事件 map（Go map 序列化按键排序，子串顺序匹配会误判）。

import (
	"encoding/json"
	"strings"
	"testing"
)

// reasoningTestFrames 思维链 + 正文 + 尾帧 usage（含缓存与 thinking 计量）。
var reasoningTestFrames = []string{
	`{"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"step one"}}]}`,
	`{"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":" step two"}}]}`,
	`{"id":"c1","choices":[{"index":0,"delta":{"content":"answer"}}]}`,
	`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"prompt_cache_hit_tokens":34,"prompt_cache_write_tokens":3,"completion_tokens":15,"completion_thinking_tokens":10,"completion_tokens_details":{"reasoning_tokens":10},"total_tokens":55}}`,
}

// runReasoningStream 跑完一整条流（Frame ×N + Close），返回事件序列。
func runReasoningStream(tr chatStreamTranslator) []sseEvent {
	var events []sseEvent
	for _, f := range reasoningTestFrames {
		events = append(events, tr.Frame([]byte(f))...)
	}
	return append(events, tr.Close()...)
}

// evMap 断言辅助：事件 data 必须是 JSON 对象。
func evMap(t *testing.T, ev sseEvent) map[string]any {
	t.Helper()
	m, ok := ev.data.(map[string]any)
	if !ok {
		t.Fatalf("event %s data not an object: %v", ev.name, ev.data)
	}
	return m
}

// TestAnthropicStreamThinking messages 流式：reasoning_content → thinking 块
//（thinking_delta 增量 + 关块时 signature_delta 占位），正文前自动关块。
func TestAnthropicStreamThinking(t *testing.T) {
	events := runReasoningStream((anthropicAdapter{}).NewStream("glm-5.2"))
	sawThinkingStart, sawThinkingDelta, sawSignature := false, false, false
	thinkingStartIdx, textStartIdx, stopBeforeText := -1, -1, false
	for i, ev := range events {
		if ev.name != "content_block_start" && ev.name != "content_block_delta" && ev.name != "content_block_stop" {
			continue
		}
		m := evMap(t, ev)
		switch ev.name {
		case "content_block_start":
			cb, _ := m["content_block"].(map[string]any)
			typ, _ := cb["type"].(string)
			switch typ {
			case "thinking":
				sawThinkingStart = true
				thinkingStartIdx = i
			case "text":
				sawTextStart = true
				textStartIdx = i
				if thinkingStartIdx >= 0 {
					for _, prev := range events[thinkingStartIdx+1 : i] {
						if prev.name == "content_block_stop" {
							stopBeforeText = true
						}
					}
				}
			}
		case "content_block_delta":
			d, _ := m["delta"].(map[string]any)
			dt, _ := d["type"].(string)
			if dt == "thinking_delta" && d["thinking"] == "step one" {
				sawThinkingDelta = true
			}
			if dt == "signature_delta" {
				sawSignature = true
			}
		}
	}
	if !sawThinkingStart {
		t.Error("thinking content_block_start missing")
	}
	if !sawThinkingDelta {
		t.Error("thinking_delta with upstream text missing")
	}
	if !sawSignature {
		t.Error("signature_delta placeholder missing")
	}
	if !sawTextStart {
		t.Error("text content_block_start missing")
	}
	if !stopBeforeText {
		t.Error("thinking block must be closed before text block opens")
	}
}

// TestAnthropicStreamUsageCache messages 流式：message_delta usage 带缓存计量，
// output_tokens 透传 completion（实测口径已含 thinking）。
func TestAnthropicStreamUsageCache(t *testing.T) {
	tr := (anthropicAdapter{}).NewStream("glm-5.2")
	var last map[string]any
	for _, ev := range runReasoningStream(tr) {
		if ev.name == "message_delta" {
			last = evMap(t, ev)
		}
	}
	usage, _ := last["usage"].(map[string]any)
	if usage == nil {
		t.Fatalf("message_delta usage missing: %v", last)
	}
	if asNum(t, usage["output_tokens"]) != 15 {
		t.Errorf("output_tokens=%v want 15", usage["output_tokens"])
	}
	if asNum(t, usage["cache_read_input_tokens"]) != 34 {
		t.Errorf("cache_read_input_tokens=%v want 34", usage["cache_read_input_tokens"])
	}
	if asNum(t, usage["cache_creation_input_tokens"]) != 3 {
		t.Errorf("cache_creation_input_tokens=%v want 3", usage["cache_creation_input_tokens"])
	}
}

// TestAnthropicNonStreamThinking 非流式：thinking 块先于 text 块，usage 带缓存字段。
func TestAnthropicNonStreamThinking(t *testing.T) {
	chat := `{"id":"c9","model":"glm-5.2","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"deep thought","content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"prompt_cache_hit_tokens":34,"prompt_cache_write_tokens":3,"completion_tokens":15,"completion_thinking_tokens":10,"total_tokens":55}}`
	obj, _ := (anthropicAdapter{}).JSONResponse([]byte(chat)).(map[string]any)
	content, _ := obj["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content=%v want [thinking, text]", obj["content"])
	}
	th, _ := content[0].(map[string]any)
	if th["type"] != "thinking" || th["thinking"] != "deep thought" {
		t.Errorf("thinking block: %v", content[0])
	}
	usage, _ := obj["usage"].(map[string]any)
	if asNum(t, usage["cache_read_input_tokens"]) != 34 || asNum(t, usage["cache_creation_input_tokens"]) != 3 {
		t.Errorf("usage cache fields: %v", usage)
	}
}

// TestResponsesStreamReasoning responses 流式（summary=auto）：reasoning item
// 事件序列 + completed 的 usage details（reasoning_tokens / cached_tokens）。
func TestResponsesStreamReasoning(t *testing.T) {
	events := runReasoningStream((responsesAdapter{summary: "auto"}).NewStream("glm-5.2"))
	seen := map[string]bool{}
	sawDelta := false
	var completed map[string]any
	for _, ev := range events {
		seen[ev.name] = true
		m := evMap(t, ev)
		if ev.name == "response.reasoning_summary_text.delta" && (m["delta"] == "step one" || m["delta"] == " step two") {
			sawDelta = true
		}
		if ev.name == "response.completed" {
			completed = m
		}
	}
	for _, want := range []string{
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
	} {
		if !seen[want] {
			t.Errorf("missing event %s", want)
		}
	}
	if !sawDelta {
		t.Error("reasoning summary deltas missing")
	}
	if completed == nil {
		t.Fatal("response.completed missing")
	}
	resp, _ := completed["response"].(map[string]any)
	out, _ := resp["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("completed output len=%d want [reasoning, message]", len(out))
	}
	item, _ := out[0].(map[string]any)
	if item["type"] != "reasoning" || item["id"] != "rs_0" {
		t.Errorf("first output item: %v", out[0])
	}
	summary, _ := item["summary"].([]any)
	if len(summary) != 1 {
		t.Fatalf("reasoning summary: %v", item["summary"])
	}
	st, _ := summary[0].(map[string]any)
	if st["type"] != "summary_text" || st["text"] != "step one step two" {
		t.Errorf("summary entry: %v", summary[0])
	}
	usage, _ := resp["usage"].(map[string]any)
	inDetails, _ := usage["input_tokens_details"].(map[string]any)
	outDetails, _ := usage["output_tokens_details"].(map[string]any)
	if asNum(t, inDetails["cached_tokens"]) != 34 {
		t.Errorf("cached_tokens: %v", usage)
	}
	if asNum(t, outDetails["reasoning_tokens"]) != 10 {
		t.Errorf("reasoning_tokens: %v", usage)
	}
}

// TestResponsesStreamReasoningNone summary=none/缺省 → 不发 reasoning 事件
//（对齐官方语义：是否输出由请求的 reasoning.summary 决定）。
func TestResponsesStreamReasoningNone(t *testing.T) {
	for _, summary := range []string{"", "none"} {
		events := runReasoningStream((responsesAdapter{summary: summary}).NewStream("glm-5.2"))
		for _, ev := range events {
			if strings.Contains(ev.name, "reasoning") {
				t.Errorf("summary=%q must not emit reasoning events: %s", summary, ev.name)
			}
			if ev.name == "response.completed" {
				resp, _ := evMap(t, ev)["response"].(map[string]any)
				out, _ := resp["output"].([]any)
				if len(out) != 1 {
					t.Errorf("summary=%q output len=%d want [message] only", summary, len(out))
				}
			}
		}
	}
}

// TestResponsesNonStreamReasoning 非流式（summary=auto）：output 首项 reasoning，
// usage details 带缓存与 thinking 计量；summary=none 时不带 reasoning item。
func TestResponsesNonStreamReasoning(t *testing.T) {
	chat := `{"id":"c8","model":"glm-5.2","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"deep thought","content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"prompt_cache_hit_tokens":34,"completion_tokens":15,"completion_thinking_tokens":10,"total_tokens":55}}`
	obj, _ := (responsesAdapter{summary: "auto"}).JSONResponse([]byte(chat)).(map[string]any)
	out, _ := obj["output"].([]any)
	if len(out) != 2 || out[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("output=%v want [reasoning, message]", out)
	}
	usage, _ := obj["usage"].(map[string]any)
	inDetails, _ := usage["input_tokens_details"].(map[string]any)
	outDetails, _ := usage["output_tokens_details"].(map[string]any)
	if asNum(t, inDetails["cached_tokens"]) != 34 {
		t.Errorf("cached_tokens: %v", usage)
	}
	if asNum(t, outDetails["reasoning_tokens"]) != 10 {
		t.Errorf("reasoning_tokens: %v", usage)
	}
	none, _ := (responsesAdapter{summary: "none"}).JSONResponse([]byte(chat)).(map[string]any)
	outNone, _ := none["output"].([]any)
	if len(outNone) != 1 || outNone[0].(map[string]any)["type"] != "message" {
		t.Fatalf("summary=none output=%v want [message] only", outNone)
	}
}
