// promptlimit_test.go 11115 可修复重发的判定与请求体改写。
//
// 关键回归：不带 max_tokens 的 OpenAI 兼容客户端（昔涟走 ai-sdk）会被上游的
// 默认输出预留顶成 400，而带 max_tokens 的客户端（pi / Claude Code / dsh）不会。
package upstream

import (
	"encoding/json"
	"testing"
)

// TestParsePromptTooLong 覆盖上游实测报文形态（含 JSON 转义后的 >）。
func TestParsePromptTooLong(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		used  int64
		limit int64
		ok    bool
	}{
		{
			name:  "实测 global 报文（原始 JSON body）",
			body:  `{"code":11115,"msg":"prompt is too long: 1252040 tokens > 1048576 maximum","requestId":"abc"}`,
			used:  1252040,
			limit: 1048576,
			ok:    true,
		},
		{
			name:  "\\u003e 转义形态",
			body:  `{"code":11115,"msg":"prompt is too long: 868040 tokens > 1048576 maximum"}`,
			used:  868040,
			limit: 1048576,
			ok:    true,
		},
		{
			name:  "只有 msg 文本（已解码）",
			body:  "prompt is too long: 1064040 tokens > 1048576 maximum",
			used:  1064040,
			limit: 1048576,
			ok:    true,
		},
		{
			name: "大写与多余空白",
			body: "PROMPT IS TOO LONG:  700000  TOKENS  >  1000000  MAXIMUM",
			used: 700000, limit: 1000000, ok: true,
		},
		{
			name: "别的 11115 文案（没有数值）不猜",
			body: `{"code":11115,"msg":"prompt is too long"}`,
			ok:   false,
		},
		{
			name: "空 body",
			body: "",
			ok:   false,
		},
		{
			name: "无关错误",
			body: `{"code":11102,"msg":"no such model"}`,
			ok:   false,
		},
	}
	for _, c := range cases {
		used, limit, ok := ParsePromptTooLong(c.body)
		if ok != c.ok {
			t.Fatalf("%s: ok=%v want %v", c.name, ok, c.ok)
		}
		if !ok {
			continue
		}
		if used != c.used || limit != c.limit {
			t.Fatalf("%s: used/limit=%d/%d want %d/%d", c.name, used, limit, c.used, c.limit)
		}
	}
}

// TestMaxTokensInBody 取客户端声明的输出上限：max_tokens 优先，其次别名。
func TestMaxTokensInBody(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		want     int64
		explicit bool
	}{
		{name: "max_tokens", body: `{"max_tokens":4096}`, want: 4096, explicit: true},
		{name: "别名 max_completion_tokens", body: `{"max_completion_tokens":2048}`, want: 2048, explicit: true},
		{name: "两者都在：max_tokens 优先", body: `{"max_tokens":100,"max_completion_tokens":999}`, want: 100, explicit: true},
		{name: "缺字段", body: `{"messages":[]}`, explicit: false},
		{name: "零值不算显式", body: `{"max_tokens":0}`, explicit: false},
		{name: "负值不算显式", body: `{"max_tokens":-5}`, explicit: false},
		{name: "非数值不算显式", body: `{"max_tokens":"big"}`, explicit: false},
		{name: "坏 JSON", body: `{oops`, explicit: false},
		{name: "空 body", body: ``, explicit: false},
	}
	for _, c := range cases {
		got, explicit := MaxTokensInBody([]byte(c.body))
		if explicit != c.explicit {
			t.Fatalf("%s: explicit=%v want %v", c.name, explicit, c.explicit)
		}
		if explicit && got != c.want {
			t.Fatalf("%s: got=%d want %d", c.name, got, c.want)
		}
	}
}

// TestPlanPromptTooLongRepair 覆盖两种收敛路径与所有「救不回来」的边界。
func TestPlanPromptTooLongRepair(t *testing.T) {
	cases := []struct {
		name     string
		used     int64
		limit    int64
		sentMax  int64
		explicit bool
		want     int64
		ok       bool
	}{
		{
			// 昔涟实案：不带 max_tokens，上游按 global 默认 384000 预留记账。
			// 补一个明确的 32768 小预留，绝大多数场景这一跳就成功。
			name: "无显式 max_tokens → 补小预留",
			used: 1252040, limit: 1048576, explicit: false,
			want: promptTooLongBlindMax, ok: true,
		},
		{
			// 第二跳的现实形态：输入已接近窗口，明确的小预留仍越界，
			// used - 32768 就是真实 prompt，一次算出精确预算。
			name: "预留可信 → 一次算出精确预算",
			used: 1035000 + promptTooLongBlindMax, limit: 1048576,
			sentMax: promptTooLongBlindMax, explicit: true,
			want: 1048576 - 1035000 - promptTooLongSafety, ok: true,
		},
		{
			// 显式 max_tokens 本来就小（pi 这类客户端），used - sent 同样是真实 prompt。
			name: "小显式预留越界 → 精确收敛",
			used: 1045000 + 4000, limit: 1048576,
			sentMax: 4000, explicit: true,
			want: 1048576 - 1045000 - promptTooLongSafety, ok: true,
		},
		{
			// 给了大值：可能被上游换成默认预留，无法反推 → 先补小预留让报文变准。
			name: "大显式预留无法反推 → 补小预留",
			used: 868040 + 384000, limit: 1048576,
			sentMax: 500000, explicit: true,
			want: promptTooLongBlindMax, ok: true,
		},
		{
			// 信任档上界（32768）刚刚越界：可反推，预算按剩余窗口如实给出
			// （即使只剩一千多 token，也如实交出去，由上游决定能不能写完）。
			name: "显式值恰在信任档上界 → 仍可反推",
			used: 1046000 + promptTooLongBlindMax, limit: 1048576,
			sentMax: promptTooLongBlindMax, explicit: true,
			want: 1048576 - 1046000 - promptTooLongSafety, ok: true,
		},
		{
			// 报文说输入已经顶到窗口：收窄输出没有意义，交给客户端压缩上下文。
			name: "输入已贴顶 → 不再重发",
			used: 1048576 + promptTooLongBlindMax + 10, limit: 1048576,
			sentMax: promptTooLongBlindMax, explicit: true,
			ok: false,
		},
		{
			name: "剩余预算低于下限 → 不再重发",
			used: 1048576 - 100, limit: 1048576,
			sentMax: 200, explicit: true,
			ok: false,
		},
		{
			name: "报文数值缺失 → 不猜",
			used: 0, limit: 0, explicit: false,
			ok: false,
		},
	}
	for _, c := range cases {
		got, ok := PlanPromptTooLongRepair(c.used, c.limit, c.sentMax, c.explicit)
		if ok != c.ok {
			t.Fatalf("%s: ok=%v want %v (got budget=%d)", c.name, ok, c.ok, got)
		}
		if ok && got != c.want {
			t.Fatalf("%s: budget=%d want %d", c.name, got, c.want)
		}
	}
}

// TestPlanPromptTooLongRepairConverges 两跳必须收敛：贴着窗口的输入经两轮重发
// 一定拿到一个能过上游的预算，而不是原地打转。用贴近实案的形态（输入已到
// 窗口边缘）而不是宽松余量，否则第一跳就够用了，测不出「第二跳」这条路径。
func TestPlanPromptTooLongRepairConverges(t *testing.T) {
	const prompt = 1035000 // 真实输入（上游 used - 默认预留）
	const limit = 1048576
	body := []byte(`{"model":"global:deepseek-v4.1-flash","messages":[]}`)

	// 第一跳：报文 used = prompt + global 默认预留 384000。
	budget, ok := PlanPromptTooLongRepair(prompt+384000, limit, 0, false)
	if !ok {
		t.Fatal("第一跳必须给出重发预算")
	}
	body = WithMaxTokens(body, budget)

	// 第二跳：上游按新的显式预留记账，used = prompt + budget；仍越界 → 精确收敛。
	sent, explicit := MaxTokensInBody(body)
	if !explicit || sent != budget {
		t.Fatalf("重发体里的 max_tokens=%d explicit=%v，want %d", sent, explicit, budget)
	}
	budget2, ok := PlanPromptTooLongRepair(prompt+sent, limit, sent, explicit)
	if !ok {
		t.Fatal("第二跳必须收敛出精确预算")
	}
	if prompt+budget2 > limit {
		t.Fatalf("收敛后的 prompt+预算=%d 仍超窗口 %d", prompt+budget2, limit)
	}
	if budget2 < promptTooLongFloor {
		t.Fatalf("收敛预算 %d 低于下限", budget2)
	}
	if prompt+budget2 > limit-promptTooLongSafety+1 {
		t.Fatalf("收敛预算没有留出安全余量：%d + %d > %d", prompt, budget2, limit)
	}
}

// TestWithMaxTokens 改写输出上限：可解析的 body 用 max_tokens 覆盖并保留其余字段；
// 坏 body 原样返回（不二次错误化）。
func TestWithMaxTokens(t *testing.T) {
	in := []byte(`{"model":"m","max_completion_tokens":999,"messages":[{"role":"user","content":"hi"}]}`)
	out := WithMaxTokens(in, 12345)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("out not json: %v", err)
	}
	if v, _ := obj["max_tokens"].(float64); int64(v) != 12345 {
		t.Fatalf("max_tokens=%v want 12345", obj["max_tokens"])
	}
	if obj["model"] != "m" {
		t.Fatalf("其它字段被破坏: %v", obj)
	}
	if msgs, ok := obj["messages"].([]any); !ok || len(msgs) != 1 {
		t.Fatalf("messages 被破坏: %v", obj["messages"])
	}

	if got := string(WithMaxTokens([]byte(`{oops`), 10)); got != `{oops` {
		t.Fatalf("坏 body 应原样返回，got %q", got)
	}
	if got := string(WithMaxTokens(nil, 10)); got != "" {
		t.Fatalf("空 body 应原样返回，got %q", got)
	}
	if got := string(WithMaxTokens(in, 0)); got != string(in) {
		t.Fatalf("非正预算应原样返回，got %q", got)
	}
}

// TestIsPromptTooLongBody 防御路径：报文带 11115 句式即视为越界。
func TestIsPromptTooLongBody(t *testing.T) {
	if !isPromptTooLongBody(`{"code":11115,"msg":"prompt is too long: 1 tokens > 0 maximum"}`) {
		t.Fatal("应识别 11115 报文")
	}
	if !isPromptTooLongBody(`{"msg":"Prompt is Too Long"}`) {
		t.Fatal("无数字的越界文案也应识别")
	}
	if isPromptTooLongBody(`{"code":11102,"msg":"no such model"}`) {
		t.Fatal("无关错误不应识别")
	}
	if isPromptTooLongBody("   ") {
		t.Fatal("空 body 不应识别")
	}
}
