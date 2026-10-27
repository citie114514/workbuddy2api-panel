package server

// proto_toolimage_test.go v1.16.1：工具结果里的图片透传。
//
// 此前 Responses 的 function_call_output 数组被整体 json.Marshal 成文本（图片
// data URL 变成百万 token 级文本垃圾、模型看不到图）；Anthropic 的 tool_result
// 图片块被静默丢弃。v1.16.1 两条路径都按图片区块透传。

import (
	"encoding/json"
	"testing"
)

const testPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// TestResponsesToolOutputImageArray function_call_output 数组：文本转 text、
// input_image 保留为 image_url 图片区块，顺序保持。
func TestResponsesToolOutputImageArray(t *testing.T) {
	raw := `{"model":"m","max_tokens":9,"input":[
		{"type":"function_call","call_id":"call_1","name":"screenshot","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_1","output":[
			{"type":"input_text","text":"captured:"},
			{"type":"input_image","image_url":"data:image/png;base64,` + testPNG + `"}
		]}]}`
	body, perr := (responsesAdapter{}).ConvertRequest([]byte(raw))
	if perr != nil {
		t.Fatalf("ConvertRequest: %+v", perr)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v", msgs)
	}
	tool, _ := msgs[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Fatalf("tool message: %v", msgs[1])
	}
	content, ok := tool["content"].([]any)
	if !ok {
		t.Fatalf("tool content must stay an array, got %T: %v", tool["content"], tool["content"])
	}
	if len(content) != 2 {
		t.Fatalf("content blocks=%v", content)
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "captured:" {
		t.Errorf("text part: %v", content[0])
	}
	second, _ := content[1].(map[string]any)
	if second["type"] != "image_url" {
		t.Fatalf("image part: %v", content[1])
	}
	iu, _ := second["image_url"].(map[string]any)
	if iu["url"] != "data:image/png;base64,"+testPNG {
		t.Errorf("image url lost: %v", iu)
	}
}

// TestResponsesToolOutputStringUnchanged 字符串 output 形态零回归。
func TestResponsesToolOutputStringUnchanged(t *testing.T) {
	raw := `{"model":"m","max_tokens":9,"input":[
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"plain text result"}]}`
	body, perr := (responsesAdapter{}).ConvertRequest([]byte(raw))
	if perr != nil {
		t.Fatalf("ConvertRequest: %+v", perr)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	msgs, _ := got["messages"].([]any)
	tool, _ := msgs[1].(map[string]any)
	if tool["content"] != "plain text result" {
		t.Errorf("string output must pass through verbatim: %v", tool["content"])
	}
}

// TestResponsesToolOutputInputImageURLObject image_url 为 {url} 对象形态 + 顶层 url 键兜底。
func TestResponsesToolOutputInputImageURLObject(t *testing.T) {
	raw := `{"model":"m","max_tokens":9,"input":[
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[
			{"type":"input_image_url","image_url":{"url":"data:image/png;base64,` + testPNG + `"}}
		]}]}`
	body, perr := (responsesAdapter{}).ConvertRequest([]byte(raw))
	if perr != nil {
		t.Fatalf("ConvertRequest: %+v", perr)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	msgs, _ := got["messages"].([]any)
	tool, _ := msgs[1].(map[string]any)
	content, _ := tool["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content=%v", content)
	}
	part, _ := content[0].(map[string]any)
	iu, _ := part["image_url"].(map[string]any)
	if iu["url"] != "data:image/png;base64,"+testPNG {
		t.Errorf("object-form image_url lost: %v", part)
	}
}

// TestAnthropicToolResultImage tool_result 图片块转 image_url data URL 透传
//（此前被静默丢弃，Claude Code 截图工具结果模型看不到）。
func TestAnthropicToolResultImage(t *testing.T) {
	raw := `{"model":"m","max_tokens":9,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"shot","input":{}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_1","content":[
				{"type":"text","text":"shot:"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + testPNG + `"}}
			]}
		]}]}`
	body, perr := (anthropicAdapter{}).ConvertRequest([]byte(raw))
	if perr != nil {
		t.Fatalf("ConvertRequest: %+v", perr)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v", msgs)
	}
	tool, _ := msgs[1].(map[string]any)
	content, ok := tool["content"].([]any)
	if !ok {
		t.Fatalf("tool content must be an array, got %T: %v", tool["content"], tool["content"])
	}
	if len(content) != 2 {
		t.Fatalf("content blocks=%v", content)
	}
	first, _ := content[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "shot:" {
		t.Errorf("text part: %v", content[0])
	}
	second, _ := content[1].(map[string]any)
	iu, _ := second["image_url"].(map[string]any)
	if iu["url"] != "data:image/png;base64,"+testPNG {
		t.Errorf("anthropic tool_result image lost: %v", content[1])
	}
}
