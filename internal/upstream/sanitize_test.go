package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	ccIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
	ccBranch   = "Main branch (you will usually use this for PRs)"
	ccHeader   = "x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli;"
	// Codex instructions 首段（上游逐字精确指纹，三句缺一不可）。
	codexInstructions = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI. You are expected to be precise, safe, and helpful."
)

func TestIdentityRewritten(t *testing.T) {
	out := sanitizeText(ccIdentity)
	if !strings.Contains(out, "official CLI tool for Claude.") {
		t.Errorf("identity not rewritten: %q", out)
	}
	if strings.Contains(out, ccIdentity) {
		t.Errorf("original identity still present: %q", out)
	}
}

// 桌面版（claude-desktop-3p / Agent SDK）的身份句以逗号接后继内容，结尾不是句号。
// 回归用例：匹配串曾带结尾句号，导致该形态漏网、指纹原样发上游 → 400 code=11128。
func TestIdentityDesktopVariantRewritten(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."
	out := sanitizeText(in)
	if strings.Contains(out, "official CLI for Claude") {
		t.Errorf("desktop identity not rewritten: %q", out)
	}
	if !strings.Contains(out, "official CLI tool for Claude, running within the Claude Agent SDK.") {
		t.Errorf("desktop identity suffix not preserved: %q", out)
	}
}

func TestBranchRewritten(t *testing.T) {
	out := sanitizeText(ccBranch)
	if !strings.Contains(out, "Default branch (you will usually use this for PRs)") {
		t.Errorf("branch not rewritten: %q", out)
	}
	if strings.Contains(out, "Main branch") {
		t.Errorf("original branch still present: %q", out)
	}
}

// 反馈句带 Anthropic 仓库链接，上游按整句拦截（实测只留链接或只留半边均不拦）。
// 回归用例：give→provide 一词之差即可绕过。
func TestFeedbackSentenceRewritten(t *testing.T) {
	in := "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"
	out := sanitizeText(in)
	if strings.Contains(out, "To give feedback") {
		t.Errorf("feedback sentence not rewritten: %q", out)
	}
	if !strings.Contains(out, "To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues") {
		t.Errorf("feedback sentence not rewritten as expected: %q", out)
	}
}

// 上游反探测：请求体里出现裸数字 11128 即整单拦截（与上下文无关）。
// 回归用例：该串会被改写为 11-128 以打断精确匹配。
func TestUpstreamErrorCodeRewritten(t *testing.T) {
	in := "upstream returned code=11128 for this request"
	out := sanitizeText(in)
	if strings.Contains(out, "11128") {
		t.Errorf("error code not rewritten: %q", out)
	}
	if !strings.Contains(out, "11-128") {
		t.Errorf("error code not rewritten as expected: %q", out)
	}
}

// 回归：工具调用消息的 content 常为 null，而旧版 sanitizeMessages 在 content 缺失时
// 直接 continue，整条消息连 tool_calls 一起被跳过 → arguments 里的被拦字符串原样漏出。
func TestToolCallArgumentsSanitized(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "run"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{
				"name":      "Bash",
				"arguments": `{"command":"echo 11128"}`,
			}},
		}},
	}
	if !sanitizeMessages(msgs) {
		t.Fatal("sanitizeMessages 未报告任何改动，tool_calls 被跳过")
	}
	fn := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	got := fn["arguments"].(string)
	if strings.Contains(got, "11128") {
		t.Errorf("tool_call arguments 未被净化: %q", got)
	}
}

func TestBillingHeaderStrippedValueIrrelevant(t *testing.T) {
	out := sanitizeText(ccHeader)
	if strings.Contains(out, "x-anthropic-billing-header") {
		t.Errorf("header not stripped: %q", out)
	}
}

// 附加验证：正常对话里出现 github.com/anthropics/ 链接（但不是反馈句整句）
// 时，预检特征命中（进入净化），但改写层只动精确匹配的整句——普通链接文本
// 不该被改写。同理，既不含 11128 也不含反馈整句的文本原样返回。
func TestNormalAnthropicLinkNotRewritten(t *testing.T) {
	in := "see https://github.com/anthropics/anthropic-cookbook for examples"
	if out := sanitizeText(in); out != in {
		t.Errorf("normal anthropic link should be untouched: %q -> %q", in, out)
	}
}

// Codex instructions 首段：命中预告且整句改写，逐字指纹被破坏、语义保留。
func TestCodexInstructionsRewritten(t *testing.T) {
	out := sanitizeText(codexInstructions)
	if strings.Contains(out, codexInstructions) {
		t.Errorf("codex fingerprint still present: %q", out)
	}
	if !strings.Contains(out, "You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.") {
		t.Errorf("codex first sentence not rewritten: %q", out)
	}
	// 第二句「Codex CLI is an open source project led by OpenAI」本身就是上游的
	// system 首行触发 token（可单独出现），现在也一并改写（open source → open-source）；
	// 第三句不含任何指纹，必须逐字保留。
	if !strings.Contains(out, "Codex CLI is an open-source project led by OpenAI.") {
		t.Errorf("codex second sentence should also be rewritten: %q", out)
	}
	if !strings.Contains(out, "You are expected to be precise, safe, and helpful.") {
		t.Errorf("codex third sentence altered: %q", out)
	}
}

// 预检必须能认出 Codex 特征（此前只含 Claude Code，导致提前放行）。
func TestCodexFingerprintDetected(t *testing.T) {
	if !hasFingerprint(codexInstructions) {
		t.Error("codex fingerprint not detected by precheck")
	}
}

// 非精确变体不应被改写：仅去掉其中一个词即视为已破坏，无需改动。
func TestCodexVariantNotTouched(t *testing.T) {
	in := "You are a coding agent running in a CLI, a terminal-based coding assistant."
	if out := sanitizeText(in); out != in {
		t.Errorf("already-broken variant should be untouched: %q -> %q", in, out)
	}
}

func TestBillingHeaderCaseInsensitive(t *testing.T) {
	alt := "X-Anthropic-Billing-Header: cc_version=1.0;"
	out := strings.ToLower(sanitizeText(alt))
	if strings.Contains(out, "billing") {
		t.Errorf("case-insensitive header not stripped: %q", out)
	}
}

func TestTrailingKVStripped(t *testing.T) {
	out := sanitizeText("...; cc_version=2.0; cc_entrypoint=cli;")
	if strings.Contains(out, "cc_version") || strings.Contains(out, "cc_entrypoint") {
		t.Errorf("trailing kv not stripped: %q", out)
	}
}

func TestExactMatchOnlyVariantNotTouched(t *testing.T) {
	in := "...official CLI for Claude!"
	if out := sanitizeText(in); out != in {
		t.Errorf("variant should be untouched: %q -> %q", in, out)
	}
}

func TestUserFreeTextNotTouched(t *testing.T) {
	in := "please use main branch for this repo"
	if out := sanitizeText(in); out != in {
		t.Errorf("free text should be untouched: %q -> %q", in, out)
	}
}

func TestNoFeatureReturnsSameString(t *testing.T) {
	in := "ordinary user message"
	if out := sanitizeText(in); out != in {
		t.Errorf("no-feature text should pass through unchanged: %q -> %q", in, out)
	}
}

func TestMultimodalTextPartOnly(t *testing.T) {
	imgPart := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "..."}}
	content := []any{
		map[string]any{"type": "text", "text": ccIdentity},
		imgPart,
	}
	out, changed := sanitizeContent(content, true)
	if !changed {
		t.Fatal("expected change")
	}
	parts := out.([]any)
	txt, _ := parts[0].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "CLI tool") {
		t.Errorf("text part not sanitized: %q", txt)
	}
	img, _ := parts[1].(map[string]any)
	if img["type"] != "image" || img["source"].(map[string]any)["data"] != "..." {
		t.Error("image part modified")
	}
}

// 集成：完整请求体经 PrepareBodyOpt 净化后无残留指纹，且 stream/tool_choice 行为不受影响。
func TestPrepareBodyOptSanitizesSystem(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + ccIdentity + ` ` + ccHeader + `"},` +
		`{"role":"user","content":"hi"}]}`)
	out := PrepareBodyOpt(body, true)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["stream"] != true {
		t.Error("stream not forced")
	}
	msgs := obj["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, "x-anthropic-billing-header") || strings.Contains(sys, ccIdentity) || strings.Contains(sys, ccBranch) {
		t.Errorf("fingerprints remain: %q", sys)
	}
	if !strings.Contains(sys, "CLI tool") {
		t.Errorf("rewrite missing: %q", sys)
	}
}

func TestPrepareBodyOptDisabledPreservesFingerprints(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"}]}`)
	out := PrepareBodyOpt(body, false)
	if !strings.Contains(string(out), ccIdentity) {
		t.Error("sanitize=false should preserve fingerprints")
	}
	// 但 stream 仍强制
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if obj["stream"] != true {
		t.Error("stream should still be forced")
	}
}

// PrepareBody 默认行为 = 开启脱敏（保持向后兼容）。
func TestPrepareBodyDefaultSanitizes(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"}]}`)
	out := PrepareBodyOpt(body, true)
	if strings.Contains(string(out), ccIdentity) {
		t.Error("PrepareBody default should sanitize")
	}
}

// 出站边界集成：ChatStream 发往上游的 wire body 必须无残留指纹。
func TestChatStreamWireBodySanitized(t *testing.T) {
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.SanitizeFingerprints.Store(true)
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + ccIdentity + ` ` + ccHeader + `"},` +
		`{"role":"user","content":"hi"}]}`)
	rc, status, respBody, err := c.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	// 上游收到的 body：stream 强制 + 指纹已净化
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v", err)
	}
	if obj["stream"] != true {
		t.Error("wire body stream not forced")
	}
	sys, _ := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	for _, fp := range []string{"x-anthropic-billing-header", ccIdentity, ccBranch} {
		if strings.Contains(sys, fp) {
			t.Errorf("wire body contains fingerprint %q: %q", fp, sys)
		}
	}
}

// 出站边界（Codex 场景）：CPA 把 instructions 折成 role=system 的首条消息，
// 净化后 wire body 不得残留 Codex 逐字指纹；其余消息不受影响。
func TestChatStreamWireBodyCodexInstructionsSanitized(t *testing.T) {
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.SanitizeFingerprints.Store(true)
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"kimi-k3","messages":[` +
		`{"role":"system","content":"` + codexInstructions + `"},` +
		`{"role":"user","content":"say ok"}]}`)
	rc, status, respBody, err := c.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v", err)
	}
	sys, _ := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, codexInstructions) ||
		strings.Contains(sys, "in the Codex CLI, a terminal-based coding assistant.") {
		t.Errorf("wire body still contains codex fingerprint: %q", sys)
	}
	if !strings.Contains(sys, "running in the Codex CLI tool, a terminal-based") {
		t.Errorf("codex rewrite missing on wire: %q", sys)
	}
	user, _ := obj["messages"].([]any)[1].(map[string]any)["content"].(string)
	if user != "say ok" {
		t.Errorf("user message altered: %q", user)
	}
}

// 出站边界：关闭脱敏后 wire body 原样保留指纹（验证开关真实有效）。
func TestChatStreamWireBodySanitizeDisabled(t *testing.T) {
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.SanitizeFingerprints.Store(false)
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"}]}`)
	rc, status, _, err := c.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d", status)
	}
	if !strings.Contains(string(gotBody), ccIdentity) {
		t.Error("sanitize disabled should preserve fingerprint on wire")
	}
}

// TestSanitizeMessagesScrubsReasoningField thinking.go 的回填会把客户端送来的
// reasoning_content 镜像进 reasoning 字段；此前只洗 content / reasoning_content /
// tool_calls → 镜像进 reasoning 的指纹（裸 "11128" 这类反探测串）原样出站，
// 而请求体里出现裸 11128 本身就是上游整单拦截条件。
func TestSanitizeMessagesScrubsReasoningField(t *testing.T) {
	ms := []any{map[string]any{
		"role":      "assistant",
		"content":   "hi",
		"reasoning": "upstream said 11128",
	}}
	if !sanitizeMessages(ms) {
		t.Fatal("reasoning 字段里的指纹未被净化")
	}
	got, _ := ms[0].(map[string]any)["reasoning"].(string)
	if strings.Contains(got, "11128") {
		t.Fatalf("reasoning 仍含裸指纹: %q", got)
	}
}

const (
	dshGUIA = "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names."
	dshGUIB = `When the user refers to "this page", "this GUI", or "this app" without naming another target, they mean this GUI.`
)

func TestIdentitySentenceRewritten(t *testing.T) {
	cases := map[string]string{
		"You are an AI agent powered by DeepSeek Harness.":                                    "You are a helpful assistant.",
		"You are an AI assistant built on DeepSeek. Use tools wisely.":                        "You are a helpful assistant. Use tools wisely.",
		"You are Codex Agent, built by OpenAI.":                                               "You are a helpful assistant.",
		"You are an assistant created by Example. Next.":                                      "You are a helpful assistant. Next.",
		"You are a model developed by Example.":                                               "You are a helpful assistant.",
		"You are the AI made by Example.":                                                     "You are a helpful assistant.",
		"you are an llm TRAINED BY someone.":                                                  "You are a helpful assistant.",
		"You are powered by the model named Claude Opus 4.5. The exact model ID is x-4-5.":    "You are a helpful assistant. The exact model ID is x-4-5.",
		"Intro.\nYou are a coding agent powered by the deepseek-v4.1-flash model.\nTools: a.": "Intro.\nYou are a helpful assistant.\nTools: a.",
		"You are an AI agent powered by DeepSeek Harness\nnext line":                          "You are a helpful assistant.\nnext line",
	}
	for in, want := range cases {
		if got := sanitizeText(in); got != want {
			t.Errorf("sanitizeText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIdentityLookalikeNotTouched(t *testing.T) {
	for _, in := range []string{
		"You are free to use tools built by the community.",
		"This plugin is powered by DeepSeek and made by volunteers.",
		"You are an agent. The CLI was built by OpenAI.",
	} {
		if got := sanitizeText(in); got != in {
			t.Errorf("should be untouched: %q -> %q", in, got)
		}
	}
}

func TestDSHGUIParagraphRewritten(t *testing.T) {
	want := "You are interacting with the user through the DeepSeek Web GUI whose URL is named in the runtime context. " + dshGUIB
	if got := sanitizeText(dshGUIA + " " + dshGUIB); got != want {
		t.Errorf("GUI paragraph:\n got %q\nwant %q", got, want)
	}
	lower := strings.ToLower(dshGUIA)
	if got := sanitizeText(lower); strings.Contains(got, "the runtime context names") || strings.Contains(got, "harness") {
		t.Errorf("case-insensitive GUI rewrite failed: %q", got)
	}
}

func TestDSHBrandRewrittenOnlyInProse(t *testing.T) {
	if got := sanitizeText("Built with DeepSeek\tHarness today"); got != "Built with DeepSeek today" {
		t.Errorf("brand not reduced: %q", got)
	}
	for _, in := range []string{
		"github.com/deepseek-ai/deepseek-harness/tree/main",
		"import deepseek_harness as dsh",
		"/opt/deepseek-harness/app",
		"a test harness for DeepSeek",
	} {
		if got := sanitizeText(in); got != in {
			t.Errorf("identifier should be untouched: %q -> %q", in, got)
		}
	}
}

func TestOmOIdentityRewritten(t *testing.T) {
	in := "You are Sisyphus-Junior - Focused executor from OhMyOpenCode, a helper."
	if got := sanitizeText(in); got != "You are Sisyphus-Junior - Focused executor, a helper." {
		t.Errorf("OmO not rewritten: %q", got)
	}
	for _, in := range []string{"Sisyphus-Junior alone", "from OhMyOpenCode alone"} {
		if got := sanitizeText(in); got != in {
			t.Errorf("half should be untouched: %q -> %q", in, got)
		}
	}
}

func TestChannelFingerprintRewriteIdempotent(t *testing.T) {
	for _, in := range []string{
		dshGUIA + " " + dshGUIB,
		"You are an AI agent powered by DeepSeek Harness. Be brief.",
		"Sisyphus-Junior - Focused executor from OhMyOpenCode",
	} {
		once := sanitizeText(in)
		if twice := sanitizeText(once); twice != once {
			t.Errorf("not idempotent: %q -> %q -> %q", in, once, twice)
		}
	}
}

// TestChannelFingerprintScrubbedPerRoleScope 身份域指纹的净化作用域：system /
// developer / assistant 承载「客户端是谁」的指纹，必须洗掉；user / tool 里出现
// 同样的句子属**用户数据**（用户粘贴的 prompt、工具读到的文件内容），除反探测串
// 外必须逐字不动——改写等于篡改用户输入。
func TestChannelFingerprintScrubbedPerRoleScope(t *testing.T) {
	orig := []string{
		"You are an AI agent powered by DeepSeek Harness.",
		dshGUIA + " " + dshGUIB,
		"You are the AI made by Example.",
		dshGUIA,
	}
	roles := []string{"system", "user", "assistant", "tool"}
	ms := make([]any, 0, len(orig))
	for k, role := range roles {
		ms = append(ms, map[string]any{"role": role, "content": orig[k]})
	}
	if !sanitizeMessages(ms) {
		t.Fatal("expected identity-domain messages to change")
	}
	for k, m := range ms {
		got := m.(map[string]any)["content"].(string)
		if sanitizeIdentityRole(roles[k]) {
			if got == orig[k] {
				t.Errorf("role=%s identity fingerprint not scrubbed: %q", roles[k], got)
			}
			continue
		}
		if got != orig[k] {
			t.Errorf("role=%s user data rewritten (must stay verbatim): %q -> %q", roles[k], orig[k], got)
		}
	}
}

// TestAntiProbeScrubbedInEveryRole 反探测串与身份域无关：裸 11128 出现在任何角色
// 的消息里都是上游整单拦截条件（用户粘贴报错、工具回显错误码），必须照洗。
func TestAntiProbeScrubbedInEveryRole(t *testing.T) {
	const probe = "11" + "128"
	in := "upstream said " + probe + " again"
	for _, role := range []string{"system", "developer", "assistant", "user", "tool"} {
		ms := []any{map[string]any{"role": role, "content": in}}
		if !sanitizeMessages(ms) {
			t.Fatalf("role=%s: expected anti-probe rewrite", role)
		}
		got := ms[0].(map[string]any)["content"].(string)
		if strings.Contains(got, probe) {
			t.Errorf("role=%s still carries bare anti-probe token: %q", role, got)
		}
	}
}

// TestIdentityReplacementIsNotBlockedOpening P3：改写串自身不能落在上游「禁用开头」
// 名单里（名单第一条就是带 AI 的那句）。洗成另一句可疑开头等于没洗。
func TestIdentityReplacementIsNotBlockedOpening(t *testing.T) {
	if sanitizeIdentityReplacement != "You are a helpful "+"assistant." {
		t.Fatalf("replacement=%q", sanitizeIdentityReplacement)
	}
	if strings.Contains(strings.ToLower(sanitizeIdentityReplacement), " ai ") {
		t.Errorf("replacement still contains AI token: %q", sanitizeIdentityReplacement)
	}
}

func TestChannelFingerprintScrubbedInArgumentsReasoningAndMultimodal(t *testing.T) {
	imgPart := map[string]any{"type": "image", "source": map[string]any{"data": "DeepSeek Harness"}}
	ms := []any{map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "text", "text": dshGUIA + " " + dshGUIB},
			imgPart,
		},
		"reasoning_content": "You are an AI assistant created by Example.",
		"reasoning":         "Built with DeepSeek Harness today",
		"tool_calls": []any{map[string]any{"function": map[string]any{
			"arguments": `{"prompt":"Sisyphus-Junior - Focused executor from OhMyOpenCode"}`,
		}}},
	}}
	if !sanitizeMessages(ms) {
		t.Fatal("expected channel fingerprints to be scrubbed")
	}
	m := ms[0].(map[string]any)
	parts := m["content"].([]any)
	wantText := "You are interacting with the user through the DeepSeek Web GUI whose URL is named in the runtime context. " + dshGUIB
	if got := parts[0].(map[string]any)["text"]; got != wantText {
		t.Errorf("multimodal text = %q, want %q", got, wantText)
	}
	if got := imgPart["source"].(map[string]any)["data"]; got != "DeepSeek Harness" {
		t.Errorf("non-text part modified: %q", got)
	}
	if got := m["reasoning_content"]; got != sanitizeIdentityReplacement {
		t.Errorf("reasoning_content = %q", got)
	}
	if got := m["reasoning"]; got != "Built with DeepSeek today" {
		t.Errorf("reasoning = %q", got)
	}
	args := m["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	var decoded map[string]string
	if err := json.Unmarshal([]byte(args), &decoded); err != nil {
		t.Fatalf("sanitized arguments are invalid JSON: %v", err)
	}
	if got := decoded["prompt"]; got != "Sisyphus-Junior - Focused executor" {
		t.Errorf("tool arguments prompt = %q", got)
	}
}

func TestContainsFoldASCII(t *testing.T) {
	if !containsFoldASCII("xx DeepSeek HARNESS", "harness") || containsFoldASCII("harnes", "harness") || containsFoldASCII("", "a") {
		t.Fatal("containsFoldASCII wrong")
	}
}

// newTestUpstream 起一个假上游并捕获请求。
func newTestUpstream(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(h)
}

// TestSanitizeCodexStandaloneToken Codex 说明句**单独出现**时也要命中。
//
// 既有 codexInstructions 常量是「三句连排」的整段，靠第一句的 rewrite 就带过了；
// 但上游的触发判据是「system 首行含该 token」，单独一句同样会被拦（dddmiku
// 2026-10-17 实测）。所以这条要能独立生效。
func TestSanitizeCodexStandaloneToken(t *testing.T) {
	const alone = "Codex CLI is an open source project led by OpenAI."
	out := sanitizeText(alone)
	if strings.Contains(out, "Codex CLI is an open source project led by OpenAI") {
		t.Fatalf("单独出现的 Codex 说明句未被改写: %q", out)
	}
	if !strings.Contains(out, "open-source project led by OpenAI") {
		t.Fatalf("改写后语义应保留 open-source: %q", out)
	}
	if !hasFingerprint(alone) {
		t.Fatal("预检特征未命中单独出现的 Codex 说明句")
	}
}

// TestSanitizeDSHPathSentence 两句路径说明连排时拆断第一句、第二句原样。
// 2026-10-27 实测：连排 400（渠道校验），单句各自 200。
func TestSanitizeDSHPathSentence(t *testing.T) {
	const first = "Tokens prefixed with @ are paths the user explicitly referenced."
	const second = "Relative paths resolve from the workspace root; absolute paths identify files or directories on the host."
	out := sanitizeText(first + "\n" + second)

	if strings.Contains(out, "are paths the user explicitly referenced") {
		t.Fatalf("连排形态未拆断: %q", out)
	}
	if !strings.Contains(out, "are paths that the user explicitly referenced.") {
		t.Fatalf("第一句未被改写（that 未插入）: %q", out)
	}
	if !strings.Contains(out, second) {
		t.Fatalf("第二句不该被改动: %q", out)
	}
	if !hasFingerprint(first + "\n" + second) {
		t.Fatal("预检特征未命中 DSH 路径句")
	}
}

// TestSanitizeDSHPathSecondOnlyUntouched 第二句单独出现不得被改写（避免误伤用户文本）。
func TestSanitizeDSHPathSecondOnlyUntouched(t *testing.T) {
	const second = "Relative paths resolve from the workspace root; absolute paths identify files or directories on the host."
	if got := sanitizeText(second); got != second {
		t.Fatalf("第二句单独出现被误改: %q", got)
	}
}

// TestSanitizeContentPartsArray 回归 dddmiku 实测：system 为 part 数组时，
// 任一块首行含触发 token 同样被拦。净化必须走每个 text 块。
func TestSanitizeContentPartsArray(t *testing.T) {
	msgs := []any{
		map[string]any{
			"role": "system",
			"content": []any{
				map[string]any{"type": "text", "text": "Codex CLI is an open source project led by OpenAI."},
				map[string]any{"type": "text", "text": "Tokens prefixed with @ are paths the user explicitly referenced."},
			},
		},
	}
	sanitizeMessages(msgs)
	raw, _ := json.Marshal(msgs)
	s := string(raw)
	if strings.Contains(s, "Codex CLI is an open source project led by OpenAI") {
		t.Errorf("part 数组里的 Codex 说明句未改写: %s", s)
	}
	if strings.Contains(s, "are paths the user explicitly referenced") {
		t.Errorf("part 数组里的 DSH 路径句未改写: %s", s)
	}
	if !strings.Contains(s, "open-source") || !strings.Contains(s, "are paths that") {
		t.Errorf("改写结果不符合预期: %s", s)
	}
}
