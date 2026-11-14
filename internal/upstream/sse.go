// sse.go 处理上游 SSE 流：聚合成单个 OpenAI 响应，或透传给客户端。
package upstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errEmptyStream 上游返回 200 但没有有效 SSE 数据帧（空流/只有注释/[DONE]）。
// 用哨兵错误替代裸 fmt.Errorf：StreamHint 的调用方（handler 流式路径）需要区分
// 「上游空流」与「客户端断连写失败」——空流是上游缺陷，应记 502 观测；写失败是
// 客户端已走，日志口径不同。Aggregate 与 StreamHint 共用同一哨兵（errors.Is 判定）。
var errEmptyStream = errors.New("upstream stream contained no valid data events")

// IsEmptyStreamError 报告错误是否为「上游空流」（无有效 SSE 帧）——供 handler
// 在流式路径把空流记为失败观测（HTTP 头已发出只能 200，但日志/状态应收敛到
// upstream_parse 同语义），与客户端断连类错误区分。
func IsEmptyStreamError(err error) bool { return errors.Is(err, errEmptyStream) }

// FirstFrameRejected 上游首个帧就是失败帧，且此刻尚未向客户端写出任何字节。
//
// 为什么单独成一个类型：这是**唯一还能换号重试的时机**。一旦写出第一字节，HTTP
// 响应头就已提交，本可换号解决的失败只能以 200 + error 帧透给客户端（头退不回来）。
// 上游实测形态：`{code:11140,msg:"request illegal"}`（账号被限）、`{code:6021,
// msg:"queue.full.title"}`（渠道队列满）都会以 HTTP 200 的 SSE 首帧出现。
//
// Kind 已按 Classify 归好类，上层接 applyErrorPolicy 即可；它是否值得换号由
// firstFrameRejectable 把关（换号也没用的类别继续走透传，不放大无效请求）。
type FirstFrameRejected struct {
	Kind ErrKind
	Body string
}

func (e *FirstFrameRejected) Error() string {
	msg := e.Body
	if len(msg) > 120 {
		msg = msg[:120] + "…"
	}
	return fmt.Sprintf("upstream first frame rejected (%s): %s", e.Kind, msg)
}

// firstFrameRejectable 首帧否决只对「换个账号可能就是好的」类别生效。
//
// 排除项都是换号必然重撞同一堵墙的：6021 渠道队列满（ErrUpstreamBusy）、请求体
// 畸形（ErrBadParams）、上下文超限（ErrPromptTooLong）、图片无效（ErrImageInvalid）、
// 工具序列损坏（ErrToolSequence）。对它们否决只会把无效请求放大 N 倍打上游，
// 还不如按原逻辑把上游原文透传给客户端。
func firstFrameRejectable(k ErrKind) bool {
	switch k {
	case ErrSoftRate, ErrAccountFault, ErrHardCredit, ErrServer, ErrModelBlocked, ErrSessionDead:
		return true
	}
	return false
}

// clearStreamHeaders 撤销 StreamHint 开头设过的 SSE 响应头。
// 首帧否决后这次请求不会以流式返回：header 里留着 text/event-stream 会让换号后
// 的成功响应、或最终兜底的 JSON 错误带上错 Content-Type。
func clearStreamHeaders(h http.Header) {
	h.Del("Content-Type")
	h.Del("Cache-Control")
	h.Del("Connection")
	h.Del("X-Accel-Buffering")
}

// streamReadError 标记上游响应体的读失败，与写给客户端时的写失败区分。
type streamReadError struct{ err error }

func (e *streamReadError) Error() string { return "upstream stream read: " + e.err.Error() }
func (e *streamReadError) Unwrap() error { return e.err }

// IsStreamReadError 报告流是否在上游读取侧中断。客户端写失败不属于此类。
func IsStreamReadError(err error) bool {
	var readErr *streamReadError
	return errors.As(err, &readErr)
}

// Aggregate 读取完整 SSE 流，聚合 delta.content 为单个 OpenAI chat.completion 响应。
// 分片/半行由 bufio.Reader.ReadString 处理；遇到 "data: [DONE]" 结束。
// tool_calls 以流式 delta 到达（按 index 合并：首片带 id/type/name，后续只带 arguments 片段）。
// decodeDataPayload 解析一个 data 行的载荷，返回其中所有可解析的事件对象。
//
// 正常形态是一行一个 JSON，走快路径（一次 Unmarshal，不新增分配）。上游偶尔把多个
// 事件塞进同一行，两种表现都见过：重复的 "data: " 前缀，或两个 JSON 直接相邻。
// 此前整行 Unmarshal 失败即静默丢弃，客户端看到的是缺段的回答；这里只在快路径失败
// 时才切分重试，正常流量的开销为零。
func decodeDataPayload(payload string) []map[string]any {
	var one map[string]any
	if json.Unmarshal([]byte(payload), &one) == nil {
		return []map[string]any{one}
	}
	segs := []string{payload}
	if strings.Contains(payload, "data:") {
		segs = splitDataPrefix(payload)
	}
	var out []map[string]any
	for _, seg := range segs {
		seg = strings.TrimSpace(seg)
		if seg == "" || seg == "[DONE]" {
			continue
		}
		// 用流式解码器吃掉一段里连续出现的多个 JSON（{...}{...}）。
		dec := json.NewDecoder(strings.NewReader(seg))
		for {
			var obj map[string]any
			if err := dec.Decode(&obj); err != nil {
				break
			}
			out = append(out, obj)
		}
	}
	return out
}

// splitDataPrefix 把一行里重复出现的 "data:" 前缀切开，返回各段载荷。
// 前缀后可能有一个空格，也可能没有。
func splitDataPrefix(s string) []string {
	var out []string
	for {
		i := strings.Index(s, "data:")
		if i < 0 {
			if strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
			return out
		}
		if i > 0 {
			out = append(out, s[:i])
		}
		s = strings.TrimPrefix(s[i+len("data:"):], " ")
	}
}

func Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model     string
		created       float64
		content       strings.Builder
		reasoning     strings.Builder
		role          = "assistant"
		finishReason  = "stop"
		usage         map[string]any
		gotAnyContent bool
		validEvents   int
		sawDone       bool // 上游显式发过 data: [DONE]（正常收尾）
		toolCalls     = map[int]map[string]any{}
		toolOrder     []int
		// toolSeq 缺 index 的 tool_call 的分配序号源：跨帧延续「最近分配」槽位，
		// 同帧内递增（见 mergeToolCallsChunk 注释）。
		toolSeq int
		// idIndex id → 已分配的 index：跨帧持续（延续碎片的 id 常出现在后续帧），
		// 供缺 index 时按 id 归位既有调用（见 mergeToolCallsChunk 注释）。
		idIndex = map[string]int{}
	)
	// appendContent 是「已取到正文」（gotAnyContent latch）的唯一写入点：delta 与
	// message 两路 content 都必须经此并入，规约只有一份（issue #142）——
	//   S1 空串不算「已取到正文」、不占 latch 名额：OpenAI 风格 role-only 首帧
	//      （delta.content=""）和整条空 message 帧是常态帧，若空串置位 latch，
	//      后续真正文会被 message 回退分支的 !gotAnyContent 守卫静默拒绝；
	//   S2 空串本身也无可追加字节，跳过 WriteString 与追加语义自洽。
	appendContent := func(txt string) {
		if txt == "" {
			return
		}
		content.WriteString(txt)
		gotAnyContent = true
	}
	// mergeToolCallsChunk 把一段 tool_calls 数组按 index 合并进累计表。
	// delta（流式分片，按 index 累积）与 message（非 delta 整条）共用同一合并逻辑，
	// 保证「上游给的身份/函数名不丢、arguments 拼接语义一致」。
	//
	// index 缺失兼容：OpenAI 规范要求 delta 帧的 tool_call 带 index（标记分片归属），
	// 但部分上游省略它。此前缺 index 一律归 0——多调用场景下不同 call 被合并进同一
	// index 槽，arguments 串联、name 互相覆盖（数据污染）。修法按「id 优先、lastIdx 兜底」：
	//   - 带 index → 按 index 累积（合规形态，零改动）；
	//   - 缺 index 带 id 且 id 已见过 → 延续该 id 所在 index；
	//   - 缺 index 带 id 且 id 是新的 → 开新序号（多调用不合并）；
	//   - 缺 index 无 id → 追加到最近收到碎片的 index（单个调用的延续分片无 id
	//     是标准形态），无既往则开新号。
	// 带 index 的碎片照常按 index 累积，不受影响。
	// nextToolIndex 分配下一个不冲突的缺 index 序号：从 toolSeq 起递增跳过既有
	// index（合规流的 index 是 0..N-1，缺 index 的补位不能覆盖它们）。
	nextToolIndex := func() int {
		for {
			idx := toolSeq
			toolSeq++
			if _, used := toolCalls[idx]; !used {
				return idx
			}
		}
	}
	mergeToolCallsChunk := func(tcs []any) {
		for _, tc := range tcs {
			call, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			idx := -1
			if v, ok := call["index"].(float64); ok {
				idx = int(v)
			} else if cid, _ := call["id"].(string); cid != "" {
				if mid, seen := idIndex[cid]; seen {
					idx = mid // 该 id 已归位过：延续既有调用（跨帧有效）
				} else {
					idx = nextToolIndex()
				}
			} else if len(toolOrder) > 0 {
				idx = toolOrder[len(toolOrder)-1] // 无 id 碎片：延续最近调用
			} else {
				idx = nextToolIndex()
			}
			merged, seen := toolCalls[idx]
			if !seen {
				merged = map[string]any{"index": idx}
				toolCalls[idx] = merged
				toolOrder = append(toolOrder, idx)
			}
			if cid, _ := call["id"].(string); cid != "" {
				idIndex[cid] = idx
			}
			if cid, _ := merged["id"].(string); cid != "" {
				idIndex[cid] = idx
			}
			mergeToolCallDelta(merged, call)
		}
	}
	// mergeMessageFields 把非 delta 的完整 message 内容并入聚合（message 是整条下发，
	// 非流式拼接，content 只取一次）。role/reasoning_content/tool_calls 与 delta 分支
	// 同构透出；content 同样置 gotAnyContent，与 delta 路径的 latch 语义一致
	// （一帧整条 message 之后，后续 delta 帧不重复追加）。
	mergeMessageFields := func(msg map[string]any) {
		if r2, ok := msg["role"].(string); ok && r2 != "" {
			role = r2
		}
		if txt, ok := msg["content"].(string); ok {
			appendContent(txt)
		}
		if rc, ok := msg["reasoning_content"].(string); ok {
			reasoning.WriteString(rc)
		}
		if tcs, ok := msg["tool_calls"].([]any); ok {
			mergeToolCallsChunk(tcs)
		}
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				// 上游显式结束：停止读取，DONE 之后的任何数据一律忽略。
				sawDone = true
				break
			} else {
				// 一个 data 行可能塞了多个事件（重复 data: 前缀，或两个 JSON 直接相邻）。
				// 逐条取出：某一条畸形只丢那一条，不再因为整行 Unmarshal 失败而吞掉同行的
				// 正文——那会让客户端拿到缺段的回答，且日志里只看到「空流」。
				for _, chunk := range decodeDataPayload(payload) {
					// 有效事件计数：仅解析成功的事件计入（畸形片段不计）。
					validEvents++
					if v, ok := chunk["id"].(string); ok && id == "" {
						id = v
					}
					if v, ok := chunk["model"].(string); ok && model == "" {
						model = v
					}
					if v, ok := chunk["created"].(float64); ok && created == 0 {
						created = v
					}
					if u, ok := chunk["usage"].(map[string]any); ok {
						usage = u
					}
					if ch, ok := chunk["choices"].([]any); ok {
						for _, ci := range ch {
							c, _ := ci.(map[string]any)
							if c == nil {
								continue
							}
							if fr, ok := c["finish_reason"].(string); ok && fr != "" {
								finishReason = fr
							}
							if delta, ok := c["delta"].(map[string]any); ok {
								if r2, ok := delta["role"].(string); ok && r2 != "" {
									role = r2
								}
								if txt, ok := delta["content"].(string); ok {
									appendContent(txt)
								}
								if rc, ok := delta["reasoning_content"].(string); ok {
									reasoning.WriteString(rc)
								}
								if tcs, ok := delta["tool_calls"].([]any); ok {
									mergeToolCallsChunk(tcs)
								}
							}
							// 有的上游把完整消息放在 message 里（非 delta）：
							// 整条并入（role/content/reasoning_content/tool_calls），
							// 与 delta 分支同构。delta 已取过正文（gotAnyContent）则跳过
							// （避免与 delta 路径重复拼接——PR #134 的 latch 语义）。
							if msg, ok := c["message"].(map[string]any); ok && !gotAnyContent {
								mergeMessageFields(msg)
							}
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if validEvents == 0 {
		// 上游返回 200 但没有任何有效数据事件（空流/只有 [DONE]/只有注释行）：
		// 不再合成空 content 的假成功响应，直接报错，由 handler 映射为 502 upstream_parse。
		return nil, errEmptyStream
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		// P1b：流被截断时 tool_call 的 arguments 是残缺 JSON（解析失败），不把脏参数
		// 交给客户端——残留分片会被客户端解析成非法 JSON 卡死会话。截断的两个来源：
		//   - finish_reason=="length"（模型因 max_tokens 提前中止）；
		//   - 上游连接中断（EOF 收尾但未发 data: [DONE]，sawDone=false）。
		// 完整参数原样保留（正例零改动）；空参数（无参工具）不是截断，同样保留。
		// 此前只认 finish_reason=="length"，EOF 截断的 tool_calls 残缺参数被原样下发。
		if finishReason == "length" || !sawDone {
			calls = dropTruncatedToolCalls(calls)
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		// OpenAI 非流式 usage 必含 total_tokens。上游若只发 prompt_tokens +
		// completion_tokens（部分上游末帧缺 total），网关合成补齐——否则严格按
		// schema 校验的客户端收不到 total_tokens。已有 total 或二者缺一不补
		// （不臆造：单边有值无法合成可信的 total）。
		resp["usage"] = normalizeUsageCacheAliases(ensureUsageTotal(usage))
	}
	return resp, nil
}

// ensureUsageTotal 在 usage 缺 total_tokens 但 prompt_tokens/completion_tokens 都在时
// 补齐 total = prompt + completion（通过新 map 合并，不修改原上游 map）。
// 任一缺失或已有 total 时原样返回。
func ensureUsageTotal(u map[string]any) map[string]any {
	if _, ok := u["total_tokens"]; ok {
		return u
	}
	pt, pok := num64(u["prompt_tokens"])
	ct, cok := num64(u["completion_tokens"])
	if !pok || !cok {
		return u
	}
	out := make(map[string]any, len(u)+1)
	for k, v := range u {
		out[k] = v
	}
	out["total_tokens"] = pt + ct
	return out
}

// num64 把 JSON number（float64/int64 均可）归一为 float64；非数字返回 ok=false。
//
// 注意与 payload.go 的类型 switch 语义不同：那边是翻译请求别名字段（非数字拒绝
// 整个请求），这边是聚合响应求和（非数字只跳过合成）。防后人合并两处。
func num64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// stripToolCallNames 收敛流式 tool_calls 的 name 语义为「每个 index 只出现一次」：
// 首片保留 function.name，同一 index 后续分片里的 name 键一律删除（无论上游是
// 空串还是重复非空串）。这是 OpenAI 官方流的真实形态——首帧带 name，后续帧只带
// arguments 片段、不再出现 name 键——因此是累加型与覆盖型客户端的共同祖先行为。
//
// 两类消费模型在该形态下同时正确：
//   - 累加型（官方 WorkBuddy/CodeBuddy `name += tc_function?.name || ""`）：
//     后续分片 name 键缺失 → 追加空串，累积 name 保持唯一，不再拼成 Bash×帧数（issue #82）。
//   - 覆盖型（hawklithm#2 / Grok Build `name ?? state.name` 或 `if (name) state.name = name`）：
//     后续分片 name 键缺失 → 保留已建好的首帧 name，不被空串意外清空。
//     键缺失是比空串更安全的形态：`??` 与 truthy 守卫对缺失键必然保留旧值，
//     而对空串，`??` 会误判为重设并清空工具名。
//
// seen 记录每个 index 是否已发过首片（与 name 是否非空无关）；删除是幂等的。
// 只动 function.name 键，id/type/arguments 原样透传。
func stripToolCallNames(obj map[string]any, seen map[int]bool) {
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		tcs, _ := delta["tool_calls"].([]any)
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			idx := 0
			if v, ok := tc["index"].(float64); ok {
				idx = int(v)
			}
			if seen[idx] {
				// 已发过首片：删除本分片的 name 键（存在即删，幂等）。
				if fn, _ := tc["function"].(map[string]any); fn != nil {
					delete(fn, "name")
				}
				continue
			}
			// 首现：保留 name 键原样（上游首片通常带非空 name；空 name 也照发，
			// 与 OpenAI 对「首帧无 name」的容忍一致），随后分片统一删除。
			seen[idx] = true
		}
	}
}

// normalizeFrame 以 OpenAI 流式规范白名单重建帧：仅保留标准字段，
// 剔除上游噪声（finish_reason:"" → null、空 content/refusal、空 tool_calls 列表、
// 空占位 function_call、顶层未知字段），空 delta 键一律省略，
// usage 缺失 → null，保证任意标准客户端按规范解析。
func normalizeFrame(obj map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	if _, ok := out["object"]; !ok {
		out["object"] = "chat.completion.chunk"
	}
	if _, ok := out["id"]; !ok {
		out["id"] = "chatcmpl-wb2api"
	}
	if chs, ok := obj["choices"].([]any); ok {
		nchs := make([]any, 0, len(chs))
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			nc := map[string]any{}
			if idx, ok := c["index"]; ok {
				nc["index"] = idx
			}
			delta := map[string]any{}
			if d, ok := c["delta"].(map[string]any); ok {
				if v, ok := d["role"].(string); ok && v != "" {
					delta["role"] = v
				}
				if v, ok := d["content"].(string); ok && v != "" {
					delta["content"] = v
				}
				if v, ok := d["reasoning_content"].(string); ok && v != "" {
					delta["reasoning_content"] = v
				}
				if v, ok := d["refusal"].(string); ok && v != "" {
					delta["refusal"] = v
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					delta["tool_calls"] = tcs
				}
				if fc, ok := d["function_call"]; ok && fc != nil {
					// 空占位 function_call（name/arguments 全空）视为噪声剔除
					keep := false
					if fcm, ok2 := fc.(map[string]any); ok2 {
						n, _ := fcm["name"].(string)
						a, _ := fcm["arguments"].(string)
						keep = n != "" || a != ""
					} else {
						keep = true
					}
					if keep {
						delta["function_call"] = fc
					}
				}
			}
			nc["delta"] = delta
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				nc["finish_reason"] = fr
			} else {
				nc["finish_reason"] = nil
			}
			nchs = append(nchs, nc)
		}
		out["choices"] = nchs
	}
	if rawUsage, ok := obj["usage"]; ok {
		if u, ok := rawUsage.(map[string]any); ok {
			out["usage"] = normalizeUsageCacheAliases(u)
		} else {
			out["usage"] = rawUsage
		}
	} else {
		out["usage"] = nil
	}
	return out
}

// Stream 透传上游 SSE 到 w（逐帧规范化后 flush），保证至少写一个 [DONE]。
// 调用方必须先设置过 status 200；本函数自设 SSE headers。
// 流式策略：逐帧透传（规范化已剥空 content 噪声），恢复与上游一致的平滑流式。
//
// StreamHint 变体（gateway_hint 任务）：上游 error 帧透传时附加
// error.gateway_hint 字段——message 原文不动，hint 并列补充；hintFn 返回空串
// 或 nil 时与 Stream 行为逐字节一致。
func Stream(w http.ResponseWriter, r io.Reader) error {
	return StreamHint(w, r, nil)
}

// StreamOption StreamHint 的可选行为开关。
// WithErrorFrameObserver 只做旁路观测、不改透传字节；WithMarkupRepair 会改写出站
// 帧（把正文里的原生工具调用标记还原成 delta.tool_calls，见 dsml.go）。
type StreamOption func(*streamOptions)

type streamOptions struct {
	onErrorFrame func(payload string)
	markup       *MarkupRepair
	pingEvery    time.Duration
	rescue       func() (io.Reader, func(), error)
}

// DefaultStreamPingInterval 静默多久写一个保活注释帧。
//
// 取 1 秒的依据是客户端的耐心，不是服务端的偏好：线上被标记为 interrupted 的
// 请求里，绝大多数「总耗时」与「首字节耗时」几乎相等（1264/1265、1114/1115、
// 1410/1410…），说明客户端在首字节到达前就断开了。也就是说等待窗口只有 1 秒
// 量级，间隔再长就等于从未发出过。
//
// 两处保活共用这一个值，保持「客户端任意时刻最多 1 秒收不到字节」的单一语义：
//   - handler 层（server.streamKeepAlive）：覆盖建连 + 选中账号 + 上游排队/预填，
//     也就是拿到响应头之前的整段空窗；
//   - 流内（本文件的 StreamHint）：覆盖「已开流、首内容帧未到」，例如上游先发
//     role 帧、随后长时间思考。
//
// 注释帧是 SSE 规范里的 no-op，客户端不会解析成内容；空回复救援按正文层判断，
// 同样不受影响。
const DefaultStreamPingInterval = time.Second

// WithPingInterval 覆盖静默保活间隔（<=0 使用 DefaultStreamPingInterval）。
func WithPingInterval(d time.Duration) StreamOption {
	return func(o *streamOptions) { o.pingEvery = d }
}

// WithMarkupRepair 注册「模型原生工具调用标记」修复器（见 dsml.go）。
//
// 上游在没有 tools 的请求里会把工具调用吐成标记文本，Codex 这类只执行结构化
// function_call 的客户端于是把它当普通正文落进会话历史，回合直接结束、工具一次
// 都没跑。挂上修复器后，这类标记在透传前被还原成 delta.tool_calls。
//
// 传 nil、或传入未启用的修复器（客户端没声明过 tools）→ 与 Stream 行为逐字节一致。
func WithMarkupRepair(r *MarkupRepair) StreamOption {
	return func(o *streamOptions) { o.markup = r }
}

// WithEmptyReplyRescue 注册「空回复救援」：整条流没有正文、没有工具调用，且上游以
// finish_reason=length 收尾时（思考吃光 max_tokens 的实测形态：thinking token 数正好
// 等于预算、正文零字节），调用 fn 取一条新的上游流替换掉这条空流。fn 返回的清理函数
// 在函数退出前调用，可为 nil；返回 (nil, _, nil) 表示放弃救援，行为与未接线一致。
//
// 为什么由网关做：客户端给的是偏小的 max_tokens，上游把 reasoning 与正文算在同一份
// 预算里，思考可以独吞全部额度。Agent 循环拿到 finish_reason=length + 零字节正文
// 会当成"回合正常结束但什么都没说"，比报错更难排查。
func WithEmptyReplyRescue(fn func() (io.Reader, func(), error)) StreamOption {
	return func(o *streamOptions) { o.rescue = fn }
}

// WithErrorFrameObserver 注册"上游以 error 帧报错"的观察者：透传该帧之前先用
// payload（原始 data 内容）回调一次。给调用方一个**账号处置**的挂载点——
// 上游「200 + error 帧」是真实形态（6004 限流、内容拦截、审核），此前网关在读
// 第一帧之前就把账号记成功，限流号被当成健康号；有观察者后可在流尾按帧分类处置。
func WithErrorFrameObserver(fn func(payload string)) StreamOption {
	return func(o *streamOptions) { o.onErrorFrame = fn }
}

// StreamHint 同 Stream，但上游 error 帧透出前把 hintFn(payload) 的返回值写入
// error.gateway_hint。hintFn 为 nil 或返回空串 → 原样透传（零改写）。
// 空流兜底 error 帧（"empty upstream stream"）不带 hint（网关本地故障形态
// 未覆盖，不编造）。
// opts：旁路观测开关（见 WithErrorFrameObserver），不改变任何透传字节。
func StreamHint(w http.ResponseWriter, r io.Reader, hintFn func(string) string, opts ...StreamOption) error {
	var o streamOptions
	for _, f := range opts {
		if f != nil {
			f(&o)
		}
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	// 所有响应体写入共用一把锁：保活 goroutine 与主循环不得并发写 ResponseWriter。
	var (
		writeMu   sync.Mutex
		lastWrite atomic.Int64
	)
	lastWrite.Store(time.Now().UnixNano())
	writeOut := func(s string) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if _, werr := io.WriteString(w, s); werr != nil {
			return werr
		}
		if fl != nil {
			fl.Flush()
		}
		lastWrite.Store(time.Now().UnixNano())
		return nil
	}

	// 空回复救援的预写缓冲。
	//
	// 上游把 reasoning 与正文字节算在同一个 max_tokens 里，思考可以独吞整份预算，
	// 结果是 finish_reason=length、正文零字节、没有 tool_calls。此时客户端已经收到
	// 的只有 role/finish 这类**没有信息量**的元数据帧，而流一旦写出去就无法回退。
	//
	// 做法：正文（或工具调用、错误帧）出现之前，帧先攒在 pending 里不发给客户端；
	// 一旦出现正文/工具调用/错误，立即按原序补发并转入直通（committed）。整条流
	// 始终没有正文且以 length 收尾时，攒下的元数据帧直接丢弃，改用关闭思考的重发流
	// ——客户端只会看到一份正常回复，既没有空回复也没有重复的 finish 帧。
	//
	// 延迟影响为零：缓冲区里只可能是首内容帧之前的元数据帧，客户端对它们没有可
	// 执行动作；保活 ping 走 writeOut 直写，不受缓冲影响。
	var (
		pending      []string
		committed    bool
		contentSeen  bool
		sawToolCalls bool
		finishReason string
	)
	flushPending := func() error {
		if committed {
			return nil
		}
		committed = true
		for _, p := range pending {
			if werr := writeOut(p); werr != nil {
				return werr
			}
		}
		pending = nil
		return nil
	}
	// emit 写出一帧；commit=false 且尚未落定时先进缓冲。
	emit := func(s string, commit bool) error {
		if committed {
			return writeOut(s)
		}
		if !commit {
			pending = append(pending, s)
			return nil
		}
		if werr := flushPending(); werr != nil {
			return werr
		}
		return writeOut(s)
	}
	pingEvery := o.pingEvery
	if pingEvery <= 0 {
		pingEvery = DefaultStreamPingInterval
	}
	stopPing := make(chan struct{})
	var pingWG sync.WaitGroup
	pingWG.Add(1)
	go func() {
		defer pingWG.Done()
		// 每半个间隔检查一次，使保活最迟在空闲达到 1.5 个间隔时发出。
		checkEvery := pingEvery / 2
		if checkEvery <= 0 {
			checkEvery = time.Nanosecond
		}
		t := time.NewTicker(checkEvery)
		defer t.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-t.C:
				if time.Since(time.Unix(0, lastWrite.Load())) < pingEvery {
					continue
				}
				if err := writeOut(": ping\n\n"); err != nil {
					// 客户端已走：主循环下一次写会拿到同一个错误，这里直接退出。
					return
				}
			}
		}
	}()
	defer func() {
		close(stopPing)
		pingWG.Wait()
	}()

	// toolCallSeen 跨帧记录 delta.tool_calls 里已发过首片的 index，
	// 供逐 chunk 透传时收敛 name 为「每 index 一次」（对齐 OpenAI 官方流）。
	toolCallSeen := map[int]bool{}

	// firstID 透传流的消息级 id 基准：缓存首个非空上游 id，后续帧缺失/空串时复用
	// （issue #35：同一条 SSE 消息所有帧共用一个真实 id，后台按 id 归并；此前中间帧
	// 一律补 chatcmpl-wb2api 哨兵，造成同流 id 分裂）。全流无真实 id → 才出现哨兵。
	firstID := ""

	// 标记修复（见 dsml.go）：把上游正文里的原生工具调用标记还原成 delta.tool_calls。
	// 客户端没声明过 tools 时 repair.Enabled() 为 false → 整条流零改写零分配。
	//   - synthIdx：本流已还原的调用数（合成的 index 从上游最大 index 之后接续）；
	//   - maxToolIdx：上游已用过的最大 tool_call index，避免合成的 index 与真实调用撞车；
	//   - toolFinishSent：是否已发出过 finish_reason: tool_calls，供流尾兜底。
	repair := o.markup
	synthIdx := 0
	maxToolIdx := -1
	toolFinishSent := false

	// writeSSE 原样写出一帧并 flush（正文/元数据帧：是否需要落定由调用方决定，
	// 未落定时先进空回复救援的预写缓冲）。
	writeSSE := func(payload string) error {
		return emit("data: "+payload+"\n\n", contentSeen)
	}

	// writeSSECommit 写出一帧并强制落定（错误帧、非 JSON 帧等不得被缓冲的帧）。
	writeSSECommit := func(payload string) error {
		return emit("data: "+payload+"\n\n", true)
	}

	// writeRaw 原样写出一帧（绕过 normalizeFrame）。上游 error 帧（error-passthrough）
	// 与空流错误帧需保留 error 字段，不能被白名单剥掉，故经此写出。
	// gateway_hint：上游 error 帧透出前按 hintFn 附加 error.gateway_hint 字段
	// （error 对象上加一个键，message/code/requestId 等原文不动；hintFn 为
	// nil / 空串 / 非 JSON 帧 → 原样写出，零改写）。
	writeRaw := func(payload string) error {
		if hint := frameGatewayHint(hintFn, payload); hint != "" {
			payload = attachHintToErrorFrame(payload, hint)
		}
		return writeSSECommit(payload)
	}

	// writeChunk 把一个 chunk 对象按规范白名单重建后写出并 flush，同时维护三件跨帧
	// 状态：tool_calls name 收敛（每 index 仅首片带 name）、消息 id 续传、以及上游
	// 已用过的最大 tool_call index（供标记修复接续编号）。
	// fallback 是规范化失败时的兜底原文（与修复前「marshal 失败即原样透传」一致）。
	writeChunk := func(obj map[string]any, fallback string) error {
		// 先按 index 收敛 tool_calls name（每 index 仅首片保留，后续分片删 name 键），再规范化透传。
		stripToolCallNames(obj, toolCallSeen)
		// id 续传：首帧非空真实 id 缓存；后续帧缺 id / 空 id 一律用缓存值，
		// 有自己 id 的帧保持原样（不同流分裂的帧允许各自 id）。
		if firstID == "" {
			if v, ok := obj["id"].(string); ok && v != "" {
				firstID = v
			}
		} else if v, ok := obj["id"].(string); !ok || v == "" {
			obj["id"] = firstID
		}
		if chs, ok := obj["choices"].([]any); ok {
			for _, ci := range chs {
				c, _ := ci.(map[string]any)
				if c == nil {
					continue
				}
				d, _ := c["delta"].(map[string]any)
				if d == nil {
					continue
				}
				tcs, _ := d["tool_calls"].([]any)
				for _, tci := range tcs {
					tc, _ := tci.(map[string]any)
					if tc == nil {
						continue
					}
					if v, ok := tc["index"].(float64); ok && int(v) > maxToolIdx {
						maxToolIdx = int(v)
					}
				}
			}
		}
		raw, err := json.Marshal(normalizeFrame(obj))
		if err != nil {
			return writeSSECommit(fallback)
		}
		return writeSSE(string(raw))
	}

	// repairFrame 在透传前跑标记修复，并把一帧展开成「可能多帧」：正文帧（标记被摘掉
	// 后的可见文本）+ N 个 delta.tool_calls 帧 + 收尾 finish_reason 帧。没有命中标记
	// 时原样返回入参帧（单元素），与修复前逐字节一致。
	repairFrame := func(obj map[string]any) []map[string]any {
		if !repair.Enabled() {
			return []map[string]any{obj}
		}
		chs, ok := obj["choices"].([]any)
		if !ok {
			return []map[string]any{obj}
		}
		var calls []MarkupCall
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			d, ok := c["delta"].(map[string]any)
			if !ok {
				continue
			}
			txt, ok := d["content"].(string)
			if !ok || txt == "" {
				continue
			}
			emit, got := repair.Feed(txt)
			calls = append(calls, got...)
			if emit == "" {
				delete(d, "content")
			} else {
				d["content"] = emit
			}
		}
		if len(calls) == 0 {
			// 本帧没命中标记；但本流此前还原过调用 → 把上游的 stop 收尾改写成
			// tool_calls，客户端据此知道「回合以工具调用结束」而不是「模型只说了话」。
			if synthIdx > 0 && rewriteFinishReason(obj, "tool_calls") {
				toolFinishSent = true
			}
			return []map[string]any{obj}
		}
		// 命中：finish_reason 必须挪到工具调用帧之后，否则客户端在读到调用之前就
		// 认为回合已经结束。注意「本帧本来没有 finish_reason」时**不得**补一帧收尾
		// ——上游的收尾帧随后就到，补了会变成两个 finish_reason（后续那个由
		// rewriteFinishReason 改写）。
		fr := takeFinishReason(obj)
		out := []map[string]any{obj}
		for _, call := range calls {
			idx := maxToolIdx + 1 + synthIdx
			synthIdx++
			out = append(out, markupCallFrame(obj, idx, call))
		}
		if fr != "" {
			if fr == "stop" {
				fr = "tool_calls"
			}
			out = append(out, finishOnlyFrame(obj, fr))
			toolFinishSent = fr == "tool_calls"
		}
		return out
	}

	// writeFrame 把 payload 按规范白名单重建后以 data: 帧写出并 flush。
	// 仅 JSON 解析成功时计数记为一次有效转发（JSON 解析失败照常降级原样写出，但不计数）。
	writeFrame := func(payload string) (int, error) {
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			// 非 JSON 帧：原样降级写出，不计数（与修复前一致）。形态不可辨识，
			// 按"已落定"处理，不参与空回复救援（宁可放弃救援也不误吞客户端该看到的东西）。
			if werr := writeSSECommit(payload); werr != nil {
				return 0, werr
			}
			return 0, nil
		}
		// 空回复救援的判定输入（只看帧结构，不改写字节）：
		//   - 正文/工具调用一旦出现 → contentSeen 落定，此后全部直通；
		//   - finish_reason 记下来，收尾时判是否 length（预算被截断）。
		if frameHasPayload(obj) {
			contentSeen = true
		}
		if !sawToolCalls && frameHasToolCalls(obj) {
			sawToolCalls = true
			contentSeen = true
		}
		if fr := firstFinishReason(obj); fr != "" {
			finishReason = fr
		}
		// 失败帧有两个形状：带 error 键的，以及顶层 {code,msg}（无 error 无 choices，
		// 11140 request illegal / 6021 queue.full 的实测形态）。两者必须走同一路
		// 首帧否决判据——只认 error 键会漏掉后者，让本可换号的失败被当成正常帧透传。
		//
		// 首帧否决：还没写过任何字节时，失败帧不该透传——这是唯一还能换号的时机
		// （写出第一字节即提交响应头，之后只能 200+error 帧透给客户端）。是否值得
		// 换号由 firstFrameRejectable 把关：6021 队列满 / 请求体畸形这类换号也没用的
		// 类别继续走下面的透传，避免把无效请求放大 N 倍打上游。
		_, hasErr := obj["error"]
		if hasErr || isBareErrorFrame(obj) {
			if !committed && !contentSeen && !sawToolCalls {
				if k := Classify(http.StatusOK, payload); k != ErrNone && firstFrameRejectable(k) {
					// 观测不因否决而断：账号处置虽然交给上层接手，但观察者（请求日志、
					// 测试探针）拿到的失败帧原文必须与透传路径一致，否则这类失败会
					// 在观测面上凭空消失。
					if o.onErrorFrame != nil {
						o.onErrorFrame(payload)
					}
					clearStreamHeaders(h)
					return 0, &FirstFrameRejected{Kind: k, Body: payload}
				}
			}
			// 上游错误帧透传（error-passthrough）：带 error 键的帧**原样写出**，不走
			// normalizeFrame 白名单——白名单会剥掉 error 字段，客户端就看不到上游
			// code/msg/requestId。error.message 即上游原文（如 6004 限流、审核拦截），
			// 计入有效帧（避免误判空流补写 "empty upstream stream"）。
			// 旁路通知（账号处置用）：原始 payload 交给观察者，透传字节不变。
			if o.onErrorFrame != nil {
				o.onErrorFrame(payload)
			}
			if werr := writeRaw(payload); werr != nil {
				return 0, werr
			}
			return 1, nil
		}
		for _, ch := range repairFrame(obj) {
			if werr := writeChunk(ch, payload); werr != nil {
				return 0, werr
			}
		}
		return 1, nil
	}

	// pump 读完一条上游流并转发所有帧，返回有效帧数与读错误。
	// [DONE] 不在此写出——统一在函数末尾写，保证救援换流后仍恰好一个。
	pump := func(rd io.Reader) (int, error) {
		br := bufio.NewReaderSize(rd, 64*1024)
		frames := 0
	readLoop:
		for {
			line, err := br.ReadString('\n')
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(trimmed, "data: [DONE]"):
				// 上游显式结束：停止读取，DONE 之后的任何数据（含垃圾帧）一律不再透传。
				// [DONE] 统一在循环结束后写出，保证恰好一个。
				break readLoop
			case strings.HasPrefix(trimmed, "data: "):
				n, werr := writeFrame(strings.TrimPrefix(trimmed, "data: "))
				frames += n
				if werr != nil {
					return frames, werr
				}
			case trimmed != "":
				// 注释帧原样透传并补足帧尾空行；下游 Messages/Responses 包装层
				// 按空行分帧，缺少第二个换行会把注释与后续 data 错拼成一帧。
				if strings.HasPrefix(trimmed, ":") {
					line += "\n"
				}
				if werr := writeOut(line); werr != nil {
					return frames, werr
				}
			}
			// 空行（帧分隔）吞掉：本函数自产 "\n\n"
			if err != nil {
				if err == io.EOF {
					break
				}
				// HTTP 头已发出，补明确的错误终态；仍返回读错误供账号与日志处置。
				// 走 commit 路径：错误终态绝不能留在预写缓冲里被救援逻辑丢弃。
				_ = writeSSECommit(`{"error":{"message":"upstream stream interrupted","type":"upstream_error","code":"upstream_parse"}}`)
				_ = writeOut("data: [DONE]\n\n")
				return frames, &streamReadError{err: err}
			}
		}
		return frames, nil
	}

	validFrames, pumpErr := pump(r)
	if pumpErr != nil {
		return pumpErr
	}

	// 空回复救援：整条流没有正文、没有工具调用，且以 finish_reason=length 收尾，
	// 即「思考吃光 max_tokens」形态。此时已攒下的只有 role/finish 这类无信息量的
	// 元数据帧（一个字节都没发给客户端），丢弃它们换成关闭思考的同额度重发流，
	// 客户端只会看到一份正常回复。救援失败/未提供 → 按原样补发缓冲，行为与接线前一致。
	if !committed && o.rescue != nil && finishReason == "length" && !sawToolCalls {
		if rd2, clean, rerr := o.rescue(); rerr == nil && rd2 != nil {
			if clean != nil {
				defer clean()
			}
			// 上一跳的元数据帧作废：救援流自带 role/开头帧。
			pending = pending[:0]
			committed = true
			contentSeen = false
			finishReason = ""
			// 空流判定以救援流为准（救援流要是也是空的，那就是真的空）。
			validFrames, pumpErr = pump(rd2)
			if pumpErr != nil {
				_ = writeOut("data: [DONE]\n\n")
				return pumpErr
			}
		}
	}
	if !committed {
		if werr := flushPending(); werr != nil {
			return werr
		}
	}
	// 标记修复的尾部收尾：流结束时仍未判定的字节一律原文回吐（绝不吞字节，未闭合
	// 的块连起始标记一起交还）；若本流还原过调用、但上游从未给出收尾帧，补一帧
	// finish_reason: tool_calls，保证客户端不会把「工具调用回合」读成「模型只说了话」。
	if repair.Enabled() {
		if tail := repair.Flush(); tail != "" {
			tailObj := map[string]any{
				"object": "chat.completion.chunk",
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"content": tail},
				}},
			}
			if werr := writeChunk(tailObj, ""); werr != nil {
				return werr
			}
			validFrames++
		}
		if synthIdx > 0 && !toolFinishSent {
			if werr := writeChunk(finishOnlyFrame(map[string]any{}, "tool_calls"), ""); werr != nil {
				return werr
			}
		}
	}
	// 空流（0 有效帧）：先写一帧 error（绕过 normalizeFrame 原样保留 error 字段），
	// 再补 [DONE] 保证客户端能正常收尾，并返回非 nil error 供调用方记录。
	// 网关本地空流兜底帧走 hintFn=nil 的直写路径：该形态未覆盖（不编造 hint），
	// 且 writeRaw 的 hintFn 闭包在空流路径下可能携带上一帧的上下文造成误配。
	if validFrames == 0 {
		_ = writeRaw(`{"error":{"message":"empty upstream stream","type":"upstream_error","code":"upstream_parse"}}`)
	}
	// 保证恰好写一个 [DONE]（上游漏发时兜底补上）。
	if err := writeOut("data: [DONE]\n\n"); err != nil {
		return err
	}
	if validFrames == 0 {
		return errEmptyStream
	}
	return nil
}

// frameHasPayload 该帧是否携带客户端可见内容（正文 / 工具调用 / 非流式 message）。
// 空回复救援只认「整条流一个字节正文都没有」，因此只要出现其中任一形态即落定。
func frameHasPayload(obj map[string]any) bool {
	chs, ok := obj["choices"].([]any)
	if !ok {
		return false
	}
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if d, ok := c["delta"].(map[string]any); ok {
			if s, ok := d["content"].(string); ok && s != "" {
				return true
			}
		}
		if m, ok := c["message"].(map[string]any); ok {
			if s, ok := m["content"].(string); ok && s != "" {
				return true
			}
		}
	}
	return false
}

// frameHasToolCalls 该帧是否携带工具调用（delta 或 message 形态）。
// 有工具调用的回合即使正文为空也是合法终态，不触发空回复救援。
func frameHasToolCalls(obj map[string]any) bool {
	chs, ok := obj["choices"].([]any)
	if !ok {
		return false
	}
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if d, ok := c["delta"].(map[string]any); ok {
			if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
				return true
			}
		}
		if m, ok := c["message"].(map[string]any); ok {
			if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
				return true
			}
		}
	}
	return false
}

// firstFinishReason 取第一个非空 finish_reason（只读取，不改写帧内容）。
func firstFinishReason(obj map[string]any) string {
	chs, _ := obj["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if fr, ok := c["finish_reason"].(string); ok && fr != "" {
			return fr
		}
	}
	return ""
}

// isBareErrorFrame 形状判定：顶层 {code: 数字, msg: 字符串}，且没有 choices、没有
// error 对象。上游 11140 安全审核、6021 队列满的实测形态就是这种「200 内嵌失败帧」
// ——此前只认 error 键，这类帧会被当正常帧透传，账号也不做处置。
//
// 宁缺勿滥：必须同时满足四个条件才判为失败帧（code 是数字、msg 是字符串、无
// choices、无 error），避免把带 code 的正常数据帧误判成错误。
func isBareErrorFrame(obj map[string]any) bool {
	if _, ok := obj["error"]; ok {
		return false
	}
	if _, ok := obj["choices"]; ok {
		return false
	}
	if _, ok := obj["code"].(float64); !ok {
		return false
	}
	msg, ok := obj["msg"].(string)
	return ok && strings.TrimSpace(msg) != ""
}

// EmptyTruncatedReply 报告聚合响应是否属于「预算被截断且正文为空」：
// finish_reason 为 length、正文零字节、没有 tool_calls。非流式空回复救援只认这一
// 形态——有正文的截断是客户端额度语义的正常结果，不动；有工具调用的回合是合法终态。
func EmptyTruncatedReply(resp map[string]any) bool {
	if resp == nil {
		return false
	}
	chs, ok := resp["choices"].([]any)
	if !ok || len(chs) == 0 {
		return false
	}
	for _, ci := range chs {
		c, ok := ci.(map[string]any)
		if !ok {
			return false
		}
		if fr, _ := c["finish_reason"].(string); fr != "length" {
			return false
		}
		m, ok := c["message"].(map[string]any)
		if !ok {
			return false
		}
		if s, _ := m["content"].(string); s != "" {
			return false
		}
		if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
			return false
		}
	}
	return true
}

// frameGatewayHint 取 error 帧的 gateway_hint（hintFn 缺失/异常返回空串 → 不附加）。
func frameGatewayHint(hintFn func(string) string, payload string) string {
	if hintFn == nil {
		return ""
	}
	// panic 隔离：hint 判定是补充功能，任何实现缺陷不得击穿流透传主路径。
	defer func() { _ = recover() }()
	return strings.TrimSpace(hintFn(payload))
}

// attachHintToErrorFrame 在 error 帧的 error 对象上附加 gateway_hint 字段。
// message/code/requestId 等既有键原样保留（只加不改）；非 JSON / 无 error 对象 →
// payload 原样返回（宁可不加 hint 也不破坏原文透传）。
func attachHintToErrorFrame(payload, hint string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return payload
	}
	e, ok := obj["error"].(map[string]any)
	if !ok {
		return payload
	}
	e["gateway_hint"] = hint
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return string(out)
}

// takeFinishReason 取出并删除所有 choice 的 finish_reason，返回第一个非空值。
// 标记修复把一帧拆成多帧时需要它：finish_reason 必须落在工具调用帧之后，否则
// 客户端在读到调用之前就认为回合结束了。
func takeFinishReason(obj map[string]any) string {
	fr := ""
	chs, _ := obj["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if v, ok := c["finish_reason"].(string); ok && v != "" && fr == "" {
			fr = v
		}
		delete(c, "finish_reason")
	}
	return fr
}

// rewriteFinishReason 把 choice 上「已有且为 stop」的 finish_reason 改写成 want。
// 本来就没有 finish_reason 的中间帧一律不动（绝不凭空造收尾），已有其他值
// （length / content_filter 等）时也不动——那些收尾语义更需要被客户端看到。
// 返回是否发生了改写。
func rewriteFinishReason(obj map[string]any, want string) bool {
	changed := false
	chs, _ := obj["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if v, ok := c["finish_reason"].(string); ok && v == "stop" {
			c["finish_reason"] = want
			changed = true
		}
	}
	return changed
}

// markupCallFrame 以 src 帧为模板，构造一帧只带单个 delta.tool_calls 的 chunk。
// id 不在这里写：交给 writeChunk 的 id 续传逻辑补齐（src 帧先写出，firstID 已就位）。
func markupCallFrame(src map[string]any, idx int, call MarkupCall) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": markupToolCalls([]MarkupCall{call}, idx),
			},
		}},
	}
	for _, k := range []string{"created", "model"} {
		if v, ok := src[k]; ok && v != nil {
			frame[k] = v
		}
	}
	return frame
}

// finishOnlyFrame 构造一帧只带 finish_reason 的收尾 chunk（空 delta）。
func finishOnlyFrame(src map[string]any, finish string) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finish,
		}},
	}
	for _, k := range []string{"created", "model"} {
		if v, ok := src[k]; ok && v != nil {
			frame[k] = v
		}
	}
	return frame
}
