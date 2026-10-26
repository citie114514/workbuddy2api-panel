// proto_adapter.go 协议适配层（继任维护新增，v1.14.0）：
// /v1/responses（OpenAI Responses）与 /v1/messages（Anthropic Messages）的翻译基础设施。
//
// 设计约束：不改 chatCompletions 管道一字节。新端点把目标协议请求转成 chat
// 请求体、换掉 r.Body 后调用 h.chatCompletions，再用本文件的 ResponseWriter
// 包装器把管道写出的 OpenAI 形态响应（SSE 帧 / 聚合 JSON / 错误 JSON）实时
// 翻译回目标协议。由此选号、会话粘性、熔断冷却、限流治理、用量统计、
// 原生工具标记修复等全部既有能力对两个新协议自动生效（它们的输入都是
// chat 形态的 body：session.ExtractKey / TurnKey / ToolNameAllowlist 照常工作）。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
)

// sseEvent 目标协议的一个 SSE 事件（event 名 + data 对象；name 为空表示裸 data 帧）。
type sseEvent struct {
	name string
	data any
}

// chatStreamTranslator 有状态的 chat SSE → 目标协议事件翻译器。
type chatStreamTranslator interface {
	// Frame 处理一帧 chat "data:" payload（不含前缀）；nil 表示 [DONE]。
	Frame(payload []byte) []sseEvent
	// Close 流收尾（[DONE] 或底层写完）时调用；已收尾则返回空。
	Close() []sseEvent
}

// protoAdapter 一个目标协议的完整适配器。
type protoAdapter interface {
	// forRequest 返回请求级适配器实例：从原始请求解析影响出站形态的选项
	// （如 Responses 的 reasoning.summary）。无请求级状态的适配器返回自身。
	forRequest(raw []byte) protoAdapter
	// ConvertRequest 目标协议请求体 → chat 请求体。
	ConvertRequest(raw []byte) ([]byte, *protoAPIError)
	// JSONResponse chat 聚合 JSON（status 200）→ 目标协议响应对象。
	JSONResponse(chat []byte) any
	// JSONError chat 错误 JSON（{"error":{...}}）→ 目标协议错误对象与最终状态码。
	JSONError(status int, chatErrBody []byte) (int, any)
	// NewStream 创建流式翻译器（model 用于回显）。
	NewStream(model string) chatStreamTranslator
	// WriteErrorDirect 以目标协议错误形状直接写响应（鉴权失败 / 请求不可解析，
	// 此时没有 chat 形态可翻译）。
	WriteErrorDirect(w http.ResponseWriter, status int, errType, message string)
}

// protoAPIError 请求转换阶段的协议层错误（不经 chat 管道）。
type protoAPIError struct {
	status int
	typ    string // 协议各自的错误类型标识（Responses: code；Anthropic: error.type）
	msg    string
}

// protoResponseWriter 包装真实 ResponseWriter，拦截 chat 管道的写出并翻译。
//
// 模式按写出形态自动判定（chatCompletions 只有三种写法，全部覆盖）：
//   - 流式：Content-Type: text/event-stream + 200 —— 头透传，帧逐条翻译
//   - 非流式：application/json + 200 —— 缓冲整包，finish() 时翻译
//   - 错误：application/json + >=400 —— 缓冲，finish() 时翻译成目标协议错误
//
// JSON 两类都延迟到 finish()（chatCompletions 同步返回后由端点调用），
// 因此 Content-Length 由 net/http 自动计算，无需手动回填。
type protoResponseWriter struct {
	real    http.ResponseWriter
	adapter protoAdapter
	model   string

	status     int
	decided    bool
	headerSent bool
	isSSE      bool
	jsonBuf    bytes.Buffer
	frameBuf   []byte
	stream     chatStreamTranslator
	finished   bool
}

func newProtoWriter(real http.ResponseWriter, ad protoAdapter, model string) *protoResponseWriter {
	return &protoResponseWriter{real: real, adapter: ad, model: model, status: http.StatusOK}
}

func (p *protoResponseWriter) Header() http.Header { return p.real.Header() }

// WriteHeader 判定模式。SSE 头原样透传（两协议同为 text/event-stream）；
// JSON（含错误）延迟到 finish() 统一写出，期间不透传状态码。
func (p *protoResponseWriter) WriteHeader(code int) {
	if p.decided {
		return
	}
	p.decided = true
	p.status = code
	if code == http.StatusOK && strings.Contains(p.real.Header().Get("Content-Type"), "text/event-stream") {
		p.isSSE = true
		p.real.WriteHeader(code)
		p.headerSent = true
	}
}

func (p *protoResponseWriter) Write(b []byte) (int, error) {
	if !p.decided {
		p.WriteHeader(http.StatusOK)
	}
	if p.isSSE {
		p.feed(b)
	} else {
		p.jsonBuf.Write(b)
	}
	return len(b), nil
}

// Flush 透传 flush（仅 SSE 模式已发头时；JSON 模式未发头，flush 无意义）。
func (p *protoResponseWriter) Flush() {
	if p.headerSent {
		if fl, ok := p.real.(http.Flusher); ok {
			fl.Flush()
		}
	}
}

// feed 累积 SSE 字节，按 "\n\n" 切帧。StreamHint 每帧一次 WriteString，
// 但不依赖该形态——半帧/粘帧同样正确。
func (p *protoResponseWriter) feed(b []byte) {
	p.frameBuf = append(p.frameBuf, b...)
	for {
		i := bytes.Index(p.frameBuf, []byte("\n\n"))
		if i < 0 {
			return
		}
		frame := p.frameBuf[:i]
		p.frameBuf = append([]byte(nil), p.frameBuf[i+2:]...)
		for _, line := range strings.Split(string(frame), "\n") {
			line = strings.TrimRight(line, "\r")
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if p.stream == nil {
				p.stream = p.adapter.NewStream(p.model)
			}
			if payload == "[DONE]" {
				p.emit(p.stream.Close())
				continue
			}
			p.emit(p.stream.Frame([]byte(payload)))
		}
	}
}

// emit 写出若干目标协议事件（event 名 + data JSON），逐事件 flush。
//
// data 负载必须写进 "data:" 字段（SSE 规范）：此前裸写 JSON 行被 eventsource
// 解析器当未知字段丢弃、事件以空 data 派发 —— codex 0.164 的 SSE 消费层因此
// 收不到任何事件，报 "stream closed before response.completed"（v1.14.1 实案）。
// curl / 子串断言都测不出这类错误，只有真实 eventsource 客户端会暴露。
func (p *protoResponseWriter) emit(events []sseEvent) {
	for _, ev := range events {
		data, err := json.Marshal(ev.data)
		if err != nil {
			continue
		}
		var buf bytes.Buffer
		if ev.name != "" {
			buf.WriteString("event: " + ev.name + "\n")
		}
		buf.WriteString("data: ")
		buf.Write(data)
		buf.WriteString("\n\n")
		_, _ = p.real.Write(buf.Bytes())
		p.Flush()
	}
}

// finish chatCompletions 返回后由端点调用：JSON 模式在此翻译并真正写出。
func (p *protoResponseWriter) finish() {
	if p.finished {
		return
	}
	p.finished = true
	if p.headerSent {
		return // SSE 已边写边译
	}
	body := p.jsonBuf.Bytes()
	p.real.Header().Set("Content-Type", "application/json")
	if p.status >= http.StatusBadRequest || !json.Valid(body) {
		status, obj := p.adapter.JSONError(p.status, body)
		raw, _ := json.Marshal(obj)
		p.real.WriteHeader(status)
		_, _ = p.real.Write(raw)
		return
	}
	raw, err := json.Marshal(p.adapter.JSONResponse(body))
	if err != nil {
		p.real.WriteHeader(http.StatusBadGateway)
		_, _ = p.real.Write([]byte(`{"error":{"message":"protocol translation failed"}}`))
		return
	}
	p.real.WriteHeader(p.status)
	_, _ = p.real.Write(raw)
}

// ── chat 形态解析（两个适配器共用）────────────────────────────────────────

// chatToolDelta 一片流式 tool_calls 增量。
type chatToolDelta struct {
	index     int
	id        string
	name      string
	arguments string
}

// chatUsageInfo usage 帧的 token 计数。
// reasoning = completion_tokens_details.reasoning_tokens（缺省回落 completion_thinking_tokens），
// cachedRead/cachedWrite = prompt_cache_hit_tokens / prompt_cache_write_tokens
// （别名 cache_read_input_tokens / cache_creation_input_tokens 兜底）。
// 加和口径（实测）：total = prompt + completion，thinking 已计入 completion。
type chatUsageInfo struct {
	prompt     int
	completion int
	total      int
	reasoning  int
	thinking   int
	cachedRead  int
	cachedWrite int
}

// chatFrameError 上游 error 帧 / 错误 JSON 的标准形状。
type chatFrameError struct {
	message string
	typ     string
	code    string
	hint    string
}

// chatChunk 从 chat SSE 帧解析出的标准化增量。
type chatChunk struct {
	id        string
	model     string
	content   string
	reasoning string
	toolCalls []chatToolDelta
	finish    string
	usage     *chatUsageInfo
	err       *chatFrameError
}

// parseUsageInfo 从 usage map（normalizeUsageCacheAliases 后的形态）提取计数。
func parseUsageInfo(u map[string]any) *chatUsageInfo {
	ui := &chatUsageInfo{
		prompt:     int(numOf2int(u["prompt_tokens"])),
		completion: int(numOf2int(u["completion_tokens"])),
		total:      int(numOf2int(u["total_tokens"])),
		thinking:   int(numOf2int(u["completion_thinking_tokens"])),
		cachedRead: int(numOf2int(u["prompt_cache_hit_tokens"])),
		cachedWrite: int(numOf2int(u["prompt_cache_write_tokens"])),
	}
	if ui.cachedRead == 0 {
		ui.cachedRead = int(numOf2int(u["cache_read_input_tokens"]))
	}
	if ui.cachedWrite == 0 {
		ui.cachedWrite = int(numOf2int(u["cache_creation_input_tokens"]))
	}
	if details, ok := u["completion_tokens_details"].(map[string]any); ok {
		ui.reasoning = int(numOf2int(details["reasoning_tokens"]))
	}
	if ui.reasoning == 0 {
		ui.reasoning = ui.thinking
	}
	return ui
}

// parseChatChunk 解析一帧 chat SSE payload；非 JSON / 空 JSON 返回 nil。
// error 对象优先（error 帧 choices 为空或缺失）。
func parseChatChunk(payload []byte) *chatChunk {
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil
	}
	c := &chatChunk{}
	c.id, _ = obj["id"].(string)
	c.model, _ = obj["model"].(string)
	if e, ok := obj["error"].(map[string]any); ok {
		fe := &chatFrameError{}
		fe.message, _ = e["message"].(string)
		fe.typ, _ = e["type"].(string)
		fe.code, _ = e["code"].(string)
		fe.hint, _ = e["gateway_hint"].(string)
		c.err = fe
		return c
	}
	if u, ok := obj["usage"].(map[string]any); ok {
		c.usage = parseUsageInfo(u)
	}
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		ch, _ := chs[0].(map[string]any)
		if ch != nil {
			c.finish, _ = ch["finish_reason"].(string)
			if d, ok := ch["delta"].(map[string]any); ok {
				c.content, _ = d["content"].(string)
				c.reasoning, _ = d["reasoning_content"].(string)
				if tcs, ok := d["tool_calls"].([]any); ok {
					for _, raw := range tcs {
						tc, _ := raw.(map[string]any)
						if tc == nil {
							continue
						}
						td := chatToolDelta{index: int(numOf2int(tc["index"]))}
						td.id, _ = tc["id"].(string)
						if fn, ok := tc["function"].(map[string]any); ok {
							td.name, _ = fn["name"].(string)
							td.arguments, _ = fn["arguments"].(string)
						}
						c.toolCalls = append(c.toolCalls, td)
					}
				}
			}
		}
	}
	return c
}

// chatCompletion 非流式聚合响应的标准化形态。
type chatCompletion struct {
	id        string
	model     string
	content   string
	reasoning string
	toolCalls []chatToolDelta
	finish    string
	usage     *chatUsageInfo
}

// parseChatCompletion 解析 chat 聚合 JSON（choices[0].message + usage）。
func parseChatCompletion(body []byte) *chatCompletion {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}
	c := &chatCompletion{}
	c.id, _ = obj["id"].(string)
	c.model, _ = obj["model"].(string)
	if u, ok := obj["usage"].(map[string]any); ok {
		c.usage = parseUsageInfo(u)
	}
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		ch, _ := chs[0].(map[string]any)
		if ch == nil {
			return c
		}
		c.finish, _ = ch["finish_reason"].(string)
		if msg, ok := ch["message"].(map[string]any); ok {
			c.content, _ = msg["content"].(string)
			c.reasoning, _ = msg["reasoning_content"].(string)
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, raw := range tcs {
					tc, _ := raw.(map[string]any)
					if tc == nil {
						continue
					}
					td := chatToolDelta{}
					td.id, _ = tc["id"].(string)
					if fn, ok := tc["function"].(map[string]any); ok {
						td.name, _ = fn["name"].(string)
						td.arguments, _ = fn["arguments"].(string)
					}
					c.toolCalls = append(c.toolCalls, td)
				}
			}
		}
	}
	return c
}

// parseChatErrorBody 解析错误 JSON 的 {"error":{...}}；非该形状时 message 兜底原文。
func parseChatErrorBody(body []byte) *chatFrameError {
	var obj struct {
		Error *chatFrameErrorJSON `json:"error"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Error == nil {
		return &chatFrameError{message: strings.TrimSpace(string(body))}
	}
	return &chatFrameError{
		message: obj.Error.Message,
		typ:     obj.Error.Type,
		code:    obj.Error.Code,
		hint:    obj.Error.GatewayHint,
	}
}

type chatFrameErrorJSON struct {
	Message     string `json:"message"`
	Type        string `json:"type"`
	Code        string `json:"code"`
	GatewayHint string `json:"gateway_hint"`
}

// numOf2int JSON 数字（float64）→ 整型；非数字返回 0。
func numOf2int(v any) float64 {
	f, _ := v.(float64)
	return f
}

// serveChatViaAdapter 端点主体：读目标协议请求 → 转换 → 以包装器调 chatCompletions。
// 转换失败直接以目标协议错误形状响应（不经 chat 管道）。
func (h *Handler) serveChatViaAdapter(w http.ResponseWriter, r *http.Request, ad protoAdapter) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		ad.WriteErrorDirect(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	ad = ad.forRequest(raw) // 请求级状态（如 reasoning.summary）供流式/非流式翻译共用
	chatBody, perr := ad.ConvertRequest(raw)
	if perr != nil {
		ad.WriteErrorDirect(w, perr.status, perr.typ, perr.msg)
		return
	}
	model := ""
	var peek struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(chatBody, &peek)
	if peek.Model != "" {
		model = peek.Model
	}

	pw := newProtoWriter(w, ad, model)
	r.Body = io.NopCloser(bytes.NewReader(chatBody))
	r.ContentLength = int64(len(chatBody))
	h.chatCompletions(pw, r)
	pw.finish()
}

// responsesEndpoint POST /v1/responses —— OpenAI Responses（Codex 等直连）。
func (h *Handler) responsesEndpoint(w http.ResponseWriter, r *http.Request) {
	h.serveChatViaAdapter(w, r, responsesAdapter{})
}

// anthropicMessages POST /v1/messages —— Anthropic Messages（Claude Code 等直连）。
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	h.serveChatViaAdapter(w, r, anthropicAdapter{})
}

// anthropicCountTokens POST /v1/messages/count_tokens —— 本地粗估（chars/4），
// 不打上游。Claude Code 用它预检上下文长度，容忍近似值。
func (h *Handler) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	ad := anthropicAdapter{}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		ad.WriteErrorDirect(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req anthropicRequest // count_tokens 不要求 max_tokens，宽解析
	if err := json.Unmarshal(raw, &req); err != nil {
		ad.WriteErrorDirect(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	chatBody, perr := anthropicReqToChat(&req)
	if perr != nil {
		ad.WriteErrorDirect(w, perr.status, perr.typ, perr.msg)
		return
	}
	var obj any
	if json.Unmarshal(chatBody, &obj) != nil {
		obj = map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": estimateChatTokens(obj)})
}

// estimateChatTokens 按全部字符串值的字符数 / 4 粗估 token（中文偏保守无害：
// 只用于上下文预检，宁可高估）。+1 保证非空输入不为 0。
func estimateChatTokens(obj any) int {
	total := 0
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			total += len([]rune(t))
		case map[string]any:
			for _, vv := range t {
				walk(vv)
			}
		case []any:
			for _, vv := range t {
				walk(vv)
			}
		}
	}
	walk(obj)
	return total/4 + 1
}

// withAuthAcceptAPIKey Bearer 或 x-api-key 二选一（Anthropic 客户端两种形态都发：
// ANTHROPIC_AUTH_TOKEN → Authorization: Bearer；默认 SDK → x-api-key）。
//
// anthropicShape 决定鉴权失败的错误形状：
//   - true（/v1/messages 系）：恒 Anthropic 形状——Claude Code 不总发
//     anthropic-version 头，不能拿头的有无判协议；
//   - false（/v1/models 双协议共享）：按 anthropic-version 头分流，
//     OpenAI 客户端不受影响。
func (h *Handler) withAuthAcceptAPIKey(next http.HandlerFunc, anthropicShape bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearerOrAPIKey(r, h.loadLive().APIKey) {
			if anthropicShape || r.Header.Get("Anthropic-Version") != "" {
				(anthropicAdapter{}).WriteErrorDirect(w, http.StatusUnauthorized, "authentication_error",
					"missing or invalid API key (Authorization: Bearer or x-api-key)")
			} else {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			}
			return
		}
		next(w, r)
	}
}
