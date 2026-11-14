package upstream

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// topLevelKeys 按**字节顺序**读出 JSON 对象顶层键（用于断言键序，
// 不能用 map 比较——map 天然丢序）。
func topLevelKeys(t *testing.T, body []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("读首个 token 失败: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		t.Fatalf("不是 JSON 对象: %s", body)
	}
	var keys []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			t.Fatalf("读键失败: %v", err)
		}
		k, ok := kt.(string)
		if !ok {
			t.Fatalf("键不是字符串: %v", kt)
		}
		keys = append(keys, k)
		// 跳过值（Skip 不解析内容，纯跳过）
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("跳过 %q 的值失败: %v", k, err)
		}
	}
	return keys
}

func mustReorder(t *testing.T, in string) string {
	t.Helper()
	return string(reorderTopLevelKeys([]byte(in), officialTopLevelOrder))
}

// TestReorderTopLevelKeys_OfficialOrder 核心用例：字母序输入 → 官方插入序。
func TestReorderTopLevelKeys_OfficialOrder(t *testing.T) {
	in := `{"stream":true,"stream_options":{"include_usage":true},"model":"deepseek-v4.1-flash","messages":[],"max_tokens":100,"temperature":1,"tools":[]}`
	got := mustReorder(t, in)

	want := []string{"model", "messages", "tools", "temperature", "max_tokens", "stream", "stream_options"}
	if keys := topLevelKeys(t, []byte(got)); !equalStrings(keys, want) {
		t.Fatalf("顶层键序不对\n got=%v\nwant=%v", keys, want)
	}
	// 首键必须是 model（官方首位，也是本次改动的直接目标）
	if !strings.HasPrefix(got, `{"model":"deepseek-v4.1-flash"`) {
		t.Fatalf("首个成员不是 model: %.80s", got)
	}
}

// TestReorderTopLevelKeys_UnknownKeysKeepRelativeOrder 未列入官方序的键（我们的
// prompt_cache_key / thinking，或下游额外字段）按输入相对序追加在尾部，不丢不重。
func TestReorderTopLevelKeys_UnknownKeysKeepRelativeOrder(t *testing.T) {
	in := `{"zzz":1,"prompt_cache_key":"abc","model":"m","thinking":{"type":"enabled"},"aaa":2}`
	got := mustReorder(t, in)
	want := []string{"model", "zzz", "prompt_cache_key", "thinking", "aaa"}
	if keys := topLevelKeys(t, []byte(got)); !equalStrings(keys, want) {
		t.Fatalf("未列出键的相对序被破坏\n got=%v\nwant=%v", keys, want)
	}
}

// TestReorderTopLevelKeys_NestedOpaque 嵌套对象/数组必须原样搬运（内部键序不动）。
func TestReorderTopLevelKeys_NestedOpaque(t *testing.T) {
	in := `{"z":{"y":1,"x":[{"b":1,"a":2}]},"model":"m"}`
	got := mustReorder(t, in)
	want := `{"model":"m","z":{"y":1,"x":[{"b":1,"a":2}]}}`
	if got != want {
		t.Fatalf("嵌套内容被改动\n got=%s\nwant=%s", got, want)
	}
}

// TestReorderTopLevelKeys_StringTricks 值里的 {}[] , : 与转义引号不得影响切分。
func TestReorderTopLevelKeys_StringTricks(t *testing.T) {
	in := `{"content":"}{\"a\":1,[]","model":"m"}`
	got := mustReorder(t, in)
	want := `{"model":"m","content":"}{\"a\":1,[]"}`
	if got != want {
		t.Fatalf("字符串内特殊字符导致切分错误\n got=%s\nwant=%s", got, want)
	}
	// 语义必须完全等价
	assertJSONEqual(t, got, in)
}

// TestReorderTopLevelKeys_LengthAndSemanticsUnchanged 重排不改长度、不改语义
// （长度不变是「不改变出站流量」的前提）。
func TestReorderTopLevelKeys_LengthAndSemanticsUnchanged(t *testing.T) {
	in := `{"stream":true,"model":"m","messages":[{"content":"hi","role":"user"}],"max_tokens":9}`
	got := mustReorder(t, in)
	if len(got) != len(in) {
		t.Fatalf("长度变了: %d → %d", len(in), len(got))
	}
	assertJSONEqual(t, got, in)
}

// TestReorderTopLevelKeys_Idempotent 已按官方序排好的 body 再排一次必须逐字节不变。
func TestReorderTopLevelKeys_Idempotent(t *testing.T) {
	once := mustReorder(t, `{"stream":true,"model":"m","messages":[]}`)
	twice := string(reorderTopLevelKeys([]byte(once), officialTopLevelOrder))
	if once != twice {
		t.Fatalf("非幂等\n once=%s\ntwice=%s", once, twice)
	}
}

// TestReorderTopLevelKeys_DuplicateKeysKept JSON 允许重复键，重排必须一个不少。
func TestReorderTopLevelKeys_DuplicateKeysKept(t *testing.T) {
	in := `{"model":"a","x":1,"model":"b"}`
	got := mustReorder(t, in)
	if strings.Count(got, `"model"`) != 2 {
		t.Fatalf("重复键被吞: %s", got)
	}
}

// TestReorderTopLevelKeys_BadInputUnchanged 非法/非对象输入一律原样返回。
func TestReorderTopLevelKeys_BadInputUnchanged(t *testing.T) {
	for _, in := range []string{
		``,
		`{}`,
		`{"model":"m"}`,
		`[1,2]`,
		`{"model":`,
		`{"model":"m",}`,
		`not json`,
	} {
		if got := mustReorder(t, in); got != in {
			t.Fatalf("非法输入被改动: in=%q got=%q", in, got)
		}
	}
}

// TestPrepareBody_TopLevelStartsWithModel 端到端：真实管线（含 stream 注入、
// stream_options 补全、tool_choice 归一化）之后，顶层仍按官方序、model 在首位。
func TestPrepareBody_TopLevelStartsWithModel(t *testing.T) {
	in := []byte(`{"stream":false,"max_tokens":100,"messages":[{"role":"user","content":"hi"}],"model":"deepseek-v4.1-flash","temperature":1}`)
	out := PrepareBodyOpt(in, false)

	// 管线会为 deepseek 系模型补 reasoning_effort / thinking（既有行为），
	// thinking 不在官方序里 → 落尾部；其余按官方序。
	want := []string{"model", "messages", "temperature", "max_tokens", "stream", "stream_options", "reasoning_effort", "thinking"}
	if keys := topLevelKeys(t, out); !equalStrings(keys, want) {
		t.Fatalf("PrepareBodyOpt 顶层键序不对\n got=%v\nwant=%v\nbody=%s", keys, want, out)
	}
	// stream 必须被改成 true（既有行为不能被键序重排影响）
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if obj["stream"] != true {
		t.Fatalf("stream 未强制 true: %v", obj["stream"])
	}
}

// TestEnsureConsoleSystem_TopLevelOrder global 兜底注入走的第二个序列化入口，
// 同样必须按官方序输出（否则只有 global 账号形态不一致）。
func TestEnsureConsoleSystem_TopLevelOrder(t *testing.T) {
	in := []byte(`{"stream":true,"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	out := ensureConsoleSystem(in)
	keys := topLevelKeys(t, out)
	if len(keys) == 0 || keys[0] != "model" {
		t.Fatalf("ensureConsoleSystem 输出首键不是 model: %v (%s)", keys, out)
	}
	if !strings.Contains(string(out), `"role":"system"`) {
		t.Fatalf("兜底 system 未注入: %s", out)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func assertJSONEqual(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got 不是合法 JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want 不是合法 JSON: %v (%s)", err, want)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if !bytes.Equal(gb, wb) {
		t.Fatalf("语义不等价\n got=%s\nwant=%s", gb, wb)
	}
}
