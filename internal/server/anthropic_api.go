// anthropic_api.go Anthropic Messages API（POST /v1/messages、POST /v1/messages/count_tokens）
// 适配器，v1.14.0 新增。
//
// 面向 Claude Code 等原生走 Anthropic 协议的客户端（ANTHROPIC_BASE_URL 指向网关，
// ANTHROPIC_AUTH_TOKEN 或 x-api-key 均可鉴权）：此前需要 ccswitch 之类的外层
// 协议转换代理，本端点把转换收进网关，客户端直连即可。
//
// 支持面（v1）：system（string / blocks）、text / image / tool_use / tool_result
// 内容块、tools + tool_choice、max_tokens（必填，对齐 Anthropic 规范）、
// stop_sequences、temperature / top_p、流式与非流式。thinking 块与
// thinking 请求参数不透传（上游思维链不出站，与 Responses 侧同口径）；
// 流式事件按官方形态（message_start / content_block_* / message_delta /
// message_stop / ping）。
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// anthropicAdapter Anthropic Messages ↔ chat 适配器。
type anthropicAdapter struct{}

// forRequest 无请求级状态（thinking 块有无取决于上游是否发 reasoning_content），
// 返回自身。
func (a anthropicAdapter) forRequest(raw []byte) protoAdapter { return a }

// ── 请求转换 ────────────────────────────────────────────────────────────

// anthropicRequest 只声明网关关心的字段。
type anthropicRequest struct {
	Model         string          `json:"model"`
	MaxTokens     *int64          `json:"max_tokens"`
	System        json.RawMessage `json:"system"`
	Messages      []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Stream   bool `json:"stream"`
	Tools    []struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"input_schema"`
	} `json:"tools"`
	ToolChoice     *struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"tool_choice"`
	Temperature  *float64  `json:"temperature"`
	TopP         *float64  `json:"top_p"`
	StopSequences []string `json:"stop_sequences"`
}

// ConvertRequest Anthropic Messages → chat 请求体。
func (a anthropicAdapter) ConvertRequest(raw []byte) ([]byte, *protoAPIError) {
	req, perr := parseAnthropicRequest(raw)
	if perr != nil {
		return nil, perr
	}
	return anthropicReqToChat(req)
}

// parseAnthropicRequest 解析 + Anthropic 规范级校验（max_tokens 必填）。
// 导出给 count_tokens 复用（count_tokens 不要求 max_tokens，调用方按需豁免）。
func parseAnthropicRequest(raw []byte) (*anthropicRequest, *protoAPIError) {
	var req anthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, &protoAPIError{status: http.StatusBadRequest, typ: "invalid_request_error", msg: "invalid JSON body: " + err.Error()}
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, &protoAPIError{status: http.StatusBadRequest, typ: "invalid_request_error", msg: "model: Field required"}
	}
	if req.MaxTokens == nil || *req.MaxTokens <= 0 {
		return nil, &protoAPIError{status: http.StatusBadRequest, typ: "invalid_request_error", msg: "max_tokens: Field required"}
	}
	return &req, nil
}

// anthropicReqToChat 核心转换（被 messages 与 count_tokens 共用；
// opts 为 nil 时按聊天请求组装）。
func anthropicReqToChat(req *anthropicRequest) ([]byte, *protoAPIError) {
	out := map[string]any{"model": req.Model, "stream": req.Stream}
	msgs := make([]any, 0, len(req.Messages)+1)

	if sys := anthropicSystemText(req.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	callSeq := 0
	for _, m := range req.Messages {
		role := m.Role
		if role != "user" && role != "assistant" && role != "system" {
			return nil, &protoAPIError{status: http.StatusBadRequest, typ: "invalid_request_error",
				msg: fmt.Sprintf("messages: roles must alternatively be 'user' or 'assistant', got %q", role)}
		}
		// 容忍 role:"system" 消息（Claude Code 实测会这样发，尽管 Anthropic 规范
		// 要求 system 走顶层参数）：chat 形态本就支持 system 角色，原样透传。
		var blocks []map[string]any
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			// content 允许是裸字符串。
			var s string
			if json.Unmarshal(m.Content, &s) == nil {
				blocks = []map[string]any{{"type": "text", "text": s}}
			} else {
				return nil, &protoAPIError{status: http.StatusBadRequest, typ: "invalid_request_error",
					msg: "messages.content: could not validate content block"}
			}
		}
		// Anthropic 的 user 消息里 tool_result 与 text 可混排；chat 形态里 tool 结果
		// 必须是独立 role:"tool" 消息。按块顺序拆分：文本聚成本消息正文，
		// tool_result 各自成 tool 消息（保持先后关系）。
		var textParts []string
		var imageParts []any
		var toolResults []map[string]any
		var toolUses []map[string]any
		for _, b := range blocks {
			switch t, _ := b["type"].(string); t {
			case "text", "":
				text, _ := b["text"].(string)
				textParts = append(textParts, text)
			case "image":
				if src, ok := b["source"].(map[string]any); ok {
					if st, _ := src["type"].(string); st == "base64" {
						mt, _ := src["media_type"].(string)
						data, _ := src["data"].(string)
						imageParts = append(imageParts, map[string]any{
							"type":      "image_url",
							"image_url": map[string]any{"url": "data:" + mt + ";base64," + data},
						})
					}
				}
			case "tool_use":
				toolUses = append(toolUses, b)
			case "tool_result":
				toolResults = append(toolResults, b)
			case "thinking", "redacted_thinking", "server_tool_use", "web_search_tool_result":
				// 思考与服务端工具痕迹不出站（dropReasoningContentParts 同口径）。
			default:
				// 未知块跳过，宁缺毋错。
			}
		}
		// tool_result 只应出现在 user 消息；畸形位置按文本化兜底不在这里拦（上游会报）。
		for _, tr := range toolResults {
			tuID, _ := tr["tool_use_id"].(string)
			msgs = append(msgs, map[string]any{
				"role": "tool", "tool_call_id": tuID,
				"content": anthropicResultContent(tr["content"]),
			})
		}
		switch {
		case role == "assistant" && len(toolUses) > 0:
			tcs := make([]any, 0, len(toolUses))
			for _, tu := range toolUses {
				callSeq++
				id, _ := tu["id"].(string)
				if id == "" {
					id = fmt.Sprintf("call_%d", callSeq)
				}
				name, _ := tu["name"].(string)
				rawArgs, _ := json.Marshal(tu["input"])
				tcs = append(tcs, map[string]any{
					"id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": string(rawArgs)},
				})
			}
			am := map[string]any{"role": "assistant", "tool_calls": tcs}
			if txt := strings.Join(textParts, "\n"); txt != "" {
				am["content"] = txt
			}
			msgs = append(msgs, am)
		default:
			if len(imageParts) > 0 {
				content := make([]any, 0, len(textParts)+len(imageParts))
				for _, t := range textParts {
					content = append(content, map[string]any{"type": "text", "text": t})
				}
				content = append(content, imageParts...)
				msgs = append(msgs, map[string]any{"role": role, "content": content})
			} else if txt := strings.Join(textParts, "\n"); txt != "" {
				msgs = append(msgs, map[string]any{"role": role, "content": txt})
			} else if role == "user" && len(toolResults) == 0 {
				// Anthropic 规范 user 消息必非空；空 content 原样上抛给上游报真实错误，
				// 不在网关侧静默吞（与「坏 body 不二次错误化」的既有口径一致）。
				msgs = append(msgs, map[string]any{"role": role, "content": ""})
			}
			// 其余空壳（如仅含 tool_result 的 user 消息）不保留：tool 消息已拆出，
			// 空 user 壳会被上游 content 校验拒绝。
		}
	}
	out["messages"] = msgs

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if t.InputSchema != nil {
				fn["parameters"] = t.InputSchema
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		out["tools"] = tools
	}
	if req.ToolChoice != nil {
		switch req.ToolChoice.Type {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "tool":
			if req.ToolChoice.Name != "" {
				out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": req.ToolChoice.Name}}
			}
		}
	}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		out["max_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, &protoAPIError{status: http.StatusInternalServerError, typ: "api_error", msg: "convert request: " + err.Error()}
	}
	return body, nil
}

// anthropicSystemText system 字段（string 或 [{type:"text",text}] 块）→ 纯文本。
func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// anthropicResultContent tool_result.content（string / 块数组）→ chat 的 content。
// 文本块转 text，图片块（base64 source）转 image_url data URL 透传 —— 此前图片被
// 静默丢弃，Claude Code 的截图类工具结果模型根本看不到。单一文本折叠为裸字符串。
func anthropicResultContent(content any) any {
	blocks, ok := content.([]any)
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
	for _, raw := range blocks {
		b, _ := raw.(map[string]any)
		if b == nil {
			continue
		}
		switch t, _ := b["type"].(string); t {
		case "text", "":
			if text, _ := b["text"].(string); text != "" {
				pieces = append(pieces, piece{"text", map[string]any{"type": "text", "text": text}})
			}
		case "image":
			if src, ok := b["source"].(map[string]any); ok && src["type"] == "base64" {
				mt, _ := src["media_type"].(string)
				data, _ := src["data"].(string)
				if data != "" {
					pieces = append(pieces, piece{"image", map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "data:" + mt + ";base64," + data},
					}})
				}
			}
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
}

// ── 非流式响应转换 ──────────────────────────────────────────────────────

// anthropicStopReason chat finish_reason → Anthropic stop_reason。
func anthropicStopReason(finish string) string {
	switch finish {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "stop":
		return "stop_sequence" // chat 的 stop 若因 stop 序列命中，OpenAI 也报 stop；细分不可知，取 end_turn 更保守
	default:
		return "end_turn"
	}
}

func (anthropicAdapter) JSONResponse(chat []byte) any {
	cc := parseChatCompletion(chat)
	if cc == nil {
		return map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "unparseable upstream response"}}
	}
	content := make([]any, 0, 2)
	// thinking 块：上游 reasoning_content 原样透出（signature 占位空串 —— 网关
	// 不回传客户端思维链供续写，入站侧本就丢弃 thinking 块，签名无需可验）。
	if cc.reasoning != "" {
		content = append(content, map[string]any{
			"type": "thinking", "thinking": cc.reasoning, "signature": "",
		})
	}
	if cc.content != "" {
		content = append(content, map[string]any{"type": "text", "text": cc.content})
	}
	for i, tc := range cc.toolCalls {
		id := tc.id
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		input := map[string]any{}
		if tc.arguments != "" {
			var parsed any
			if json.Unmarshal([]byte(tc.arguments), &parsed) == nil {
				if m, ok := parsed.(map[string]any); ok {
					input = m
				} else {
					input = map[string]any{"_arguments": tc.arguments}
				}
			} else {
				input = map[string]any{"_arguments_raw": tc.arguments}
			}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": id, "name": tc.name, "input": input,
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	// usage：cache_read_input_tokens ← prompt_cache_hit_tokens、
	// cache_creation_input_tokens ← prompt_cache_write_tokens；
	// output_tokens = completion_tokens（实测口径已含 thinking token）。
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if cc.usage != nil {
		usage["input_tokens"] = cc.usage.prompt
		usage["output_tokens"] = cc.usage.completion
		usage["cache_read_input_tokens"] = cc.usage.cachedRead
		usage["cache_creation_input_tokens"] = cc.usage.cachedWrite
	}
	return map[string]any{
		"id": nextResponsesID("msg"), "type": "message", "role": "assistant",
		"model": cc.model, "content": content,
		"stop_reason": anthropicStopReason(cc.finish), "stop_sequence": nil,
		"usage": usage,
	}
}

// anthropicErrorTypeFromStatus chat 管道的 HTTP 状态码 → Anthropic error.type。
func anthropicErrorTypeFromStatus(status int) string {
	switch {
	case status == http.StatusBadRequest:
		return "invalid_request_error"
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "api_error"
	}
}

func (anthropicAdapter) JSONError(status int, chatErrBody []byte) (int, any) {
	fe := parseChatErrorBody(chatErrBody)
	msg := fe.message
	if fe.hint != "" {
		msg += " (gateway hint: " + fe.hint + ")"
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	return status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": anthropicErrorTypeFromStatus(status), "message": msg},
	}
}

func (anthropicAdapter) WriteErrorDirect(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

// ── 流式翻译器 ──────────────────────────────────────────────────────────

// anthToolState 一个进行中的 tool_use 内容块。
type anthToolState struct {
	blockIndex int
	id         string
	name       string
	args       strings.Builder
	opened     bool
	closed     bool
}

// anthropicStream chat SSE → Anthropic 事件的状态机。
type anthropicStream struct {
	model  string
	msgID  string
	started bool

	nextBlock int
	textOpen   bool
	textIndex  int
	textBuf    strings.Builder

	// thinking 块（上游 reasoning_content → thinking_delta）：与文本块互斥开启，
	// 切换时先关当前块。signature 以空串占位（入站侧丢弃 thinking，无可验签名场景）。
	reasoningOpen bool
	reasoningIndex int
	reasoningBuf   strings.Builder

	tools    map[int]*anthToolState
	toolKeys []int

	usageIn   int
	usageOut  int
	cacheRead  int
	cacheWrite int
	finish   string
	failed   bool
	stopped  bool
}

func (anthropicAdapter) NewStream(model string) chatStreamTranslator {
	return &anthropicStream{
		model: model,
		msgID: nextResponsesID("msg"),
		tools: map[int]*anthToolState{},
	}
}

func (s *anthropicStream) Frame(payload []byte) []sseEvent {
	ch := parseChatChunk(payload)
	if ch == nil {
		return nil
	}
	if s.failed || s.stopped {
		return nil
	}
	if ch.err != nil {
		s.failed = true
		msg := ch.err.message
		if ch.err.hint != "" {
			msg += " (gateway hint: " + ch.err.hint + ")"
		}
		return []sseEvent{{"error", map[string]any{
			"type": "error",
			"error": map[string]any{"type": anthropicErrorTypeFromStatus(http.StatusServiceUnavailable), "message": msg},
		}}}
	}
	var events []sseEvent
	if !s.started {
		s.started = true
		if ch.model != "" && s.model == "" {
			s.model = ch.model
		}
		events = append(events,
			sseEvent{"message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id": s.msgID, "type": "message", "role": "assistant", "model": s.model,
					"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
					"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
				},
			}},
			sseEvent{"ping", map[string]any{"type": "ping"}})
	}
	if ch.usage != nil {
		s.usageIn, s.usageOut = ch.usage.prompt, ch.usage.completion
		s.cacheRead, s.cacheWrite = ch.usage.cachedRead, ch.usage.cachedWrite
	}
	// 思维链先行：上游 reasoning_content 先于正文流出，先处理 thinking 再处理 text。
	if ch.reasoning != "" {
		if s.textOpen {
			events = append(events, s.closeTextBlock()...)
		}
		if !s.reasoningOpen {
			s.reasoningOpen = true
			s.reasoningIndex = s.nextBlock
			s.nextBlock++
			s.reasoningBuf.Reset()
			events = append(events, sseEvent{"content_block_start", map[string]any{
				"type": "content_block_start", "index": s.reasoningIndex,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			}})
		}
		s.reasoningBuf.WriteString(ch.reasoning)
		events = append(events, sseEvent{"content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.reasoningIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": ch.reasoning},
		}})
	}
	if ch.content != "" {
		if !s.textOpen {
			events = append(events, s.closeReasoningBlock()...)
			s.textOpen = true
			s.textIndex = s.nextBlock
			s.nextBlock++
			s.textBuf.Reset()
			events = append(events, sseEvent{"content_block_start", map[string]any{
				"type": "content_block_start", "index": s.textIndex,
				"content_block": map[string]any{"type": "text", "text": ""},
			}})
		}
		s.textBuf.WriteString(ch.content)
		events = append(events, sseEvent{"content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.textIndex,
			"delta": map[string]any{"type": "text_delta", "text": ch.content},
		}})
	}
	for _, td := range ch.toolCalls {
		st := s.tools[td.index]
		if st == nil {
			st = &anthToolState{blockIndex: s.nextBlock, id: td.id}
			if st.id == "" {
				st.id = fmt.Sprintf("call_%d", len(s.tools))
			}
			s.tools[td.index] = st
			s.toolKeys = append(s.toolKeys, td.index)
			s.nextBlock++
		}
		if td.name != "" && st.name == "" {
			st.name = td.name
		}
		if !st.opened {
			events = append(events, s.closeReasoningBlock()...)
			events = append(events, s.closeTextBlock()...)
			st.opened = true
			events = append(events, sseEvent{"content_block_start", map[string]any{
				"type": "content_block_start", "index": st.blockIndex,
				"content_block": map[string]any{"type": "tool_use", "id": st.id, "name": st.name, "input": map[string]any{}},
			}})
		}
		if td.arguments != "" {
			st.args.WriteString(td.arguments)
			events = append(events, sseEvent{"content_block_delta", map[string]any{
				"type": "content_block_delta", "index": st.blockIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": td.arguments},
			}})
		}
	}
	if ch.finish != "" {
		s.finish = ch.finish
	}
	return events
}

// closeTextBlock 关闭进行中的文本块（若无则空）。
func (s *anthropicStream) closeTextBlock() []sseEvent {
	if !s.textOpen {
		return nil
	}
	s.textOpen = false
	return []sseEvent{{"content_block_stop", map[string]any{
		"type": "content_block_stop", "index": s.textIndex,
	}}}
}

// closeReasoningBlock 关闭进行中的 thinking 块：先补 signature_delta（空串占位，
// Anthropic 形态要求 thinking 块以签名收尾），再 content_block_stop。
func (s *anthropicStream) closeReasoningBlock() []sseEvent {
	if !s.reasoningOpen {
		return nil
	}
	s.reasoningOpen = false
	return []sseEvent{
		{"content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.reasoningIndex,
			"delta": map[string]any{"type": "signature_delta", "signature": ""},
		}},
		{"content_block_stop", map[string]any{
			"type": "content_block_stop", "index": s.reasoningIndex,
		}},
	}
}

// Close 流结束：关闭未关的块，message_delta + message_stop（幂等）。
func (s *anthropicStream) Close() []sseEvent {
	if s.stopped || s.failed {
		return nil
	}
	s.stopped = true
	events := s.closeReasoningBlock()
	events = append(events, s.closeTextBlock()...)
	for _, k := range s.toolKeys {
		st := s.tools[k]
		if !st.closed {
			st.closed = true
			events = append(events, sseEvent{"content_block_stop", map[string]any{
				"type": "content_block_stop", "index": st.blockIndex,
			}})
		}
	}
	deltaUsage := map[string]any{"output_tokens": s.usageOut}
	if s.usageIn > 0 || s.cacheRead > 0 || s.cacheWrite > 0 {
		// usage 帧只在流尾出现，缓存计量在这里随 message_delta 下发
		//（message_start 时上游还没给 usage，只能全零）。
		deltaUsage["input_tokens"] = s.usageIn
		deltaUsage["cache_read_input_tokens"] = s.cacheRead
		deltaUsage["cache_creation_input_tokens"] = s.cacheWrite
	}
	events = append(events,
		sseEvent{"message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{"stop_reason": anthropicStopReason(s.finish), "stop_sequence": nil},
			"usage": deltaUsage,
		}},
		sseEvent{"message_stop", map[string]any{"type": "message_stop"}})
	return events
}
