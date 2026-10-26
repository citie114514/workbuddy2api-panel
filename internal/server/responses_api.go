// responses_api.go OpenAI Responses API（POST /v1/responses）适配器，v1.14.0 新增。
//
// 面向 Codex CLI 等原生走 /v1/responses 的客户端：此前链路需要 new-api / ccswitch
// 之类的外层转换器把 Responses 转成 chat/completions 再喂网关（tools 声明在那一层
// 丢失是实测常态，见 dsml.go 的标记修复背景）。本端点把转换收进网关，客户端
// 直连即可，选号 / 粘性 / 限流治理 / 标记修复全部继承。
//
// 支持面（v1）：instructions、input（string / message / function_call /
// function_call_output 项，input_text / output_text / input_image 片段）、
// function 工具、tool_choice、reasoning.effort、max_output_tokens、
// temperature / top_p、流式与非流式。reasoning 项与 summary 透出不支持
// （上游思维链不出站）；store / previous_response_id 无状态忽略；
// 非 function 工具（web_search 等）上游无对应能力，跳过并记日志。
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

// respIDGen 响应对象 ID 生成（resp_ 前缀 + 随机十六进制）。
var respIDGen = struct {
	mu sync.Mutex
	r  *rand.Rand
}{r: rand.New(rand.NewSource(time.Now().UnixNano()))}

func nextResponsesID(prefix string) string {
	respIDGen.mu.Lock()
	defer respIDGen.mu.Unlock()
	return fmt.Sprintf("%s_wb2api_%08x", prefix, respIDGen.r.Uint32())
}

// responsesAdapter Responses ↔ chat 适配器。
// responsesAdapter Responses ↔ chat 适配器。summary 为请求级状态（forRequest 从
// 原始请求的 reasoning.summary 解析），决定思维链是否以 reasoning 输出项透出：
// 非 "none" 且非空才发（none / 缺省 = 不发，对齐官方语义）。
type responsesAdapter struct {
	summary string
}

// forRequest 解析 reasoning.summary；其余字段 ConvertRequest 自己再解析一遍
//（解析成本可忽略，保持两个入口各自独立）。
func (a responsesAdapter) forRequest(raw []byte) protoAdapter {
	var peek struct {
		Reasoning *struct {
			Summary string `json:"summary"`
		} `json:"reasoning"`
	}
	_ = json.Unmarshal(raw, &peek)
	sum := ""
	if peek.Reasoning != nil {
		sum = strings.ToLower(strings.TrimSpace(peek.Reasoning.Summary))
	}
	return responsesAdapter{summary: sum}
}

// summaryEnabled 报告是否应把上游思维链以 reasoning 输出项透出。
func (a responsesAdapter) summaryEnabled() bool {
	return a.summary != "" && a.summary != "none"
}

// ── 请求转换 ────────────────────────────────────────────────────────────

// responsesRequest 只声明网关关心的字段；其余（store、metadata、include 等）
// 忽略即可——转换产物是从零构建的 chat body，天然不透传。
type responsesRequest struct {
	Model           string          `json:"model"`
	Instructions    string          `json:"instructions"`
	Input           json.RawMessage `json:"input"`
	MaxOutputTokens *int64          `json:"max_output_tokens"`
	Temperature     *float64        `json:"temperature"`
	TopP            *float64        `json:"top_p"`
	Stream          bool            `json:"stream"`
	Tools           []struct {
		Type        string         `json:"type"`
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
		Strict      *bool          `json:"strict"`
	} `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls"`
	Reasoning           *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
}

func (responsesAdapter) ConvertRequest(raw []byte) ([]byte, *protoAPIError) {
	var req responsesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, &protoAPIError{status: http.StatusBadRequest, typ: "invalid_request_error", msg: "invalid JSON body: " + err.Error()}
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, &protoAPIError{status: http.StatusBadRequest, typ: "invalid_request_error", msg: "model is required"}
	}
	out := map[string]any{"model": req.Model}

	msgs := make([]any, 0, 8)
	if strings.TrimSpace(req.Instructions) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.Instructions})
	}
	converted, perr := responsesInputToMessages(req.Input)
	if perr != nil {
		return nil, perr
	}
	msgs = append(msgs, converted...)
	out["messages"] = msgs

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		dropped := 0
		for _, t := range req.Tools {
			if t.Type != "function" && t.Type != "" { // 裸对象（无 type）按 function 兼容
				dropped++
				continue
			}
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if t.Parameters != nil {
				fn["parameters"] = t.Parameters
			}
			if t.Strict != nil {
				fn["strict"] = *t.Strict
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		if dropped > 0 {
			log.Printf("INFO: [responses] dropped %d non-function tool(s) (upstream has no server-side search etc.)", dropped)
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		var tcStr string
		if err := json.Unmarshal(req.ToolChoice, &tcStr); err == nil {
			out["tool_choice"] = tcStr
		} else {
			var tcObj struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if json.Unmarshal(req.ToolChoice, &tcObj) == nil && tcObj.Type == "function" && tcObj.Name != "" {
				out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": tcObj.Name}}
			} else if tcObj.Type == "auto" || tcObj.Type == "required" || tcObj.Type == "none" {
				out["tool_choice"] = tcObj.Type
			}
		}
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out["reasoning_effort"] = req.Reasoning.Effort
	}
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		out["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	out["stream"] = req.Stream

	body, err := json.Marshal(out)
	if err != nil {
		return nil, &protoAPIError{status: http.StatusInternalServerError, typ: "server_error", msg: "convert request: " + err.Error()}
	}
	return body, nil
}

// responsesInputToMessages 把 input（string 或项数组）转成 chat messages。
// 认识的项：message / function_call / function_call_output；reasoning 等跳过。
func responsesInputToMessages(input json.RawMessage) ([]any, *protoAPIError) {
	if len(input) == 0 || string(input) == "null" {
		return nil, &protoAPIError{status: http.StatusBadRequest, typ: "invalid_request_error", msg: "input is required"}
	}
	var asStr string
	if err := json.Unmarshal(input, &asStr); err == nil {
		if strings.TrimSpace(asStr) == "" {
			return nil, &protoAPIError{status: http.StatusBadRequest, typ: "invalid_request_error", msg: "input is required"}
		}
		return []any{map[string]any{"role": "user", "content": asStr}}, nil
	}
	var items []map[string]any
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, &protoAPIError{status: http.StatusBadRequest, typ: "invalid_request_error", msg: "input must be a string or an array of items"}
	}
	msgs := make([]any, 0, len(items))
	appendAssistantToolCall := func(id, name, args string) {
		var last map[string]any
		if n := len(msgs); n > 0 {
			last, _ = msgs[n-1].(map[string]any)
		}
		if last != nil && last["role"] == "assistant" {
			if tcs, ok := last["tool_calls"].([]any); ok {
				last["tool_calls"] = append(tcs, map[string]any{
					"id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": args},
				})
				return
			}
		}
		msgs = append(msgs, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
			"id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": args},
		}}})
	}
	callSeq := 0
	for _, it := range items {
		typ, _ := it["type"].(string)
		switch typ {
		case "message", "": // 旧形态无 type 字段按 message 兼容
			role, _ := it["role"].(string)
			switch role {
			case "developer":
				role = "system" // payload.go 也会归一，这里先行一步
			case "user", "assistant", "system":
			default:
				role = "user"
			}
			msgs = append(msgs, map[string]any{"role": role, "content": responsesContentToChat(it["content"])})
		case "function_call":
			callSeq++
			callID, _ := it["call_id"].(string)
			if callID == "" {
				callID = fmt.Sprintf("call_%d", callSeq)
			}
			name, _ := it["name"].(string)
			args, _ := it["arguments"].(string)
			if args == "" {
				args = "{}"
			}
			appendAssistantToolCall(callID, name, args)
		case "function_call_output":
			callID, _ := it["call_id"].(string)
			output := it["output"]
			switch o := output.(type) {
			case string:
			case nil:
				output = ""
			default:
				raw, _ := json.Marshal(o)
				output = string(raw)
			}
			msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": callID, "content": output})
		case "reasoning", "item_reference":
			// 思考片段与引用项：出站侧已剔除，回放侧同样跳过。
		default:
			log.Printf("INFO: [responses] skipped unknown input item type=%q", typ)
		}
	}
	return msgs, nil
}

// responsesContentToChat message 项的 content（string 或片段数组）→ chat content。
// 单一文本片段折叠为裸字符串（对上游最友好），多片段保持数组形态。
func responsesContentToChat(content any) any {
	parts, ok := content.([]any)
	if !ok {
		if s, ok := content.(string); ok {
			return s
		}
		return ""
	}
	type piece struct {
		kind string
		body map[string]any
	}
	var pieces []piece
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch t, _ := part["type"].(string); t {
		case "input_text", "output_text", "text", "summary_text":
			text, _ := part["text"].(string)
			pieces = append(pieces, piece{"text", map[string]any{"type": "text", "text": text}})
		case "input_image":
			// image_url 兼容 string 与 {url} 对象两种形态（payload.go 归一化兜底）。
			if iu, ok := part["image_url"].(map[string]any); ok {
				pieces = append(pieces, piece{"image", map[string]any{"type": "image_url", "image_url": iu}})
			} else if s, ok := part["image_url"].(string); ok {
				pieces = append(pieces, piece{"image", map[string]any{"type": "image_url", "image_url": map[string]any{"url": s}}})
			}
		case "reasoning_text", "reasoning":
			// 跳过：思考片段不出站（dropReasoningContentParts 同口径）。
		}
	}
	if len(pieces) == 0 {
		return ""
	}
	if len(pieces) == 1 && pieces[0].kind == "text" {
		return pieces[0].body["text"]
	}
	out := make([]any, len(pieces))
	for i, p := range pieces {
		out[i] = p.body
	}
	return out
}

// ── 非流式响应转换 ──────────────────────────────────────────────────────

func (a responsesAdapter) JSONResponse(chat []byte) any {
	cc := parseChatCompletion(chat)
	if cc == nil {
		return map[string]any{"error": map[string]any{"code": "server_error", "message": "unparseable upstream response"}}
	}
	return a.buildResponseObject(cc, false)
}

// buildResponseObject 从标准化的 chat 完成形态构造 Responses 响应对象。
// incomplete：finish == "length" 时置 true（status / incomplete_details 跟随）。
// reasoning：summary 开启且上游给了 reasoning_content 时，作为首个输出项透出
//（官方 Responses 语义里 reasoning item 先于 message）。
func (a responsesAdapter) buildResponseObject(cc *chatCompletion, streamFinal bool) map[string]any {
	id := cc.id
	if id == "" {
		id = nextResponsesID("resp")
	}
	output := make([]any, 0, 2)
	var textOut strings.Builder
	if a.summaryEnabled() && cc.reasoning != "" {
		output = append(output, map[string]any{
			"id": "rs_0", "type": "reasoning", "summary": []any{
				map[string]any{"type": "summary_text", "text": cc.reasoning},
			},
		})
	}
	if cc.content != "" {
		textOut.WriteString(cc.content)
		output = append(output, map[string]any{
			"id": "msg_0", "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": cc.content, "annotations": []any{}}},
		})
	}
	for i, tc := range cc.toolCalls {
		callID := tc.id
		if callID == "" {
			callID = fmt.Sprintf("call_%d", i)
		}
		output = append(output, map[string]any{
			"id": fmt.Sprintf("fc_%d", i), "type": "function_call", "status": "completed",
			"call_id": callID, "name": tc.name, "arguments": tc.arguments,
		})
	}
	status := "completed"
	var incomplete any
	if cc.finish == "length" {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0,
		"input_tokens_details": map[string]any{"cached_tokens": 0},
		"output_tokens_details": map[string]any{"reasoning_tokens": 0}}
	if cc.usage != nil {
		usage["input_tokens"] = cc.usage.prompt
		usage["output_tokens"] = cc.usage.completion
		usage["total_tokens"] = cc.usage.total
		if details, ok := usage["input_tokens_details"].(map[string]any); ok {
			details["cached_tokens"] = cc.usage.cachedRead
		}
		if details, ok := usage["output_tokens_details"].(map[string]any); ok {
			details["reasoning_tokens"] = cc.usage.reasoning
		}
	}
	return map[string]any{
		"id": id, "object": "response", "created_at": time.Now().Unix(),
		"status": status, "model": cc.model, "output": output,
		"output_text": textOut.String(),
		"error": nil, "incomplete_details": incomplete,
		"usage": usage,
		"parallel_tool_calls": false, "tools": []any{}, "tool_choice": "auto",
		"reasoning": map[string]any{"effort": nil, "summary": nil},
		"metadata": map[string]any{},
	}
}

func (responsesAdapter) JSONError(status int, chatErrBody []byte) (int, any) {
	fe := parseChatErrorBody(chatErrBody)
	code := fe.code
	if code == "" {
		code = fe.typ
	}
	msg := fe.message
	if fe.hint != "" {
		msg += " (gateway hint: " + fe.hint + ")"
	}
	return status, map[string]any{"error": map[string]any{
		"code": code, "message": msg,
	}}
}

func (responsesAdapter) WriteErrorDirect(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": typ, "message": msg}})
}

// ── 流式翻译器 ──────────────────────────────────────────────────────────

// respToolState 一个进行中的 function_call 输出项。
type respToolState struct {
	itemID      string
	callID      string
	name        string
	args        strings.Builder
	outputIndex int
	opened      bool
	done        bool
}

// responsesStream chat SSE → Responses 事件的状态机。
type responsesStream struct {
	model     string
	respID    string
	createdAt int64
	started   bool
	summary   string // 请求级 reasoning.summary（见 responsesAdapter.forRequest）

	nextOutput int
	textOpen   bool
	textItemID string
	textOutputIndex int
	textBuf    strings.Builder

	// reasoning 输出项（summary 开启时把上游 reasoning_content 透出为
	// reasoning item + summary_text 增量；官方形态里先于 message 项）。
	reasoningOpen        bool
	reasoningItemID      string
	reasoningOutputIndex int
	reasoningBuf         strings.Builder

	tools    map[int]*respToolState
	toolKeys []int

	usage      *chatUsageInfo
	itemsDone  bool // finish 帧已关闭全部输出项
	completed  bool // response.completed 已发
	failed     bool
	finishSeen string
}

func (a responsesAdapter) NewStream(model string) chatStreamTranslator {
	return &responsesStream{
		model:     model,
		respID:    nextResponsesID("resp"),
		createdAt: time.Now().Unix(),
		summary:   a.summary,
		tools:     map[int]*respToolState{},
	}
}

// summaryEnabled 同适配器侧口径（流内复制一份，Close 组装 completed 时用）。
func (s *responsesStream) summaryEnabled() bool {
	return s.summary != "" && s.summary != "none"
}

func (s *responsesStream) Frame(payload []byte) []sseEvent {
	ch := parseChatChunk(payload)
	if ch == nil {
		return nil
	}
	if s.failed {
		return nil
	}
	if ch.err != nil {
		s.failed = true
		return s.failedEvents(ch.err)
	}
	var events []sseEvent
	if !s.started {
		s.started = true
		events = append(events, sseEvent{"response.created", map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id": s.respID, "object": "response", "created_at": s.createdAt,
				"status": "in_progress", "model": s.model,
				"output": []any{}, "error": nil, "usage": nil,
			},
		}})
	}
	if ch.model != "" && s.model == "" {
		s.model = ch.model
	}
	if ch.usage != nil {
		s.usage = ch.usage
	}
	// 思维链先行：上游 reasoning_content 先于正文，作为 reasoning 输出项透出
	//（受请求 reasoning.summary 控制，none/缺省不发）。
	if ch.reasoning != "" && s.summaryEnabled() {
		if s.textOpen {
			events = append(events, s.closeTextItem()...)
		}
		if !s.reasoningOpen {
			s.reasoningOpen = true
			s.reasoningItemID = fmt.Sprintf("rs_%d", s.nextOutput)
			s.reasoningOutputIndex = s.nextOutput
			s.nextOutput++
			s.reasoningBuf.Reset()
			events = append(events,
				sseEvent{"response.output_item.added", map[string]any{
					"type": "response.output_item.added", "output_index": s.reasoningOutputIndex,
					"item": map[string]any{"id": s.reasoningItemID, "type": "reasoning", "summary": []any{}},
				}},
				sseEvent{"response.reasoning_summary_part.added", map[string]any{
					"type": "response.reasoning_summary_part.added", "item_id": s.reasoningItemID,
					"output_index": s.reasoningOutputIndex, "summary_index": 0,
					"part": map[string]any{"type": "summary_text", "text": ""},
				}})
		}
		s.reasoningBuf.WriteString(ch.reasoning)
		events = append(events, sseEvent{"response.reasoning_summary_text.delta", map[string]any{
			"type": "response.reasoning_summary_text.delta", "item_id": s.reasoningItemID,
			"output_index": s.reasoningOutputIndex, "summary_index": 0, "delta": ch.reasoning,
		}})
	}
	if ch.content != "" {
		if !s.textOpen {
			events = append(events, s.closeReasoningItem()...)
			s.textOpen = true
			s.textItemID = fmt.Sprintf("msg_%d", s.nextOutput)
			s.textOutputIndex = s.nextOutput
			s.textBuf.Reset()
			events = append(events,
				sseEvent{"response.output_item.added", map[string]any{
					"type": "response.output_item.added", "output_index": s.nextOutput,
					"item": map[string]any{"id": s.textItemID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
				}},
				sseEvent{"response.content_part.added", map[string]any{
					"type": "response.content_part.added", "item_id": s.textItemID,
					"output_index": s.nextOutput, "content_index": 0,
					"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
				}})
			s.nextOutput++
		}
		s.textBuf.WriteString(ch.content)
		events = append(events, sseEvent{"response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": s.textItemID,
			"output_index": s.textOutputIndex, "content_index": 0, "delta": ch.content,
		}})
	}
	for _, td := range ch.toolCalls {
		st := s.tools[td.index]
		if st == nil {
			st = &respToolState{
				itemID:      fmt.Sprintf("fc_%d", len(s.tools)),
				callID:      td.id,
				outputIndex: s.nextOutput,
			}
			if st.callID == "" {
				st.callID = fmt.Sprintf("call_%d", len(s.tools))
			}
			s.tools[td.index] = st
			s.toolKeys = append(s.toolKeys, td.index)
			s.nextOutput++
		}
		if td.id != "" && st.callID == "" {
			st.callID = td.id
		}
		if td.name != "" && st.name == "" {
			st.name = td.name
		}
		if !st.opened {
			// 工具项开启前先收拢进行中的文本项（Responses 事件流按输出项分组）。
			events = append(events, s.closeTextItem()...)
			st.opened = true
			events = append(events, sseEvent{"response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": st.outputIndex,
				"item": map[string]any{
					"id": st.itemID, "type": "function_call", "status": "in_progress",
					"call_id": st.callID, "name": st.name, "arguments": "",
				},
			}})
		}
		if td.arguments != "" {
			st.args.WriteString(td.arguments)
			events = append(events, sseEvent{"response.function_call_arguments.delta", map[string]any{
				"type": "response.function_call_arguments.delta", "item_id": st.itemID,
				"output_index": st.outputIndex, "call_id": st.callID, "delta": td.arguments,
			}})
		}
	}
	if ch.finish != "" {
		s.finishSeen = ch.finish
		events = append(events, s.closeItems()...)
	}
	return events
}

// closeTextItem 关闭进行中的文本输出项（若无则空）。
func (s *responsesStream) closeTextItem() []sseEvent {
	if !s.textOpen {
		return nil
	}
	s.textOpen = false
	full := s.textBuf.String()
	idx := s.textIndexOf()
	return []sseEvent{
		{"response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": s.textItemID,
			"output_index": idx, "content_index": 0, "text": full,
		}},
		{"response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": s.textItemID,
			"output_index": idx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": full, "annotations": []any{}},
		}},
		{"response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": idx,
			"item": map[string]any{
				"id": s.textItemID, "type": "message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": full, "annotations": []any{}}},
			},
		}},
	}
}

// textIndexOf 文本项的 output_index（开启时单独记录，避免「文本 → 工具 →
// 再文本」形态下从 nextOutput 推导漂移）。
func (s *responsesStream) textIndexOf() int {
	return s.textOutputIndex
}

// closeReasoningItem 关闭进行中的 reasoning 输出项（若无则空）：
// summary_text.done → part.done → output_item.done（item.summary 带全文）。
func (s *responsesStream) closeReasoningItem() []sseEvent {
	if !s.reasoningOpen {
		return nil
	}
	s.reasoningOpen = false
	full := s.reasoningBuf.String()
	return []sseEvent{
		{"response.reasoning_summary_text.done", map[string]any{
			"type": "response.reasoning_summary_text.done", "item_id": s.reasoningItemID,
			"output_index": s.reasoningOutputIndex, "summary_index": 0, "text": full,
		}},
		{"response.reasoning_summary_part.done", map[string]any{
			"type": "response.reasoning_summary_part.done", "item_id": s.reasoningItemID,
			"output_index": s.reasoningOutputIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": full},
		}},
		{"response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": s.reasoningOutputIndex,
			"item": map[string]any{
				"id": s.reasoningItemID, "type": "reasoning",
				"summary": []any{map[string]any{"type": "summary_text", "text": full}},
			},
		}},
	}
}

// closeItems 关闭全部开启的输出项（finish 帧调用；幂等）。
func (s *responsesStream) closeItems() []sseEvent {
	if s.itemsDone {
		return nil
	}
	var events []sseEvent
	events = append(events, s.closeReasoningItem()...)
	events = append(events, s.closeTextItem()...)
	for _, k := range s.toolKeys {
		st := s.tools[k]
		if st.done {
			continue
		}
		st.done = true
		args := st.args.String()
		if args == "" {
			args = "{}"
		}
		events = append(events,
			sseEvent{"response.function_call_arguments.done", map[string]any{
				"type": "response.function_call_arguments.done", "item_id": st.itemID,
				"output_index": st.outputIndex, "call_id": st.callID, "arguments": args,
			}},
			sseEvent{"response.output_item.done", map[string]any{
				"type": "response.output_item.done", "output_index": st.outputIndex,
				"item": map[string]any{
					"id": st.itemID, "type": "function_call", "status": "completed",
					"call_id": st.callID, "name": st.name, "arguments": args,
				},
			}})
	}
	s.itemsDone = true
	return events
}

// Close 流结束（[DONE]）：发出 response.completed（含 usage 与完整 output）。
// 若 finish 帧缺失（上游异常形态），先强制收项再补 completed。
func (s *responsesStream) Close() []sseEvent {
	if s.completed || s.failed {
		return nil
	}
	s.completed = true
	events := s.closeItems()
	cc := &chatCompletion{
		id:        s.respID,
		model:     s.model,
		finish:    s.finishSeen,
		usage:     s.usage,
		reasoning: s.reasoningBuf.String(),
	}
	if s.textBuf.Len() > 0 {
		cc.content = s.textBuf.String()
	}
	for _, k := range s.toolKeys {
		st := s.tools[k]
		args := st.args.String()
		if args == "" {
			args = "{}"
		}
		cc.toolCalls = append(cc.toolCalls, chatToolDelta{id: st.callID, name: st.name, arguments: args})
	}
	resp := responsesAdapter{summary: s.summary}.buildResponseObject(cc, true)
	resp["id"] = s.respID
	resp["created_at"] = s.createdAt
	events = append(events, sseEvent{"response.completed", map[string]any{
		"type": "response.completed", "response": resp,
	}})
	return events
}

// failedEvents 上游 error 帧 → response.failed。
func (s *responsesStream) failedEvents(fe *chatFrameError) []sseEvent {
	code := fe.code
	if code == "" {
		code = fe.typ
	}
	msg := fe.message
	if fe.hint != "" {
		msg += " (gateway hint: " + fe.hint + ")"
	}
	s.completed = true // failed 即终态，Close 不再补 completed
	return []sseEvent{{"response.failed", map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"id": s.respID, "object": "response", "created_at": s.createdAt,
			"status": "failed", "model": s.model, "output": []any{}, "usage": nil,
			"error": map[string]any{"code": code, "message": msg},
		},
	}}}
}
