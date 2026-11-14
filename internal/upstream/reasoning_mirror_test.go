// reasoning_mirror_test.go 非 deepseek 家族的 reasoning 镜像：kimi/glm/minimax/
// mimo/step/space-bunny 的上游按 `reasoning` 读历史思考段，只发 reasoning_content
// 的客户端在它们那里等于「assistant 没有思考」。
//
// 钉住的关键是纪律：**只镜像已有的非空值，绝不新建字段、绝不填占位**（与 deepseek
// 那份 backfillReasoningContent 的「补齐」相反）。
package upstream

import (
	"encoding/json"
	"testing"
)

// 家族前缀命中：cn/global 前缀必须被剥掉后再匹配，否则 global 域全部漏判。
func TestReadsUpstreamReasoning(t *testing.T) {
	yes := []string{
		"kimi-k2.7", "cn:kimi-k2.7", "global:kimi-k3.1-pro",
		"glm-5.3", "cn:glm-5.3", "GLM-5.2",
		"minimax-m2", "mimo-7b", "step-2", "space-bunny-1",
		" global:mimo-7b ",
	}
	for _, m := range yes {
		if !readsUpstreamReasoning(m) {
			t.Errorf("%q 应命中 reasoning 镜像家族", m)
		}
	}
	no := []string{
		"deepseek-v4.1-flash", "cn:deepseek-v4-pro", "qwen-3", "gpt-5", "", "  ",
	}
	for _, m := range no {
		if readsUpstreamReasoning(m) {
			t.Errorf("%q 不应命中 reasoning 镜像家族", m)
		}
	}
}

// mirrorBody 跑一遍镜像并取回 messages，便于逐条断言。
func mirrorBody(t *testing.T, model string, msgs []any) []any {
	t.Helper()
	obj := map[string]any{"model": model, "messages": msgs}
	mirrorUpstreamReasoning(obj)
	out, ok := obj["messages"].([]any)
	if !ok {
		t.Fatalf("messages 类型被破坏：%T", obj["messages"])
	}
	return out
}

func TestMirrorUpstreamReasoningCopies(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "assistant", "content": "a", "reasoning_content": "thinking"},
	}
	got := mirrorBody(t, "cn:glm-5.3", msgs)[0].(map[string]any)
	if got["reasoning"] != "thinking" {
		t.Fatalf("reasoning = %v，期望镜像为 thinking", got["reasoning"])
	}
}

// 已有非空 reasoning 时不覆盖：上游自己写的更权威。
func TestMirrorUpstreamReasoningKeepsExisting(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "assistant", "content": "a", "reasoning_content": "client", "reasoning": "upstream"},
	}
	got := mirrorBody(t, "kimi-k2.7", msgs)[0].(map[string]any)
	if got["reasoning"] != "upstream" {
		t.Fatalf("reasoning = %v，已有非空值不应被覆盖", got["reasoning"])
	}
}

// 绝不新建字段：没有 reasoning_content（或只有空白）时不能凭空补出 reasoning。
func TestMirrorUpstreamReasoningNeverCreates(t *testing.T) {
	cases := map[string]map[string]any{
		"缺 reasoning_content": {"role": "assistant", "content": "a"},
		"reasoning_content 空串": {"role": "assistant", "content": "a", "reasoning_content": ""},
		"reasoning_content 空白": {"role": "assistant", "content": "a", "reasoning_content": "   "},
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			got := mirrorBody(t, "glm-5.3", []any{msg})[0].(map[string]any)
			if _, has := got["reasoning"]; has {
				t.Fatalf("不得凭空新建 reasoning 字段：%v", got)
			}
		})
	}
}

// 只处理 assistant：user/tool 没有思考段。
func TestMirrorUpstreamReasoningAssistantOnly(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "q", "reasoning_content": "x"},
		map[string]any{"role": "tool", "content": "r", "reasoning_content": "y"},
	}
	for i, raw := range mirrorBody(t, "glm-5.3", msgs) {
		if _, has := raw.(map[string]any)["reasoning"]; has {
			t.Fatalf("第 %d 条非 assistant 消息不应被镜像", i)
		}
	}
}

// 非目标家族（deepseek 归 backfillReasoningContent 管）零改动。
func TestMirrorUpstreamReasoningIgnoresOtherFamilies(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "assistant", "content": "a", "reasoning_content": "thinking"},
	}
	got := mirrorBody(t, "cn:deepseek-v4.1-flash", msgs)[0].(map[string]any)
	if _, has := got["reasoning"]; has {
		t.Fatalf("deepseek 系不应由本层写入 reasoning：%v", got)
	}
}

// 端到端：PrepareBody 管线上镜像必须真的生效（模型经 JSON 往返后仍是字符串）。
func TestPrepareBodyAppliesReasoningMirror(t *testing.T) {
	body := []byte(`{"model":"cn:glm-5.3","messages":[` +
		`{"role":"user","content":"q"},` +
		`{"role":"assistant","content":"a","reasoning_content":"thought"}]}`)
	out := PrepareBodyOpt(body, false)
	var obj struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("出站 body 解析失败：%v", err)
	}
	if len(obj.Messages) != 2 {
		t.Fatalf("messages 条数变了：%d", len(obj.Messages))
	}
	if got := obj.Messages[1]["reasoning"]; got != "thought" {
		t.Fatalf("出站 assistant.reasoning = %v，期望 thought（镜像未生效）", got)
	}
}
