package upstream

import "encoding/json"

// 官方 CLI 内核会给每个 chat 请求自动补两个字段（客户端没显式给时）：
//
//	cache_retention:   "long"  —— 前缀缓存保留档，1 小时；未配置时按会话 latch
//	reasoning_summary: "auto"  —— 只在请求带思考档位时补
//
// 这两个字段属于「客户端形态」而非 agent 行为：同一个模型、同一段提示词，官方客户端
// 总会带上它们，缺了就是可判别的差异。这里在转发前补齐，客户端已显式给出则不覆盖。
const (
	defaultCacheRetention   = "long"
	defaultReasoningSummary = "auto"
)

// applyChatDefaults 给 chat 请求体补上述两个默认字段；body 不是 JSON 对象时原样返回。
//
// 放在 prepareBody 的最后一步：它之前的清洗/密钥注入可能重建 body，先补会被覆盖。
func applyChatDefaults(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return body
	}
	changed := false
	if _, ok := obj["cache_retention"]; !ok {
		obj["cache_retention"] = defaultCacheRetention
		changed = true
	}
	if _, ok := obj["reasoning_summary"]; !ok && requestHasThinking(obj) {
		obj["reasoning_summary"] = defaultReasoningSummary
		changed = true
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// requestHasThinking 判断请求是否带思考档位：官方只在「有 thinking」时补 reasoning_summary。
// 两种写法都认：顶层 `reasoning_effort`，或嵌套 `reasoning.effort`。
func requestHasThinking(obj map[string]any) bool {
	if _, ok := obj["reasoning_effort"]; ok {
		return true
	}
	if r, ok := obj["reasoning"].(map[string]any); ok {
		if _, ok := r["effort"]; ok {
			return true
		}
	}
	return false
}
