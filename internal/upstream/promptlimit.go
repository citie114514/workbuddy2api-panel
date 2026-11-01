// promptlimit.go 11115「prompt is too long」的可修复重发决策。
//
// 背景（实测 2026-11-01，global / cn 两域对照）：
//
//	上游的上下文校验口径是 **prompt_tokens + 输出预留 > 模型窗口**，而不是只看
//	prompt。输出预留的取值：
//	  - 客户端显式给 max_tokens（且不超过上游 clamp 上限）→ 预留就是该值；
//	  - 客户端没给（或给了会被 clamp 的大值）→ 预留取模型默认值。实测
//	    global:deepseek-v4.1-flash 默认 384000、cn:deepseek-v4.1-flash 默认 128000。
//
//	于是同样 ~870k token 的会话：pi / Claude Code / dsh 都显式发 max_tokens，
//	预留被压到几万，正常通过；而只发 OpenAI 兼容体、不带 max_tokens 的客户端
//	（如昔涟 Cyrene 走 ai-sdk 路径）会凭空背上 384k 预留，被硬顶成 400 ——
//	报文形如 `prompt is too long: 868040 tokens > 1048576 maximum`。
//
// 修复思路：不猜、不估算。上游报文里的 used 是它自己的记账数（prompt + 预留），
// 与 limit 一起给出了精确的越界量；只要知道「这次预留是多少」，prompt 就能反推。
//   - 预留可信（客户端显式且低于上游 clamp 上限）→ prompt = used - 预留，一次
//     就能算出刚好放得下的 max_tokens；
//   - 预留不可信（没给 / 给得太大被 clamp）→ 先发一个明确的、远低于 clamp 上限的
//     小预留，下一次 11115 的 used 就带着真实 prompt，第二次精确收敛。
//
// 只在上游已经判失败的路径上动作：本来必然 400 的请求变成可用，不影响任何
// 正常请求的语义；收窄后的输出上限是「模型窗口还装得下多少」的如实值。
package upstream

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

const (
	// promptTooLongSafety 预算安全余量：上游的 token 计数与模型实际分词之间没有
	// 承诺，留一点余量避免「算得刚好、边界上差一个 token 又 400」来回打转。
	promptTooLongSafety int64 = 1024
	// promptTooLongFloor 给出低于该值的输出上限没有意义（连一段正常回复都不够，
	// 交给客户端压缩上下文更诚实）。
	promptTooLongFloor int64 = 256
	// promptTooLongBlindMax 预留不可信时先发的明确预留：远低于实测的 clamp 上限
	// （cn 128000 / global 384000），保证下一次 11115 能精确反推 prompt。
	promptTooLongBlindMax int64 = 32768
)

// promptTooLongRe 匹配上游 11115 报文的两个数：`prompt is too long: 868040 tokens > 1048576 maximum`。
// 容忍 JSON 转义（>）与多余空白；只认这条固定句式，避免把别的错误误读成越界。
var promptTooLongRe = regexp.MustCompile(`(?i)prompt is too long[^0-9]*([0-9]+)\s*tokens?\s*(?:>|\\u003e|&gt;)\s*([0-9]+)`)

// ParsePromptTooLong 解析 11115 报文里的 used（上游口径的「输入+输出预留」）与
// limit（模型窗口）。body 可以是上游原始 JSON，也可以是已解码的 message 文本；
// 匹配不到返回 ok=false（调用方走原有透传，不猜）。
func ParsePromptTooLong(body string) (used, limit int64, ok bool) {
	if body == "" {
		return 0, 0, false
	}
	m := promptTooLongRe.FindStringSubmatch(body)
	if len(m) != 3 {
		return 0, 0, false
	}
	u, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || u <= 0 {
		return 0, 0, false
	}
	l, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil || l <= 0 {
		return 0, 0, false
	}
	return u, l, true
}

// MaxTokensInBody 取请求体里客户端声明的输出上限（max_tokens 优先，其次
// max_completion_tokens 别名）。非正值/非数值/缺字段一律 ok=false。
func MaxTokensInBody(body []byte) (int64, bool) {
	if len(body) == 0 {
		return 0, false
	}
	var peek struct {
		MaxTokens           json.Number `json:"max_tokens"`
		MaxCompletionTokens json.Number `json:"max_completion_tokens"`
	}
	if err := json.Unmarshal(body, &peek); err != nil {
		return 0, false
	}
	for _, n := range []json.Number{peek.MaxTokens, peek.MaxCompletionTokens} {
		if n == "" {
			continue
		}
		v, err := n.Int64()
		if err == nil && v > 0 {
			return v, true
		}
	}
	return 0, false
}

// WithMaxTokens 把请求体的输出上限改成 n（max_tokens 字段；max_completion_tokens
// 别名由 payload.go 的 translateMaxCompletionTokens 在显式 max_tokens 在场时删除）。
// body 不可解析时原样返回（不二次错误化，与其它改写函数的口径一致）。
func WithMaxTokens(body []byte, n int64) []byte {
	if len(body) == 0 || n <= 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	obj["max_tokens"] = n
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// PlanPromptTooLongRepair 依据一次 11115 的 used/limit 决定重发用的 max_tokens。
//
//	sentMax/explicit：本次请求体里客户端声明的输出上限（没有 → explicit=false）。
//	返回的 n 是重发时应写入的 max_tokens；ok=false 表示重发也救不回来
//	（输入本身已贴顶 / 没有值得给的输出余量），调用方应原样透传上游错误。
//
// 只有「上游确实按 sentMax 记账」的值才能反推 prompt。上游会把超过模型上限的
// max_tokens 静默换成自己的默认预留（实测 cn 128000 / global 384000），而客户端
// 声明的大值与默认值在报文里无法区分——所以只信任远低于任一 clamp 的显式值：
//   - sentMax ≤ blind（32768）：used - sentMax 就是真实 prompt，一跳算准预算
//     （pi / Claude Code / dsh 这类客户端就在这一档）；
//   - 其余（没给 max_tokens，或给了可能被 clamp 的大值）：先发 blind 这个明确值。
//     绝大多数场景（输入距窗口还有 32k 以上余量）这一跳就成功；真的贴顶时，
//     下一跳的报文落进上一档，第二跳精确收敛。
//
// 两档都只在「预算确实比本次更小」时动手，绝不原地重试。
func PlanPromptTooLongRepair(used, limit, sentMax int64, explicit bool) (int64, bool) {
	if used <= 0 || limit <= 0 {
		return 0, false
	}
	if explicit && sentMax > 0 && sentMax <= promptTooLongBlindMax {
		prompt := used - sentMax
		if prompt <= 0 {
			return 0, false // 报文自相矛盾，不猜
		}
		budget := limit - prompt - promptTooLongSafety
		if budget < promptTooLongFloor || budget >= sentMax {
			// 贴顶（没有值得给的输出余量），或算出不小于本次值（没有进展）。
			return 0, false
		}
		return budget, true
	}
	return promptTooLongBlindMax, true
}

// PromptTooLongSummaryOf 从 11115 报文里取出「上游口径的输入+预留 / 窗口」摘要，
// 供日志一行说明这次重发是怎么算出来的；解析不到返回空串（不编造）。
func PromptTooLongSummaryOf(body string) string {
	used, limit, ok := ParsePromptTooLong(body)
	if !ok {
		return ""
	}
	return "prompt+reserve=" + strconv.FormatInt(used, 10) +
		" limit=" + strconv.FormatInt(limit, 10)
}

// isPromptTooLongBody 兼容旧调用面：报文里带 11115 句式即视为越界错误。
// 供需要「只看 body 不看分类」的测试与防御路径使用。
func isPromptTooLongBody(body string) bool {
	if strings.TrimSpace(body) == "" {
		return false
	}
	_, _, ok := ParsePromptTooLong(body)
	if ok {
		return true
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "prompt is too long")
}
