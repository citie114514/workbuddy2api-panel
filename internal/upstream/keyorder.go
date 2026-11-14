// keyorder.go 让出站 chat 请求体的**顶层键序**与官方桌面客户端一致。
//
// 官方客户端是 Node，JSON.stringify 按对象**插入序**输出；抓包实测（2026-11-03）
// 顶层顺序固定为：
//
//	model, messages, tools, temperature, max_tokens, stream, stream_options,
//	reasoning_effort, verbosity, extra_vars, reasoning_summary
//
// 我们走 json.Unmarshal → map[string]any → Marshal，Go 对 map 一律**按字母序**
// 输出（model 落到 max_tokens 之后、messages 被提到 model 前面）。键序不改变长度、
// 不改变语义（服务端反序列化到 struct 后不存在成员顺序），但会让 78KB 报文与官方
// 逐字节不同；保持同形属于「不留下可被统计的形态差异」这一类低成本动作。
//
// 实现刻意只切**一层**：字符串字面量里的 {}[]、以及嵌套对象/数组整体作为不透明
// 片段搬运，因此不会触碰转义、数字格式或嵌套结构，也不会改变任何值的字节。
// 任何解析异常都原样返回入参（宁可不排，不许破坏 body）。
package upstream

import (
	"bytes"
	"encoding/json"
)

// officialTopLevelOrder 官方桌面客户端 chat 请求体的顶层插入序（抓包实测）。
// 只影响「存在且被列出」的键；未列出的键（我们的 prompt_cache_key、thinking，
// 或下游额外传的 top_p/response_format 等）按输入相对序跟在后面。
var officialTopLevelOrder = []string{
	"model",
	"messages",
	"tools",
	"temperature",
	"max_tokens",
	"stream",
	"stream_options",
	"reasoning_effort",
	"verbosity",
	"extra_vars",
	"reasoning_summary",
}

// keyField 顶层一个成员：原始键字节（含引号，保留其转义形态）与原始值字节。
type keyField struct {
	key string // 键文本（已解引号），仅用于匹配 order
	raw string // body 里的原样片段：`"key":value`
}

// reorderTopLevelKeys 按 order 重排 JSON 对象顶层成员，其余成员按原相对序追加。
// 非对象、截断、语法异常一律返回入参本身。
func reorderTopLevelKeys(body []byte, order []string) []byte {
	fields, ok := splitTopLevel(body)
	if !ok || len(fields) < 2 {
		// 0/1 个成员没有可排的，直接返回（省一次重建，也避免无意义改动）。
		return body
	}

	used := make([]bool, len(fields))
	firstOf := make(map[string]int, len(fields))
	for i, f := range fields {
		if _, dup := firstOf[f.key]; !dup {
			firstOf[f.key] = i
		}
	}

	var out bytes.Buffer
	out.Grow(len(body))
	out.WriteByte('{')
	first := true
	write := func(f keyField) {
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.WriteString(f.raw)
	}
	for _, k := range order {
		if i, hit := firstOf[k]; hit && !used[i] {
			used[i] = true
			write(fields[i])
		}
	}
	for i, f := range fields {
		if !used[i] {
			write(f)
		}
	}
	out.WriteByte('}')
	return out.Bytes()
}

// splitTopLevel 把 JSON 对象的顶层成员切成 key/value 原始片段。
// 返回的 raw 已是 `"key":value` 的紧凑形态（成员间原空格被丢弃，与
// JSON.stringify / Go 编码器的输出形态一致）。
func splitTopLevel(body []byte) ([]keyField, bool) {
	i := skipJSONSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return nil, false
	}
	i++

	var fields []keyField
	for {
		i = skipJSONSpace(body, i)
		if i >= len(body) {
			return nil, false
		}
		if body[i] == '}' {
			i++
			break
		}
		if body[i] == ',' {
			// 尾逗号（`,` 后直接闭合）不是合法 JSON：不重排，原样返回。
			if next := skipJSONSpace(body, i+1); next < len(body) && body[next] == '}' {
				return nil, false
			}
			i++
			continue
		}
		if body[i] != '"' {
			return nil, false
		}

		keyStart := i
		keyEnd := skipJSONString(body, keyStart)
		if keyEnd < 0 {
			return nil, false
		}
		keyText, ok := unquoteJSONKey(body[keyStart:keyEnd])
		if !ok {
			return nil, false
		}

		i = skipJSONSpace(body, keyEnd)
		if i >= len(body) || body[i] != ':' {
			return nil, false
		}
		valStart := skipJSONSpace(body, i+1)

		valEnd := skipJSONValue(body, valStart)
		if valEnd < 0 {
			return nil, false
		}
		// raw 固定为紧凑形态 `"key":value`：键与值各自保留原始字节（含键的转义形态），
		// 只丢弃 `:` 两侧的空白，与 JSON.stringify / Go 编码器的输出形态一致。
		fields = append(fields, keyField{
			key: keyText,
			raw: string(body[keyStart:keyEnd]) + ":" + string(body[valStart:valEnd]),
		})
		i = valEnd
	}
	if i != len(body) && skipJSONSpace(body, i) != len(body) {
		return nil, false
	}
	return fields, true
}

// skipJSONSpace 跳过 JSON 允许的空白字符。
func skipJSONSpace(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// skipJSONString 从 b[i]=='"' 开始，返回闭合引号之后的下标；未闭合返回 -1。
// 正确处理 `\"` 等反斜杠转义（转义成对跳过，`\\` 不会误吞闭合引号）。
func skipJSONString(b []byte, i int) int {
	if i >= len(b) || b[i] != '"' {
		return -1
	}
	i++
	for i < len(b) {
		switch b[i] {
		case '\\':
			i += 2
		case '"':
			return i + 1
		default:
			i++
		}
	}
	return -1
}

// skipJSONValue 返回一个完整 JSON 值之后的下标；结构异常返回 -1。
// 标量按「遇到 , 或 } 或空白即止」扫描，对象/数组按深度扫描并跳过其中的字符串。
func skipJSONValue(b []byte, i int) int {
	if i >= len(b) {
		return -1
	}
	if b[i] == '"' {
		return skipJSONString(b, i)
	}
	if b[i] != '{' && b[i] != '[' {
		start := i
		for i < len(b) {
			switch b[i] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				if i == start {
					return -1
				}
				return i
			}
			i++
		}
		if i == start {
			return -1
		}
		return i
	}

	depth := 0
	for i < len(b) {
		switch b[i] {
		case '"':
			end := skipJSONString(b, i)
			if end < 0 {
				return -1
			}
			i = end
			continue
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
		i++
	}
	return -1
}

// unquoteJSONKey 解出一个 JSON 键文本；含非法转义返回 false。
func unquoteJSONKey(raw []byte) (string, bool) {
	// 绝大多数键不含转义，先走快路径。
	if !bytes.ContainsRune(raw, '\\') {
		if len(raw) < 2 {
			return "", false
		}
		return string(raw[1 : len(raw)-1]), true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}
