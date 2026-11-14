package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// chatExtras 给 chat 请求体补两类「客户端形态」字段：
//
//  1. messages[] 的逐条元数据（conversationRequestId / messageId / stepSeq / agent /
//     model / requestModelId / requestModelName / traceId）——官方从第二轮起每条
//     assistant / tool 消息都带；
//  2. extra_vars.growthEvent —— 客户端把本轮的行为事件内联在请求里（与独立上报双通道）。
//
// 两个动作都只在字段缺失时补，客户端显式给出的一律保留。
func applyChatExtras(body []byte, meta ChatMeta) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return body
	}
	model, _ := obj["model"].(string)
	modelName := model
	changed := injectMessageMetadata(obj, meta, model, modelName)
	if injectGrowthEvent(obj, meta, model, modelName) {
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

// injectMessageMetadata 给 assistant / tool 消息补元数据，返回是否有改动。
//
// messageId 用「角色 + 内容」哈希派生而不是随机生成：同一条历史消息在多轮请求里会
// 反复出现，随机值会让每次请求的字节都不同（上游前缀缓存直接失效）。派生值稳定，
// 同一会话同一消息永远同值。
func injectMessageMetadata(obj map[string]any, meta ChatMeta, model, modelName string) bool {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return false
	}
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		convReqID = meta.ConversationID
	}
	if convReqID == "" {
		return false
	}
	traceID := meta.TraceID
	if traceID == "" {
		traceID = convReqID
	}
	step := 0
	changed := false
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role != "assistant" && role != "tool" {
			continue
		}
		step++
		fill := map[string]any{
			"conversationRequestId": convReqID,
			"stepSeq":               step,
			"agent":                 "cli",
			"traceId":               traceID,
		}
		if model != "" {
			fill["model"] = model
			fill["requestModelId"] = model
		}
		if modelName != "" {
			fill["requestModelName"] = modelName
		}
		if _, ok := m["messageId"]; !ok {
			fill["messageId"] = deriveMessageID(role, m)
		}
		for k, v := range fill {
			if _, exists := m[k]; exists {
				continue
			}
			m[k] = v
			changed = true
		}
	}
	return changed
}

// deriveMessageID 由消息内容派生稳定的 32 位小写 hex（官方 messageId 就是这种形态）。
func deriveMessageID(role string, m map[string]any) string {
	h := sha256.New()
	h.Write([]byte(role))
	h.Write([]byte{0})
	if c, ok := m["content"]; ok {
		if raw, err := json.Marshal(c); err == nil {
			h.Write(raw)
		}
	}
	// tool 消息靠 tool_call_id 区分（同一内容可能被不同调用复用）。
	if id, ok := m["tool_call_id"].(string); ok && id != "" {
		h.Write([]byte{0})
		h.Write([]byte(id))
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}

// injectGrowthEvent 把本轮的行为事件内联进 extra_vars.growthEvent（字符串形态，
// 与客户端一致：一条序列化后的 JSON 数组）。已有该字段时不动。
func injectGrowthEvent(obj map[string]any, meta ChatMeta, model, modelName string) bool {
	var extra map[string]any
	if v, ok := obj["extra_vars"].(map[string]any); ok {
		extra = v
	}
	if extra != nil {
		if _, ok := extra["growthEvent"]; ok {
			return false
		}
	}
	id := meta.ConversationRequestID
	if id == "" {
		id = meta.ConversationID
	}
	if id == "" {
		return false
	}
	if modelName == "" {
		modelName = model
	}
	payload := []map[string]any{{
		"eventCode": "chat_request_send",
		"id":        id,
		"extra": map[string]any{
			"command":          "",
			"inputLength":      lastUserInputLength(obj),
			"mode":             "craft",
			"requestModelId":   model,
			"requestModelName": modelName,
		},
	}}
	raw, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if extra == nil {
		extra = map[string]any{}
		obj["extra_vars"] = extra
	}
	extra["growthEvent"] = string(raw)
	return true
}

// lastUserInputLength 取最后一条 user 消息的文本长度，作为事件的 inputLength。
func lastUserInputLength(obj map[string]any) int {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return 0
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "user" {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			return len(c)
		case []any:
			n := 0
			for _, part := range c {
				if pm, ok := part.(map[string]any); ok {
					if t, ok := pm["text"].(string); ok {
						n += len(t)
					}
				}
			}
			return n
		}
	}
	return 0
}
