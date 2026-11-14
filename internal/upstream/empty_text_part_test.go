// empty_text_part_test.go 空 text 段补位：上游对每个 text 段做长度校验，
// `text` 为空串 / null / 缺失时整条消息判 11151 "a message has empty content"。
//
// 与 repairEmptyContent 的分工：
//   - repairEmptyContent：整条 content 为空（"" / null / 缺键）的 assistant/system；
//   - 本层：parts 数组里**某个** text 段为空，以及「字符串信封被上游嗅探成 parts」
//     这条隐蔽形态（工具结果里常见 `[{"type":"text","text":""}]` 形状的抓取数据）。
//
// 钉住的纪律：**只补值、不删段**（删段会改段落数量与下标），且只改真正为空的段
// ——"   " 实测放行，把它当空来补等于改动了上游本来接受的报文。
package upstream

import (
	"encoding/json"
	"testing"
)

// contentOf 跑一遍补位并取回某条消息的 content。
func contentOf(t *testing.T, msg map[string]any) any {
	t.Helper()
	guardEmptyTextParts([]any{msg})
	return msg["content"]
}

func TestGuardEmptyTextPartsArray(t *testing.T) {
	cases := map[string]map[string]any{
		"空串":  {"type": "text", "text": ""},
		"null": {"type": "text", "text": nil},
		"缺键":  {"type": "text"},
	}
	for name, part := range cases {
		t.Run(name, func(t *testing.T) {
			msg := map[string]any{"role": "user", "content": []any{part}}
			parts, ok := contentOf(t, msg).([]any)
			if !ok || len(parts) != 1 {
				t.Fatalf("content 应保持数组且段数不变：%#v", msg["content"])
			}
			if got := parts[0].(map[string]any)["text"]; got != " " {
				t.Fatalf("空 text 段应补单空格，得到 %#v", got)
			}
		})
	}
}

// 空白串、非 text 段、正常文本一律不动（上游实测放行）。
func TestGuardEmptyTextPartsLeavesAlone(t *testing.T) {
	msg := map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "   "},
		map[string]any{"type": "textarea", "text": ""},
		map[string]any{"type": "text", "text": "hi"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "u"}},
	}}
	before, _ := json.Marshal(msg["content"])
	guardEmptyTextParts([]any{msg})
	after, _ := json.Marshal(msg["content"])
	if string(before) != string(after) {
		t.Fatalf("不该改动的段被改了：\nbefore=%s\nafter=%s", before, after)
	}
}

// 字符串信封：以 [ 开头且能解析成对象数组时，上游会当 parts 嗅探 → 必须补位。
func TestGuardEmptyTextPartsSniffedString(t *testing.T) {
	raw := `[{"type":"text","text":""},{"type":"text","text":"ok"}]`
	msg := map[string]any{"role": "tool", "content": raw}
	got, ok := contentOf(t, msg).(string)
	if !ok {
		t.Fatalf("content 应保持字符串形态：%#v", msg["content"])
	}
	var parts []any
	if err := json.Unmarshal([]byte(got), &parts); err != nil {
		t.Fatalf("补位后应仍是合法 JSON：%v (%s)", err, got)
	}
	if len(parts) != 2 {
		t.Fatalf("段数不应变化：%d", len(parts))
	}
	if parts[0].(map[string]any)["text"] != " " {
		t.Fatalf("嗅探出的空 text 段应补位：%s", got)
	}
	if parts[1].(map[string]any)["text"] != "ok" {
		t.Fatalf("非空段不应被改：%s", got)
	}
}

// 以 [ 开头但解析失败 / 解析成非对象数组 / 空数组 → 上游不会当 parts，原样保留。
func TestGuardEmptyTextPartsStringNotSniffed(t *testing.T) {
	for _, raw := range []string{
		`[not json`,
		`[1,2,3]`,
		`[]`,
		`普通文本，不以方括号开头`,
		`  普通文本`,
	} {
		msg := map[string]any{"role": "user", "content": raw}
		if got := contentOf(t, msg).(string); got != raw {
			t.Errorf("不该被改写的字符串被改了：%q -> %q", raw, got)
		}
	}
}

// 端到端：PrepareBody 管线上必须真的补位（普通请求零影响另有用例覆盖）。
func TestPrepareBodyAppliesEmptyTextPartGuard(t *testing.T) {
	body := []byte(`{"model":"cn:kimi-k2.7","messages":[` +
		`{"role":"user","content":[{"type":"text","text":""}]}]}`)
	out := PrepareBodyOpt(body, false)
	var obj struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("出站 body 解析失败：%v", err)
	}
	parts, _ := obj.Messages[0]["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("段数不应变化：%d", len(parts))
	}
	if got := parts[0].(map[string]any)["text"]; got != " " {
		t.Fatalf("出站空 text 段未补位：%#v", got)
	}
}
