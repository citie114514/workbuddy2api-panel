package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// toolNames 跑一遍 PrepareBodyOpt 后取出 tools 里的 function.name（裸形态取 name）。
// 返回 nil 表示 tools 键已不存在。
func toolNames(t *testing.T, body []byte) []string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("out body is not json: %v\n%s", err, body)
	}
	raw, ok := obj["tools"].([]any)
	if !ok {
		if _, exists := obj["tools"]; exists {
			t.Fatalf("tools 存在但不是数组: %T", obj["tools"])
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		n, _ := toolName(m)
		out = append(out, n)
	}
	return out
}

func bodyWithTools(t *testing.T, tools ...string) []byte {
	t.Helper()
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"tools":[` +
		strings.Join(tools, ",") + `]}`
	if !json.Valid([]byte(body)) {
		t.Fatalf("bad fixture: %s", body)
	}
	return []byte(body)
}

// TestToolNameValidationDropsUnusable 上游按 tools[N].function.name 校验，不合格会
// 整条请求 400（code 11133，extError.param 直指该路径）。这里逐类钉住「丢弃」，
// 并确保合法条目一条不少。
func TestToolNameValidationDropsUnusable(t *testing.T) {
	long65 := strings.Repeat("a", 65)
	long64 := strings.Repeat("b", 64)
	cases := []struct {
		name string
		tool string
		want bool // true = 保留
		why  string
	}{
		{"plain", `{"type":"function","function":{"name":"web_search"}}`, true, "常规名"},
		{"alnum-dash-underscore", `{"type":"function","function":{"name":"a-b_C9"}}`, true, "允许字符集边界"},
		{"len64", `{"type":"function","function":{"name":"` + long64 + `"}}`, true, "恰好 64 字节"},
		{"len65", `{"type":"function","function":{"name":"` + long65 + `"}}`, false, "超 64 字节"},
		{"empty", `{"type":"function","function":{"name":""}}`, false, "空名"},
		{"no-function", `{"type":"web_search"}`, false, "服务端工具声明（无 function 层、无 name）"},
		{"dot", `{"type":"function","function":{"name":"mcp.server.run"}}`, false, "含点号"},
		{"slash", `{"type":"function","function":{"name":"fs/read"}}`, false, "含斜杠"},
		{"cjk", `{"type":"function","function":{"name":"搜索"}}`, false, "含 CJK"},
		{"space", `{"type":"function","function":{"name":"web search"}}`, false, "含空格"},
		{"bare-name", `{"name":"bare_tool","description":"x"}`, true, "裸形态合法名"},
		{"bare-bad", `{"name":"bare.tool"}`, false, "裸形态非法名"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := PrepareBodyOpt(bodyWithTools(t, tc.tool), false)
			got := toolNames(t, out)
			if tc.want {
				if len(got) != 1 {
					t.Fatalf("%s（%s）应保留，实际 tools=%v\n%s", tc.name, tc.why, got, out)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("%s（%s）应被丢弃，实际 tools=%v\n%s", tc.name, tc.why, got, out)
			}
		})
	}
}

// TestToolNameAllDroppedRemovesKey 全部条目被丢时删掉 tools 键：留一个空数组
// 与「没声明工具」在部分上游实现里语义不同（空数组仍会走工具校验分支）。
func TestToolNameAllDroppedRemovesKey(t *testing.T) {
	out := PrepareBodyOpt(bodyWithTools(t,
		`{"type":"web_search"}`,
		`{"type":"function","function":{"name":"bad.name"}}`,
	), false)
	if got := toolNames(t, out); got != nil {
		t.Fatalf("全丢后应删除 tools 键，实际 %v\n%s", got, out)
	}
	if strings.Contains(string(out), `"tools":[]`) {
		t.Fatalf("不得留下空数组: %s", out)
	}
}

// TestToolNameMixedKeepsValidAndDedups 混合清单：合法条目原样保留（顺序不变）、
// 同名的只留第一条、非法的一条都不留。
func TestToolNameMixedKeepsValidAndDedups(t *testing.T) {
	out := PrepareBodyOpt(bodyWithTools(t,
		`{"type":"function","function":{"name":"alpha"}}`,
		`{"type":"web_search"}`,
		`{"type":"function","function":{"name":"beta"}}`,
		`{"type":"function","function":{"name":"alpha"}}`, // 重名
		`{"type":"function","function":{"name":"gam.ma"}}`,
		`{"type":"function","function":{"name":"gamma"}}`,
	), false)
	got := toolNames(t, out)
	want := []string{"alpha", "beta", "gamma"}
	if len(got) != len(want) {
		t.Fatalf("tools=%v want %v\n%s", got, want, out)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tools=%v want %v（顺序也必须保留）", got, want)
		}
	}
}

// TestToolPatternUnescapeStillWorks name 校验不能把原有的 pattern 转义修复挤掉。
func TestToolPatternUnescapeStillWorks(t *testing.T) {
	tool := `{"type":"function","function":{"name":"ok_tool","parameters":{"type":"object","properties":{"p":{"type":"string","pattern":"^a\\_b$"}}}}}`
	out := PrepareBodyOpt(bodyWithTools(t, tool), false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if strings.Contains(string(out), `a\_b`) {
		t.Fatalf("pattern 里的 \\_ 应被转成 _: %s", out)
	}
	if !strings.Contains(string(out), `^a_b$`) {
		t.Fatalf("pattern 未按预期改写: %s", out)
	}
	if got := toolNames(t, out); len(got) != 1 || got[0] != "ok_tool" {
		t.Fatalf("合法工具不应被丢: %v", got)
	}
}

// TestToolNameValidationIgnoresDocsAndOtherFields 只动 tools，不碰其它字段；
// 合法工具的 description / parameters 逐字保留（丢弃只针对不合格条目）。
func TestToolNameValidationIgnoresDocsAndOtherFields(t *testing.T) {
	tool := `{"type":"function","function":{"name":"keep_me","description":"说明: 不要改我","parameters":{"type":"object","properties":{}}}}`
	out := PrepareBodyOpt(bodyWithTools(t, tool), false)
	if !strings.Contains(string(out), "不要改我") {
		t.Fatalf("description 被动了: %s", out)
	}
	if !strings.Contains(string(out), `"model":"glm-5.2"`) {
		t.Fatalf("非 tools 字段被动了: %s", out)
	}
}
