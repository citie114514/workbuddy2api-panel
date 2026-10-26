package server

// proto_reasoning_test.go v1.16.0：reasoning / 缓存计量在两条协议路径上的透出。
//
// 背景：chat 路径上游什么都给（reasoning_content、completion_thinking_tokens、
// prompt_cache_*），适配层此前把它们吞了 —— 三路对照实测定位（计量失真）：
//   - /v1/messages：thinking 块（thinking_delta + signature 占位）+ usage 缓存字段
//   - /v1/responses：reasoning item（受请求 reasoning.summary 控制）+ usage details
//   - /v1/chat/completions 不动（已经是全的）

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

// collectEvents 跑完一整条流（Frame ×N + Close），返回事件名序列与全部 data 拼接。
func collectEvents(tr chatStreamTranslator) (names []string, data string) {
	var buf strings.Builder
	for _, f := range reasoningTestFrames {
		for _, ev := range tr.Frame([]byte(f)) {
			names = append(names, ev.name)
			if raw, err := json.Marshal(ev.data); err == nil {
				buf.Write(raw)
			}
		}
	}
	for _, ev := range tr.Close() {
		names = append(names, ev.name)
		if raw, err := json.Marshal(ev.data); err == nil {
			buf.Write(raw)
		}
	}
	return names, buf.String()
}

// TestAnthropicStreamThinking messages 流式：reasoning_content → thinking 块
//（thinking_delta 增量 + 关块时 signature_delta 占位），正文前自动关块。
func TestAnthropicStreamThinking(t *testing.T) {
	names, data := collectEvents((anthropicAdapter{}).NewStream("glm-5.2"))
	if !strings.Contains(data, `"type":"thinking_delta","thinking":"step one"`) {
		t.Errorf("thinking_delta missing:\n%s", data)
	}
	if !strings.Contains(data, `"type":"signature_delta","signature":""`) {
		t.Errorf("signature_delta placeholder missing:\n%s", data)
	}
	if !strings.Contains(data, `"content_block":{"type":"thinking"`) {
		t.Errorf("thinking content_block_start missing:\n%s", data)
	}
	// 块序：thinking 关块（stop）必须先于 text 块的 start —— 第二个 start 之前有 stop。
	firstTextStart := -1
	for i, n := range names {
		if n == "content_block_start" {
			if firstTextStart < 0 {
				firstTextStart = i
				continue
			}
			// 第二个 content_block_start = text 块；其前必有 thinking 的 stop。
			stops := 0
			for _, m := range names[:i] {
				if m == "content_block_stop" {
					stops++
				}
			}
			if stops == 0 {
				t.Errorf("text block opened before thinking closed: %v", names)
			}
			break
		}
	}
}

// TestAnthropicStreamUsageCache messages 流式：message_delta usage 带缓存计量，
// output_tokens 透传 completion（实测口径已含 thinking）。
func TestAnthropicStreamUsageCache(t *testing.T) {
	tr := (anthropicAdapter{}).NewStream("glm-5.2")
	var last map[string]any
	for _, f := range reasoningTestFrames {
		for _, ev := range tr.Frame([]byte(f)) {
			if ev.name == "message_delta" {
				last, _ = ev.data.(map[string]any)
			}
		}
	}
	for _, ev := range tr.Close() {
		if ev.name == "message_delta" {
			last, _ = ev.data.(map[string]any)
		}
	}
	if last == nil {
		t.Fatal("message_delta missing")
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
	names, data := collectEvents((responsesAdapter{summary: "auto"}).NewStream("glm-5.2"))
	join := strings.Join(names, ",")
	for _, want := range []string{
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
	} {
		if !strings.Contains(join, want) {
			t.Errorf("missing event %s in %s", want, join)
		}
	}
	if !strings.Contains(data, `"type":"summary_text","text":"step one step two"`) {
		t.Errorf("reasoning summary text missing:\n%s", data)
	}
	// completed：output 首项是 reasoning item，usage details 带缓存与 thinking 计量。
	if !strings.Contains(data, `"output":[{"id":"rs_0","type":"reasoning"`) {
		t.Errorf("completed output must lead with reasoning item:\n%.400s", data)
	}
	if !strings.Contains(data, `"input_tokens_details":{"cached_tokens":34}`) {
		t.Errorf("cached_tokens missing:\n%.600s", data)
	}
	if !strings.Contains(data, `"output_tokens_details":{"reasoning_tokens":10}`) {
		t.Errorf("reasoning_tokens missing:\n%.600s", data)
	}
}

// TestResponsesStreamReasoningNone summary=none/缺省 → 不发 reasoning 事件
//（对齐官方语义：是否输出由请求的 reasoning.summary 决定）。
func TestResponsesStreamReasoningNone(t *testing.T) {
	for _, summary := range []string{"", "none"} {
		names, data := collectEvents((responsesAdapter{summary: summary}).NewStream("glm-5.2"))
		if strings.Contains(strings.Join(names, ","), "reasoning_summary") {
			t.Errorf("summary=%q must not emit reasoning events: %s", summary, strings.Join(names, ","))
		}
		if strings.Contains(data, `"type":"reasoning"`) {
			t.Errorf("summary=%q must not emit reasoning item: %s", summary, data)
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
