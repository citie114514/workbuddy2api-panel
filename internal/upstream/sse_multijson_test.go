package upstream

import (
	"fmt"
	"strings"
	"testing"
)

// 上游偶尔把多个事件塞进同一个 data 行：重复的 "data: " 前缀，或两个 JSON 直接相邻。
// 此前整行 json.Unmarshal 失败即静默丢弃，客户端会拿到缺段的回答，日志里只看到空流。
// 这里锁住「一行多事件也要逐条取全」。
func TestAggregateSplitsMultipleJSONPerLine(t *testing.T) {
	chunk := func(content string) string {
		return fmt.Sprintf(
			`{"id":"c1","created":1,"model":"m","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`,
			content)
	}
	const want = "第一段第二段"

	cases := []struct {
		name string
		body string
	}{
		{
			name: "一行一个 data（基线不变）",
			body: "data: " + chunk("第一段") + "\n\ndata: " + chunk("第二段") + "\n\ndata: [DONE]\n\n",
		},
		{
			name: "一行两个 data 前缀",
			body: "data: " + chunk("第一段") + "data: " + chunk("第二段") + "\n\ndata: [DONE]\n\n",
		},
		{
			name: "一行两个相邻 JSON",
			body: "data: " + chunk("第一段") + chunk("第二段") + "\n\ndata: [DONE]\n\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj, err := Aggregate(strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("Aggregate 失败: %v", err)
			}
			choices, _ := obj["choices"].([]any)
			if len(choices) == 0 {
				t.Fatal("聚合结果没有 choices")
			}
			c0, _ := choices[0].(map[string]any)
			msg, _ := c0["message"].(map[string]any)
			got, _ := msg["content"].(string)
			if got != want {
				t.Fatalf("content=%q want %q（同行事件被丢弃）", got, want)
			}
		})
	}
}

// 畸形片段只丢自己，不能被切分逻辑放大成整行丢失。
func TestDecodeDataPayloadDropsOnlyBrokenSegment(t *testing.T) {
	payload := `{"id":"a"}data: {brokendata: {"id":"b"}`
	objs := decodeDataPayload(payload)
	if len(objs) != 2 {
		t.Fatalf("应解析出 2 个事件，实际 %d 个（畸形片段不该牵连相邻事件）", len(objs))
	}
	if objs[0]["id"] != "a" || objs[1]["id"] != "b" {
		t.Fatalf("事件内容不符: %v", objs)
	}
}
