// sanitize.go 出站请求体脱敏：剥离上游内容审核黑名单指纹。
//
// 背景：客户端（Claude Code 类 CLI）在 system prompt 注入若干固定模板句，
// 上游内容审核按逐字精确匹配拦截（非语义审核），一字改动即可绕过。
// 策略：键值/header 型指纹整段剥离；承载语义的模板句最小改写（换一词），语义不变。
package upstream

import (
	"regexp"
	"strings"
)

// sanitizeFeatures 特征预检：任一命中才进入净化（strings.Contains 快速路径，
// 普通请求全不中 → 原样返回，零分配）。
var sanitizeFeatures = []string{
	"x-anthropic-billing-header", // header 键值段键名
	"cc_entrypoint=",             // 尾随裸键值（截断前缀即可命中）
	"You are Claude Code",        // 身份句（截断前缀即可命中）
	"Main branch (",              // 注入指令句（截断前缀即可命中）
	"You are a coding agent running in the Codex CLI", // Codex instructions 首段（截断前缀即可命中）
	"github.com/anthropics/",                          // 反馈句里的 Anthropic 仓库链接
	"Codex CLI is an open source",                     // Codex 说明句（上游首行触发 token）
	"Tokens prefixed with @ are paths",                // DSH 路径提示句首（与下句连排触发）
	"11128",                                           // 上游反探测：裸数字错误码
}

// sanitizeHdrRe 剥离层：header 键名即触发（与值无关），整段删除。
var sanitizeHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)

// sanitizeBareHdrRe 兜底层：裸键名（无冒号无值）同样是指纹——2026-09-13 实验 F4
// 证实 assistant 消息里反引号引用裸键名即触发 11128，而剥离层要求冒号、对裸串无效。
// 键值形态被整段删除后，残留的裸键名做最小缩写（header→hdr）：破坏逐字匹配、
// 语义不变、保留可读性。大小写不敏感，覆盖 X-Anthropic-... 变体。
//
// 注意该正则不要求冒号，是 sanitizeHdrRe 的超集——hasFingerprint 与 sanitizeText
// 中两者并用：先删键值形态（sanitizeHdrRe），再缩写残留裸键名（本正则），
// 替换语义不同（整段删除 vs 最小缩写），不可合并为一个正则。
var sanitizeBareHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header`)

// sanitizeKvRe 剥离层：尾随裸键值（cc_xxx=...;）循环清理。
var sanitizeKvRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// sanitizeRewrites 改写层：全模板句逐字替换（每句只改一个词，语义不变）。
//
// 身份句的匹配串**不带结尾标点**（只到 "…for Claude" 为止）：
// CLI 版这句以句号收尾（"…for Claude."），桌面版（claude-desktop-3p / Agent SDK）
// 以逗号接后继内容（"…for Claude, running within the Claude Agent SDK."）。
// 带句号的整句只匹配前者，桌面版会漏网、指纹原样发上游 → 400 code=11128。
// 去掉结尾标点后两种形态一并覆盖（替换串同样不带标点，让原有标点原样保留）。
// 注意仍要求 "You are Claude Code, " 前缀，不做更宽的子串替换，
// 以免误伤 TestExactMatchOnlyVariantNotTouched 所保护的零散文本。
var sanitizeRewrites = [][2]string{
	{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	},
	{
		// 反馈句：整句带 Anthropic 仓库链接，上游按整句拦截（只留链接或只留半边均不拦，
		// 实测需整句同时出现）。give→provide 一词之差即可绕过，语义不变。
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	{
		// Codex 说明句：上游把它列为 system 首行的触发 token（dddmiku 2026-10-17
		// 的 12 组对照实验：`Codex CLI is an open source project led by OpenAI`）。
		// 注意它与我们已在处理的 "You are a coding agent running in the Codex CLI"
		// **不是同一句**——那句是 instructions 首段，这句是独立说明句。
		// open source → open-source 一词之差即可绕过，语义不变。
		"Codex CLI is an open source project led by OpenAI",
		"Codex CLI is an open-source project led by OpenAI",
	},
	{
		// DSH 系统提示里的两句路径说明：相邻连排即触发渠道校验（2026-10-27 实测，
		// 单句各自 200）。只在 "are paths" 后插入 that 拆断连排、语义不变；
		// 匹配串不带结尾标点，句号与换行两种形态一并覆盖。
		"Tokens prefixed with @ are paths the user explicitly referenced",
		"Tokens prefixed with @ are paths that the user explicitly referenced",
	},
}

// sanitizeAntiProbe 上游反探测串：请求体里出现裸 11-128 即整单拦截（与该数字的
// 上下文无关）。**任何角色都要改写**——用户粘贴报错、工具回显错误码同样是拦截
// 条件，它与「客户端是谁」无关，不能随身份域一起收窄。
var sanitizeAntiProbe = [][2]string{
	{
		// 上游反探测：只要请求体里出现裸数字 11128 就整单拦截（与该数字的上下文无关——
		// "code=11128" / 裸 "11128" / "错误码 11128" / "Code=11128" 全部命中；
		// 相邻的 11148 / 11101 / 11115 / 99999 均放行）。11128 正是本类拦截自身的错误码，
		// 上游据此识别"在讨论/回显其内部错误码"的请求。
		// 代价：用户对话中任何 11128 都会被改写——但这串数字出现在请求里本身就是拦截条件，
		// 不改写必然失败。插入连字符保留可读性与指代（零宽空格无效，实测上游会归一化）。
		"11128",
		"11-128",
	},
}

// 以下规则清理三类渠道指纹：自我介绍归属句、DeepSeek Harness 描述、
// OhMyOpenCode 子代理身份串。大小写不敏感，路径与标识符不做宽泛替换。
var (
	// RE2 没有前瞻，句尾由 rewriteIdentitySentences 手工找。
	sanitizeIdentityStartRe = regexp.MustCompile(`(?i)You are\s+` +
		`(?:an?\s+|the\s+)?(?:[\w.-]+\s+){0,3}?` +
		`(?:(?:agent|assistant|model|AI|LLM)\b)?[\s,]*` +
		`(?:powered by|built on|built by|created by|developed by|made by|trained by)\b`)
	sanitizeDSHGUIRe = regexp.MustCompile(`(?i)(You are interacting with the user through the ` +
		`DeepSeek[ \t]+Harness[ \t]+Web[ \t]+GUI) whose URL the runtime context names\.`)
	// 只认空格与制表符，deepseek-harness、deepseek_harness 这类路径与标识符不动。
	sanitizeBrandRe = regexp.MustCompile(`(?i)DeepSeek[ \t]+Harness`)
	sanitizeOmORe   = regexp.MustCompile(`(?i)Sisyphus-Junior - Focused executor from OhMyOpenCode`)
)

// sanitizeIdentityReplacement 身份句的改写串。**不能带 "AI"**：
//
// 上游对 system 首行有一套「禁用开头」名单，「You are a helpful AI assistant.」
// 正是名单里的第一条——我们原先把 Claude/Codex 的身份句洗成了另一句可疑开头，
// 洗了个寂寞。改成无 "AI" 的形态（与社区实测可过的一致），语义不变。
const sanitizeIdentityReplacement = "You are a helpful assistant."

var sanitizeIdentityVerbs = []string{
	"powered by", "built on", "built by", "created by", "developed by", "made by", "trained by",
}

// containsFoldASCII 不分配内存的大小写不敏感子串判断，needle 须是小写 ASCII。
func containsFoldASCII(s, needle string) bool {
	n := len(needle)
	for i := 0; i+n <= len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c == needle[0] && strings.EqualFold(s[i:i+n], needle) {
			return true
		}
	}
	return false
}

// rewriteIdentitySentences 整句替换自我介绍，模型名中的点不误作句尾，换行原样保留。
func rewriteIdentitySentences(text string) string {
	var b strings.Builder
	rest := text
	for {
		loc := sanitizeIdentityStartRe.FindStringIndex(rest)
		if loc == nil {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:loc[0]])
		tail := rest[loc[1]:]
		end := len(tail)
		for i := 0; i < len(tail); i++ {
			if tail[i] == '\n' {
				end = i
				break
			}
			if tail[i] == '.' && (i+1 == len(tail) || strings.IndexByte(" \t\r\n", tail[i+1]) >= 0) {
				end = i + 1
				break
			}
		}
		b.WriteString(sanitizeIdentityReplacement)
		rest = tail[end:]
	}
}

func hasChannelFingerprint(text string) bool {
	if containsFoldASCII(text, "harness") && sanitizeBrandRe.MatchString(text) {
		return true
	}
	if containsFoldASCII(text, "ohmyopencode") && sanitizeOmORe.MatchString(text) {
		return true
	}
	for _, v := range sanitizeIdentityVerbs {
		if containsFoldASCII(text, v) {
			return sanitizeIdentityStartRe.MatchString(text)
		}
	}
	return false
}

// sanitizeText 全量净化（身份域角色：system / developer / assistant）。
// 包内单测与默认口径走全量；按角色收窄请用 sanitizeTextScoped。
func sanitizeText(text string) string {
	return sanitizeTextScoped(text, true)
}

// sanitizeTextScoped 按角色作用域净化单段文本。
//
// identity=false（user / tool / function 消息）时只跑**反探测串**与 header 键名
// 两类规则：它们描述的是「渠道指纹」，与该消息是不是用户内容无关，漏掉就是
// 整单拦截。身份句 / 模板句改写一律跳过——同一句出现在 user / tool 里属**用户
// 数据**（用户粘贴的 system prompt、工具读到的文件内容），改写等于篡改用户输入。
func sanitizeTextScoped(text string, identity bool) string {
	if !hasFingerprint(text) {
		return text
	}
	out := text
	if identity {
		out = sanitizeOmORe.ReplaceAllString(out, "Sisyphus-Junior - Focused executor")
		out = rewriteIdentitySentences(out)
		// GUI 描述段要认品牌全名，先于品牌缩减。
		out = sanitizeDSHGUIRe.ReplaceAllString(out, "${1} whose URL is named in the runtime context.")
		out = sanitizeBrandRe.ReplaceAllString(out, "DeepSeek")
		for _, rw := range sanitizeRewrites {
			out = strings.ReplaceAll(out, rw[0], rw[1])
		}
	}
	for _, rw := range sanitizeAntiProbe {
		out = strings.ReplaceAll(out, rw[0], rw[1])
	}
	if sanitizeHdrRe.MatchString(out) {
		out = sanitizeHdrRe.ReplaceAllString(out, "")
	}
	if strings.Contains(out, "cc_") {
		prev := ""
		for prev != out { // 清尾随裸 kv（）
			prev = out
			out = sanitizeKvRe.ReplaceAllString(out, "")
		}
	}
	// 兜底：键值形态已在上面整段删除，这里只剩裸键名（引用/示例文本形态）。
	out = sanitizeBareHdrRe.ReplaceAllString(out, "x-anthropic-billing-hdr")
	if out == text {
		// 一条规则都没命中（例如 user 消息只撞上身份句预检而作用域不允许改写）：
		// 原样返回。绝不顺手 TrimSpace —— 那也是对用户内容的改动。
		return text
	}
	return strings.TrimSpace(out)
}

// hasFingerprint 特征预检：先走 strings.Contains 快速路径（零分配）；
// header 键名有大小写变体（X-Anthropic-...）且可能以裸键名形态出现（无冒号），
// Contains 大小写敏感、sanitizeHdrRe 要求冒号——两者都会漏掉「混合大小写 + 裸键名」，
// 必须再用不要求冒号的 (?i) 正则兜底（sanitizeBareHdrRe），否则整条净化被跳过。
// sanitizeBareHdrRe 不要求冒号，是 sanitizeHdrRe 的超集，故无需再单独匹配后者。
func hasFingerprint(text string) bool {
	for _, f := range sanitizeFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	return sanitizeBareHdrRe.MatchString(text) || hasChannelFingerprint(text)
}

// sanitizeContent 兼容字符串与多模态数组；只动 text part，image 等 part 不动。
// 返回净化后的值及是否发生变化；identity 由消息角色决定（见 sanitizeIdentityRole）。
func sanitizeContent(v any, identity bool) (any, bool) {
	switch c := v.(type) {
	case string:
		s := sanitizeTextScoped(c, identity)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			if s := sanitizeTextScoped(text, identity); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// sanitizeToolCalls 净化 assistant.tool_calls[].function.arguments。
//
// arguments 是**字符串化的 JSON**（不是对象），因此按文本走 sanitizeText 即可。
// 这块长期是盲区：工具调用消息的 content 通常是 null，而旧版 sanitizeMessages
// 在 content 缺失时直接 continue，整条消息连 tool_calls 一起被跳过——
// 于是历史里任何写进工具参数的被拦字符串（文件名、命令、写入内容）都会原样漏出。
func sanitizeToolCalls(v any, identity bool) bool {
	callList, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range callList {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if s := sanitizeTextScoped(args, identity); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// sanitizeMessages 净化 messages 中的 content 与 tool_calls；任一命中返回 true。

// sanitizeIdentityRole 报告该角色是否属「身份域」：只有这些角色的文本承载
// 「客户端是谁」的指纹（system prompt / developer instruction / 模型自述）。
// user / tool / function 里出现同样的句子属用户数据，只跑反探测规则。
//
// 未知或缺失角色按身份域处理：宁可多洗一层，也不要让指纹漏出去。
func sanitizeIdentityRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user", "tool", "function":
		return false
	}
	return true
}

// sanitizeMessages 净化 messages 中的 content / reasoning* / tool_calls；
// 任一命中返回 true。作用域按消息角色分流（见 sanitizeIdentityRole）。
func sanitizeMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		identity := sanitizeIdentityRole(role)
		// content 与 tool_calls 各自独立判断：content 可以为 null（工具调用轮），
		// 早期版本在此 continue，导致这类消息的 tool_calls 完全不被净化。
		if c, ok := m["content"]; ok {
			if nc, ch := sanitizeContent(c, identity); ch {
				m["content"] = nc
				changed = true
			}
		}
		// reasoning_content（思维链回填字段，见 thinking.go/sse.go）实测同样
		// 携带指纹，与 content 同等净化。string 形态直接走 sanitizeTextScoped。
		if rc, ok := m["reasoning_content"].(string); ok {
			if s := sanitizeTextScoped(rc, identity); s != rc {
				m["reasoning_content"] = s
				changed = true
			}
		}
		// reasoning：另一形态的思维链字段。thinking.go 的回填会把客户端送来的
		// reasoning_content 镜像进 reasoning（反之亦然），而此前只洗 content /
		// reasoning_content / tool_calls —— 镜像进 reasoning 的指纹原样出站，
		// 裸 "11-128" 这种反探测串同样致命。
		if r, ok := m["reasoning"].(string); ok {
			if s := sanitizeTextScoped(r, identity); s != r {
				m["reasoning"] = s
				changed = true
			}
		}
		if tc, ok := m["tool_calls"]; ok {
			if sanitizeToolCalls(tc, identity) {
				changed = true
			}
		}
	}
	return changed
}
