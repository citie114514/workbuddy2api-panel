package server

// proto_api_test.go v1.14.0 协议适配层测试：/v1/responses（OpenAI Responses）与
// /v1/messages（Anthropic Messages）。
//
// 分三层：
//  1. 请求/响应转换单测（纯函数，覆盖两种协议的典型与畸形形态）
//  2. 流式翻译器单测（chat 帧序列 → 目标协议事件序列）
//  3. 端到端（Handler + fake 上游，含鉴权形态与错误翻译）

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// asNum 兼容取数：转换层直出的 map 里是 Go int（未经 JSON 往返），
// 经 recorder body 反序列化的是 float64。两种形态都合法，统一按数取。
func asNum(t *testing.T, v any) float64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	t.Fatalf("not a number: %v (%T)", v, v)
	return 0
}

// ── Responses：请求转换 ─────────────────────────────────────────────────

func TestResponsesRequestConversion(t *testing.T) {
	raw := `{
		"model": "glm-5.2",
		"instructions": "Be brief.",
		"input": [
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hi"}]},
			{"type": "function_call", "call_id": "call_1", "name": "shell", "arguments": "{\"cmd\":\"ls\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "file-a"},
			{"type": "reasoning", "summary": []}
		],
		"tools": [
			{"type": "function", "name": "shell", "description": "run", "parameters": {"type": "object"}},
			{"type": "web_search_preview"}
		],
		"tool_choice": "auto",
		"reasoning": {"effort": "high"},
		"max_output_tokens": 100,
		"temperature": 0.2,
		"stream": true
	}`
	body, perr := responsesAdapter{}.ConvertRequest([]byte(raw))
	if perr != nil {
		t.Fatalf("ConvertRequest: %+v", perr)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("converted body not JSON: %v\n%s", err, body)
	}
	if got["model"] != "glm-5.2" || got["reasoning_effort"] != "high" {
		t.Errorf("model/reasoning_effort: %v / %v", got["model"], got["reasoning_effort"])
	}
	if n, _ := got["max_tokens"].(float64); n != 100 {
		t.Errorf("max_tokens=%v want 100", got["max_tokens"])
	}
	if got["stream"] != true {
		t.Errorf("stream=%v", got["stream"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len=%d want 4 (system/user/assistant/tool): %s", len(msgs), body)
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "Be brief." {
		t.Errorf("system message: %v", msgs[0])
	}
	user, _ := msgs[1].(map[string]any)
	if user["role"] != "user" || user["content"] != "hi" {
		t.Errorf("user message: %v", msgs[1])
	}
	asst, _ := msgs[2].(map[string]any)
	tcs, _ := asst["tool_calls"].([]any)
	if asst["role"] != "assistant" || len(tcs) != 1 {
		t.Fatalf("assistant tool_calls: %v", msgs[2])
	}
	tc, _ := tcs[0].(map[string]any)
	if tc["id"] != "call_1" {
		t.Errorf("tool_call id=%v", tc["id"])
	}
	toolMsg, _ := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "file-a" {
		t.Errorf("tool message: %v", msgs[3])
	}
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("web_search must be dropped, tools=%v", got["tools"])
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "shell" || fn["description"] != "run" {
		t.Errorf("tool shape: %v", tools[0])
	}
}

func TestResponsesRequestInputString(t *testing.T) {
	body, perr := responsesAdapter{}.ConvertRequest([]byte(`{"model":"m","input":"hello"}`))
	if perr != nil {
		t.Fatalf("ConvertRequest: %+v", perr)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages=%v", msgs)
	}
	m, _ := msgs[0].(map[string]any)
	if m["role"] != "user" || m["content"] != "hello" {
		t.Errorf("message: %v", m)
	}
}

func TestResponsesRequestValidation(t *testing.T) {
	if _, perr := (responsesAdapter{}).ConvertRequest([]byte(`{"input":"x"}`)); perr == nil || perr.status != http.StatusBadRequest {
		t.Errorf("missing model must 400, got %+v", perr)
	}
	if _, perr := (responsesAdapter{}).ConvertRequest([]byte(`{"model":"m"}`)); perr == nil || perr.status != http.StatusBadRequest {
		t.Errorf("missing input must 400, got %+v", perr)
	}
	if _, perr := (responsesAdapter{}).ConvertRequest([]byte(`not-json`)); perr == nil {
		t.Error("bad JSON must 400")
	}
}

// ── Responses：非流式响应转换 ───────────────────────────────────────────

func TestResponsesJSONResponse(t *testing.T) {
	chat := `{"id":"chatcmpl-1","model":"glm-5.2","choices":[{"index":0,"message":{"role":"assistant","content":"你好","tool_calls":[{"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`
	obj, ok := responsesAdapter{}.JSONResponse([]byte(chat)).(map[string]any)
	if !ok {
		t.Fatal("JSONResponse type")
	}
	if obj["object"] != "response" || obj["status"] != "completed" {
		t.Errorf("object/status: %v/%v", obj["object"], obj["status"])
	}
	if obj["output_text"] != "你好" {
		t.Errorf("output_text=%v", obj["output_text"])
	}
	out, _ := obj["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("output len=%d want 2 (message + function_call)", len(out))
	}
	fc, _ := out[1].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "shell" {
		t.Errorf("function_call item: %v", fc)
	}
	usage, _ := obj["usage"].(map[string]any)
	if asNum(t, usage["input_tokens"]) != 3 || asNum(t, usage["output_tokens"]) != 5 || asNum(t, usage["total_tokens"]) != 8 {
		t.Errorf("usage: %v", usage)
	}
}

func TestResponsesJSONResponseIncomplete(t *testing.T) {
	chat := `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"截"},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	obj, _ := responsesAdapter{}.JSONResponse([]byte(chat)).(map[string]any)
	if obj["status"] != "incomplete" {
		t.Errorf("status=%v want incomplete", obj["status"])
	}
	if obj["incomplete_details"] == nil {
		t.Error("incomplete_details must be set")
	}
}

// ── Responses：流式翻译器 ───────────────────────────────────────────────

func responsesTestFrames() []string {
	return []string{
		`{"id":"c1","model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","content":"你"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"好"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"c\":"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"c1","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`,
	}
}

func TestResponsesStreamTranslator(t *testing.T) {
	s := responsesAdapter{}.NewStream("glm-5.2")
	var names []string
	var completedData map[string]any
	for _, f := range responsesTestFrames() {
		for _, ev := range s.Frame([]byte(f)) {
			names = append(names, ev.name)
			if ev.name == "response.completed" {
				completedData, _ = ev.data.(map[string]any)
			}
		}
	}
	for _, ev := range s.Close() {
		names = append(names, ev.name)
		if ev.name == "response.completed" {
			completedData, _ = ev.data.(map[string]any)
		}
	}
	join := strings.Join(names, ",")
	for _, want := range []string{
		"response.created",
		"response.output_item.added", "response.content_part.added", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.completed",
	} {
		if !strings.Contains(join, want) {
			t.Errorf("missing event %s in %s", want, join)
		}
	}
	// 事件顺序：文本项关闭必须先于 function_call 项开启。
	if strings.Index(join, "response.output_item.done") > strings.Index(join, "response.function_call_arguments.delta") {
		t.Errorf("text item must close before function_call deltas: %s", join)
	}
	if completedData == nil {
		t.Fatal("response.completed missing")
	}
	resp, _ := completedData["response"].(map[string]any)
	usage, _ := resp["usage"].(map[string]any)
	if asNum(t, usage["input_tokens"]) != 2 || asNum(t, usage["output_tokens"]) != 3 {
		t.Errorf("completed usage: %v", usage)
	}
	out, _ := resp["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("completed output len=%d want 2", len(out))
	}
	if resp["output_text"] != "你好" {
		t.Errorf("output_text=%v", resp["output_text"])
	}
}

func TestResponsesStreamErrorFrame(t *testing.T) {
	s := responsesAdapter{}.NewStream("m")
	events := s.Frame([]byte(`{"error":{"message":"6004 model rate limit","code":"6004","gateway_hint":"hint-x"}}`))
	if len(events) != 1 || events[0].name != "response.failed" {
		t.Fatalf("events=%v", events)
	}
	if s.Close() != nil {
		t.Error("Close after failed must be empty")
	}
	resp, _ := events[0].data.(map[string]any)["response"].(map[string]any)
	errObj, _ := resp["error"].(map[string]any)
	if !strings.Contains(errObj["message"].(string), "hint-x") {
		t.Errorf("gateway_hint must be carried: %v", errObj)
	}
}

// ── Anthropic：请求转换 ─────────────────────────────────────────────────

func TestAnthropicRequestConversion(t *testing.T) {
	raw := `{
		"model": "glm-5.2", "max_tokens": 512, "stream": false,
		"system": [{"type": "text", "text": "sys-a"}, {"type": "text", "text": "sys-b"}],
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "list files"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "QUJD"}}]},
			{"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": {"cmd": "ls"}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": [{"type": "text", "text": "file-a"}]}]}
		],
		"tools": [{"name": "Bash", "description": "run shell", "input_schema": {"type": "object"}}],
		"tool_choice": {"type": "any"},
		"temperature": 0.5,
		"stop_sequences": ["END"]
	}`
	body, perr := anthropicAdapter{}.ConvertRequest([]byte(raw))
	if perr != nil {
		t.Fatalf("ConvertRequest: %+v", perr)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("converted body not JSON: %v", err)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len=%d want 4: %s", len(msgs), body)
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "sys-a\nsys-b" {
		t.Errorf("system: %v", msgs[0])
	}
	user, _ := msgs[1].(map[string]any)
	content, _ := user["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("user content blocks=%d want 2 (text+image)", len(content))
	}
	img, _ := content[1].(map[string]any)
	iu, _ := img["image_url"].(map[string]any)
	if iu["url"] != "data:image/png;base64,QUJD" {
		t.Errorf("image url: %v", iu)
	}
	asst, _ := msgs[2].(map[string]any)
	tcs, _ := asst["tool_calls"].([]any)
	if asst["role"] != "assistant" || len(tcs) != 1 {
		t.Fatalf("assistant: %v", msgs[2])
	}
	tc, _ := tcs[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	if tc["id"] != "toolu_1" || fn["name"] != "Bash" || fn["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("tool_call: %v", tc)
	}
	toolMsg, _ := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "toolu_1" || toolMsg["content"] != "file-a" {
		t.Errorf("tool message: %v", msgs[3])
	}
	tools, _ := got["tools"].([]any)
	fnn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if fnn["parameters"] == nil {
		t.Errorf("input_schema must map to parameters: %v", tools[0])
	}
	if got["tool_choice"] != "required" {
		t.Errorf("tool_choice any→required, got %v", got["tool_choice"])
	}
	if n, _ := got["max_tokens"].(float64); n != 512 {
		t.Errorf("max_tokens=%v", got["max_tokens"])
	}
	stop, _ := got["stop"].([]any)
	if len(stop) != 1 || stop[0] != "END" {
		t.Errorf("stop=%v", got["stop"])
	}
}

func TestAnthropicRequestValidation(t *testing.T) {
	if _, perr := (anthropicAdapter{}).ConvertRequest([]byte(`{"model":"m"}`)); perr == nil || perr.typ != "invalid_request_error" {
		t.Errorf("missing max_tokens must 400 invalid_request_error, got %+v", perr)
	}
	if _, perr := (anthropicAdapter{}).ConvertRequest([]byte(`{"max_tokens":10}`)); perr == nil {
		t.Error("missing model must 400")
	}
	// tool_result-only 的 user 消息：拆出 tool 消息后不得残留空壳 user。
	body, perr := anthropicAdapter{}.ConvertRequest([]byte(`{"model":"m","max_tokens":9,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`))
	if perr != nil {
		t.Fatalf("ConvertRequest: %+v", perr)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v want [assistant(tool_calls), tool] only", msgs)
	}
	if m, _ := msgs[1].(map[string]any); m["role"] != "tool" {
		t.Errorf("second message role=%v", msgs[1])
	}
}

// ── Anthropic：非流式响应 / 流式翻译器 ──────────────────────────────────

func TestAnthropicJSONResponseToolUse(t *testing.T) {
	chat := `{"id":"chatcmpl-9","model":"glm-5.2","choices":[{"index":0,"message":{"role":"assistant","content":"看这个","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`
	obj, _ := anthropicAdapter{}.JSONResponse([]byte(chat)).(map[string]any)
	if obj["type"] != "message" || obj["stop_reason"] != "tool_use" {
		t.Errorf("type/stop_reason: %v/%v", obj["type"], obj["stop_reason"])
	}
	content, _ := obj["content"].([]any)
	if len(content) != 2 || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("content: %v", obj["content"])
	}
	tu, _ := content[1].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "Bash" {
		t.Fatalf("tool_use block: %v", tu)
	}
	input, _ := tu["input"].(map[string]any)
	if input["cmd"] != "ls" {
		t.Errorf("tool input: %v", input)
	}
	usage, _ := obj["usage"].(map[string]any)
	if asNum(t, usage["input_tokens"]) != 4 || asNum(t, usage["output_tokens"]) != 6 {
		t.Errorf("usage: %v", usage)
	}
}

func TestAnthropicStreamTranslator(t *testing.T) {
	s := anthropicAdapter{}.NewStream("glm-5.2")
	var names []string
	var dataBuf strings.Builder
	appendEvents := func(evs []sseEvent) {
		for _, ev := range evs {
			names = append(names, ev.name)
			if raw, err := json.Marshal(ev.data); err == nil {
				dataBuf.Write(raw)
			}
		}
	}
	for _, f := range responsesTestFrames() {
		appendEvents(s.Frame([]byte(f)))
	}
	lastEvents := s.Close()
	appendEvents(lastEvents)
	dataAll := dataBuf.String()
	join := strings.Join(names, ",")
	for _, want := range []string{"message_start", "ping", "content_block_start", "content_block_delta", "message_delta", "message_stop"} {
		if !strings.Contains(join, want) {
			t.Errorf("missing event %s in %s", want, join)
		}
	}
	if !strings.Contains(dataAll, "input_json_delta") {
		t.Errorf("tool argument deltas must map to input_json_delta: %s / %s", join, dataAll)
	}
	var deltaEv map[string]any
	for _, ev := range lastEvents {
		if ev.name == "message_delta" {
			deltaEv, _ = ev.data.(map[string]any)
		}
	}
	if deltaEv == nil {
		t.Fatal("message_delta missing in Close events")
	}
	delta, _ := deltaEv["delta"].(map[string]any)
	if delta["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v want tool_use", delta["stop_reason"])
	}
	usage, _ := deltaEv["usage"].(map[string]any)
	if asNum(t, usage["output_tokens"]) != 3 {
		t.Errorf("output_tokens=%v want 3", usage)
	}
}

// ── 端到端（Handler + fake 上游）────────────────────────────────────────

// sseToolCalls fake 上游流：文本 + 工具调用 + finish + usage。
const sseToolCalls = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"shell\",\"arguments\":\"{\\\"c\\\":\"}}]}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"ls\\\"}\"}}]}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n" +
	"data: [DONE]\n\n"

func newProtoTestHandler(t *testing.T, status int, body string) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return status, body, true
	})
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
}

func TestResponsesEndpointStream(t *testing.T) {
	h := newProtoTestHandler(t, 200, sseToolCalls)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"glm-5.2","stream":true,"instructions":"sys","input":"hi","tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}]}`))
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type=%q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.output_text.delta",
		"event: response.function_call_arguments.delta",
		"event: response.completed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Error("Responses stream must not carry chat-style [DONE]")
	}
	// SSE 规范：负载必须在 data: 字段（裸 JSON 行会被 eventsource 解析器丢弃，
	// codex 实测报 stream closed before response.completed —— v1.14.1 实案）。
	if !strings.Contains(body, "event: response.created
data: ") {
		t.Errorf("event payload must be on data: line:
%.300s", body)
	}
}

func TestResponsesEndpointNonStream(t *testing.T) {
	h := newProtoTestHandler(t, 200, sseToolCalls)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("body not JSON: %v\n%s", err, rec.Body)
	}
	if obj["object"] != "response" || obj["output_text"] != "你好" {
		t.Errorf("object/output_text: %v/%v", obj["object"], obj["output_text"])
	}
	out, _ := obj["output"].([]any)
	if len(out) != 2 || out[1].(map[string]any)["type"] != "function_call" {
		t.Errorf("output items: %v", obj["output"])
	}
}

func TestAnthropicEndpointStream(t *testing.T) {
	h := newProtoTestHandler(t, 200, sseToolCalls)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"glm-5.2","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "test")
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_delta",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s\nbody=%s", want, body)
		}
	}
	if !strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Errorf("stop_reason tool_use missing:\n%s", body)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Error("Anthropic stream must not carry chat-style [DONE]")
	}
	if !strings.Contains(body, "event: message_start
data: ") {
		t.Errorf("event payload must be on data: line:
%.300s", body)
	}
}

func TestAnthropicEndpointNonStream(t *testing.T) {
	h := newProtoTestHandler(t, 200, sseToolCalls)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("body not JSON: %v\n%s", err, rec.Body)
	}
	if obj["type"] != "message" || obj["stop_reason"] != "tool_use" {
		t.Errorf("type/stop_reason: %v/%v", obj["type"], obj["stop_reason"])
	}
	content, _ := obj["content"].([]any)
	if len(content) != 2 || content[1].(map[string]any)["type"] != "tool_use" {
		t.Errorf("content blocks: %v", obj["content"])
	}
}

func TestAnthropicAuthShapes(t *testing.T) {
	// 显式配置 APIKey：默认空串 = 鉴权关闭，wrong-key 用例就失去意义。
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		APIKey:   "test",
	})
	// x-api-key 通过。
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("x-api-key auth must pass, code=%d body=%s", rec.Code, rec.Body)
	}
	// Bearer 同样通过（ANTHROPIC_AUTH_TOKEN 形态）。
	req2 := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Authorization", "Bearer test")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("Bearer auth must pass, code=%d", rec2.Code)
	}
	// 错误 key → Anthropic 错误形状 401（/v1/messages 恒 Anthropic 形状；
	// /v1/models 的形状按 anthropic-version 头分流，见 TestModelsXAPIKeyAuth）。
	req3 := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req3.Header.Set("x-api-key", "wrong")
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if rec3.Code != 401 {
		t.Fatalf("wrong key must 401, got %d", rec3.Code)
	}
	var eobj map[string]any
	if err := json.Unmarshal(rec3.Body.Bytes(), &eobj); err != nil || eobj["type"] != "error" {
		t.Errorf("error shape: %s", rec3.Body)
	}
}

// TestAnthropicSystemRoleMessage Claude Code 实测会在 messages 里发 role:"system"
//（尽管规范要求 system 走顶层参数）：必须容忍并透传为 chat system 消息。
func TestAnthropicSystemRoleMessage(t *testing.T) {
	body, perr := (anthropicAdapter{}).ConvertRequest([]byte(`{"model":"m","max_tokens":9,"messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"hi"}]}`))
	if perr != nil {
		t.Fatalf("system role must be accepted: %+v", perr)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v", msgs)
	}
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "system" {
		t.Errorf("first message role=%v want system", msgs[0])
	}
}

func TestAnthropicCountTokens(t *testing.T) {
	h := newProtoTestHandler(t, 200, sseOK)
	req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(
		`{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"hello world"}]}]}`))
	req.Header.Set("x-api-key", "test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("body: %s", rec.Body)
	}
	if n, _ := obj["input_tokens"].(float64); n < 1 {
		t.Errorf("input_tokens=%v", obj["input_tokens"])
	}
}

// TestAnthropicEndpointUpstreamError 上游错误 → Anthropic 错误形状（JSON，
// 不再是 OpenAI 的 {"error":...} 顶层形状）。
func TestAnthropicEndpointUpstreamError(t *testing.T) {
	h := newProtoTestHandler(t, 200, "data: {\"error\":{\"message\":\"6004 model rate limit\",\"code\":\"6004\"}}\n\n"+
		"data: [DONE]\n\n")
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"glm-5.2","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Fatalf("stream error event missing:\n%s", body)
	}
	if !strings.Contains(body, "6004") {
		t.Errorf("upstream message must be carried:\n%s", body)
	}
}

// TestModelsAnthropicShape /v1/models 带 anthropic-version 头 → Anthropic 列表形状。
func TestModelsAnthropicShape(t *testing.T) {
	h := newProtoTestHandler(t, 200, sseOK)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("body: %s", rec.Body)
	}
	if _, ok := obj["has_more"]; !ok {
		t.Errorf("anthropic models shape missing has_more: %s", rec.Body)
	}
	if obj["object"] == "list" {
		t.Errorf("must not be openai shape: %s", rec.Body)
	}
}

// TestProtoWriterJSONErrorTranslation 错误 JSON 翻译的直测（不经完整管道）：
// chat 管道的 writeOpenAIError 输出必须变成目标协议错误形状。
func TestProtoWriterJSONErrorTranslation(t *testing.T) {
	rec := httptest.NewRecorder()
	pw := newProtoWriter(rec, anthropicAdapter{}, "m")
	writeOpenAIError(pw, http.StatusTooManyRequests, "rate_limited", "cooling down")
	pw.finish()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d", rec.Code)
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil || obj["type"] != "error" {
		t.Fatalf("body: %s", rec.Body)
	}
	e, _ := obj["error"].(map[string]any)
	if e["type"] != "rate_limit_error" {
		t.Errorf("error.type=%v want rate_limit_error", e["type"])
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("cooling down")) {
		t.Errorf("message lost: %s", rec.Body)
	}
}

// TestModelsXAPIKeyAuth Anthropic SDK 用 x-api-key 调 /v1/models（标准形态）：
// 必须与 Bearer 等效通过，且返回 Anthropic 形状。
func TestModelsXAPIKeyAuth(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		APIKey:   "test",
	})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("x-api-key", "test")
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("x-api-key must pass on /v1/models, code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"has_more"`) {
		t.Errorf("must be anthropic shape: %s", rec.Body)
	}
	// 无 anthropic-version 的错误 key → OpenAI 形状 401（不误用 Anthropic 形状）。
	req2 := httptest.NewRequest("GET", "/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer wrong")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 401 || strings.Contains(rec2.Body.String(), `"type":"error"`) {
		t.Errorf("openai-shape 401 expected, code=%d body=%s", rec2.Code, rec2.Body)
	}
}
