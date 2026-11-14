package upstream

import (
	"encoding/json"
	"testing"
)

const extrasTestBody = `{
  "model": "fast-model",
  "messages": [
    {"role":"system","content":"sys"},
    {"role":"user","content":"hi"},
    {"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{}"}}]},
    {"role":"tool","tool_call_id":"call_1","content":"hi"},
    {"role":"user","content":"again"}
  ]
}`

var extrasTestMeta = ChatMeta{
	ConversationID:        "01a1b18f-e32c-742e-96b6-4c531adf8cec",
	ConversationRequestID: "01a1b191ff2a7c7b1e5f0c3f2d1e0a1b",
	TraceID:               "469e36fce22e4603e334ec8f4fe2949d",
}

// TestApplyChatExtrasMessageMetadata assistant / tool 消息补齐元数据，system/user 不动。
func TestApplyChatExtrasMessageMetadata(t *testing.T) {
	out := applyChatExtras([]byte(extrasTestBody), extrasTestMeta)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := obj["messages"].([]any)
	for i, raw := range msgs {
		m := raw.(map[string]any)
		role := m["role"].(string)
		_, has := m["conversationRequestId"]
		switch role {
		case "system", "user":
			if has {
				t.Errorf("messages[%d] role=%s 不该带元数据", i, role)
			}
		default:
			if !has {
				t.Fatalf("messages[%d] role=%s 缺 conversationRequestId", i, role)
			}
			if got := m["conversationRequestId"]; got != extrasTestMeta.ConversationRequestID {
				t.Errorf("conversationRequestId = %v", got)
			}
			if got := m["agent"]; got != "cli" {
				t.Errorf("agent = %v want cli", got)
			}
			if got := m["traceId"]; got != extrasTestMeta.TraceID {
				t.Errorf("traceId = %v", got)
			}
			if id, _ := m["messageId"].(string); len(id) != 32 {
				t.Errorf("messageId 不是 32 位 hex: %q", id)
			}
		}
	}
	// stepSeq 从 1 开始按 assistant/tool 顺序递增。
	// JSON 反序列化后数字是 float64。
	if got, _ := msgs[2].(map[string]any)["stepSeq"].(float64); got != 1 {
		t.Errorf("assistant stepSeq = %v want 1", got)
	}
	if got, _ := msgs[3].(map[string]any)["stepSeq"].(float64); got != 2 {
		t.Errorf("tool stepSeq = %v want 2", got)
	}
}

// TestApplyChatExtrasMessageIDStable 同一条历史消息在多轮请求里必须得到同一个 messageId，
// 否则每次请求字节都变，上游前缀缓存会整段失效。
func TestApplyChatExtrasMessageIDStable(t *testing.T) {
	first := applyChatExtras([]byte(extrasTestBody), extrasTestMeta)
	// 追加一轮后再次转发：历史消息的 messageId 不得变化。
	longer := `{
      "model": "fast-model",
      "messages": [
        {"role":"system","content":"sys"},
        {"role":"user","content":"hi"},
        {"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{}"}}]},
        {"role":"tool","tool_call_id":"call_1","content":"hi"},
        {"role":"user","content":"again"}
      ]
    }`
	second := applyChatExtras([]byte(longer), extrasTestMeta)

	var a, b map[string]any
	_ = json.Unmarshal(first, &a)
	_ = json.Unmarshal(second, &b)
	ma := a["messages"].([]any)[2].(map[string]any)["messageId"]
	mb := b["messages"].([]any)[2].(map[string]any)["messageId"]
	if ma != mb {
		t.Errorf("同一消息的 messageId 不稳定: %v vs %v", ma, mb)
	}
}

// TestApplyChatExtrasKeepsExplicit 客户端已给出的字段不覆盖。
func TestApplyChatExtrasKeepsExplicit(t *testing.T) {
	in := `{"model":"m","extra_vars":{"growthEvent":"[]"},
	  "messages":[{"role":"assistant","content":"x","messageId":"keepme","agent":"custom"}]}`
	out := applyChatExtras([]byte(in), extrasTestMeta)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if got := obj["extra_vars"].(map[string]any)["growthEvent"]; got != "[]" {
		t.Errorf("growthEvent 被覆盖: %v", got)
	}
	m := obj["messages"].([]any)[0].(map[string]any)
	if m["messageId"] != "keepme" || m["agent"] != "custom" {
		t.Errorf("显式字段被覆盖: %v", m)
	}
}

// TestApplyChatExtrasGrowthEvent 单轮请求也要内联行为事件，且是字符串形态的 JSON 数组。
func TestApplyChatExtrasGrowthEvent(t *testing.T) {
	in := `{"model":"fast-model","messages":[{"role":"user","content":"say ok"}]}`
	out := applyChatExtras([]byte(in), extrasTestMeta)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	ev, ok := obj["extra_vars"].(map[string]any)["growthEvent"].(string)
	if !ok {
		t.Fatalf("growthEvent 缺失或不是字符串")
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(ev), &arr); err != nil {
		t.Fatalf("growthEvent 不是 JSON 数组: %v", err)
	}
	if len(arr) != 1 || arr[0]["eventCode"] != "chat_request_send" {
		t.Fatalf("事件内容不对: %v", arr)
	}
	if arr[0]["id"] != extrasTestMeta.ConversationRequestID {
		t.Errorf("事件 id = %v", arr[0]["id"])
	}
	extra := arr[0]["extra"].(map[string]any)
	if extra["inputLength"].(float64) != float64(len("say ok")) {
		t.Errorf("inputLength = %v want %d", extra["inputLength"], len("say ok"))
	}
}

// TestApplyChatExtrasNonObject 非对象负载原样返回。
func TestApplyChatExtrasNonObject(t *testing.T) {
	for _, in := range []string{"", "not json", `[1,2]`} {
		if out := applyChatExtras([]byte(in), extrasTestMeta); string(out) != in {
			t.Errorf("输入 %q 被改动", in)
		}
	}
}
