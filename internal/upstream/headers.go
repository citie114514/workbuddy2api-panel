// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

const (
	// defaultClientVersion 出站 WorkBuddy 客户端版本段（UA 的 `WorkBuddy/<ver>` 与
	// 白名单头组的 X-IDE-Version）。对齐官方 WorkBuddy Desktop 分发包版本（5.11.3）。
	// config upstream.client_version 可覆盖（空 = 内置默认）。
	defaultClientVersion = "5.11.3"
	// defaultCliVersion 出站 UA 中 `CLI/<ver>` 段版本。对齐官方桌面端内置 CLI（2.176.0）。
	// config upstream.cli_version 可覆盖（空 = 内置默认）。
	defaultCliVersion = "2.176.0"

	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// setExact 以**给定的大小写**写入请求头。
//
// http.Header.Set 会把键名规范化成 textproto 的 CanonicalMIMEHeaderKey（每个词首字母
// 大写、其余小写）：`x-stainless-arch` → `X-Stainless-Arch`、`X-B3-TraceId` →
// `X-B3-Traceid`、`X-Conversation-ID` → `X-Conversation-Id`。而官方客户端是 Node，
// 直接按代码里写的键名发送，抓包实测就是 `x-stainless-*` 全小写、`X-B3-TraceId` 保留
// 大写的 Id、`traceparent`/`baggage`/`b3` 全小写。
//
// 直接写 map 可以绕过规范化：net/http 落盘时按 map 里的键原样写出（只对键排序）。
//
// 约束：用本函数写的头**不能用 Header.Get/Del 按规范键名读回**（Get 会查规范形）。
// 目前这些头只写不读，符合约束；将来若要读，请用 map 直接索引。
func setExact(h http.Header, key, value string) {
	h[key] = []string{value}
}

// originRefererFor 按账号 realm 返回 Origin/Referer 基础域：
// global → https://www.workbuddy.ai；cn（含全局开关未开）→ https://www.codebuddy.cn。
func originRefererFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return originRefererGlobal
	}
	return originRefererCN
}

// clientVersion 生效的 WorkBuddy 客户端版本：Client.ClientVersion 非空则取之，
// 否则内置默认 defaultClientVersion。
func (c *Client) clientVersion() string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	return defaultClientVersion
}

// cliVersion 生效的 CLI 版本：Client.CliVersion 非空则取之，否则内置默认 defaultCliVersion。
func (c *Client) cliVersion() string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	return defaultCliVersion
}

// defaultWorkBuddyUAFor 组装默认客户端出站 UA（官方桌面端 RestOperations 层形状）：
// `WorkBuddy/<clientVersion> <platform>/<clientVersion> CLI/<cliVersion>`。
// 平台段（第二段）品牌按 realm 切换——CN 用 applicationName 同值 `WorkBuddy`，
// global 用官方国际版 productName `WorkBuddy AI`（intl 项目逆向证据：
// `WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2`）。
// global 账号送错平台段（`WorkBuddy` 非 `WorkBuddy AI`）可能触发上游 403 code 11140
// "request illegal" 风控。官方无任何 UA 随机化，故默认确定性。
func (c *Client) defaultWorkBuddyUAFor(a *auth.Auth) string {
	platform := "WorkBuddy"
	if a != nil && a.IsGlobal() {
		platform = "WorkBuddy AI"
	}
	return "WorkBuddy/" + c.clientVersion() + " " + platform + "/" + c.clientVersion() + " CLI/" + c.cliVersion()
}

// defaultWorkBuddyUA 返回 CN 形态的默认 UA（默认账号形态即 CN，零回归兼容既有调用/测试）。
func (c *Client) defaultWorkBuddyUA() string {
	return c.defaultWorkBuddyUAFor(nil)
}

// userAgent 返回当前出站 UA（客户端出站路径：chat/refresh/FetchModels）。
// 优先级：Client.UserAgent（config user_agent）显式覆盖 > 按账号 realm 的默认 WorkBuddy 三段式。
// 显式覆盖兼容既有覆盖逻辑：用户配了即以用户值为准（自定义品牌/版本），
// 未配则走官方桌面端默认形态（global 换 `WorkBuddy AI` 平台段）。
func (c *Client) userAgent(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return c.defaultWorkBuddyUAFor(a)
}

// billingUA 白名单类（billing/checkin/banner）出站 UA：单段 `WorkBuddy/<clientVersion>`
// （官方 banner 显式覆写形态，不带 CLI 段）。默认生效（伪造官方桌面端指纹）；
// 显式 client_name="SaaS" 才不设 UA（还原旧行为，Go 默认 UA）。
func (c *Client) billingUA() string {
	if c == nil || c.attributionClientName() == "SaaS" {
		return ""
	}
	return "WorkBuddy/" + c.clientVersion()
}

// reportUA `/v2/report` 出站 UA：单段 `WorkBuddy/<clientVersion>`。
//
// 该通道的发送方是桌面端守护进程，其 UA 由 `applicationName/appVersion` 组成，
// 与对话链的三段 UA（`WorkBuddy/<v> WorkBuddy/<v> CLI/<v>`）不是一回事。
// 显式配置 c.UserAgent 时优先（运维逃生门，同其它头族）。
func (c *Client) reportUA() string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return "WorkBuddy/" + c.clientVersion()
}

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
// 三者皆空/读失败则返回空串（调用方不注入该头，优雅降级）。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// injectDeviceToken 在 req 注入 X-Device-Token 头（仅当取到非空 token）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// deriveAccountStableID 按 uid + 用途盐稳定派生 36 hex 设备/会话标识。
// 跨重启稳定（固定盐 "wb2a:"，不随进程换——这是与 session 包派生盐的本质差异：
// 那是会话键维度的进程级随机盐，重启换新；本函数是账号维度，必须跨重启恒定）、
// 账号间互异（uid 不同则不同）、同 uid 同用途恒同值（幂等）。用 sha256 与项目
// 既有派生（session/ids.go、cache_key.go）保持一致；截 36 hex 提供更长熵。
//
// 两个用途：
//   - purpose="machine" → X-Machine-ID（设备级，跨会话稳定）
//   - purpose="session" → X-Session-ID（账号固定会话，跨重启稳定）
//
// 与 injectDeviceToken 的 X-Device-Token 并存不冲突：那是登录时上游签发的
// 真实设备令牌（有则发，权威）；本对头是「每账号一台固定虚拟设备」的稳定指纹，
// 防多号被上游按设备指纹缺失/漂移关联风控。两者是不同头族，官方桌面端都发。
func deriveAccountStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

// injectAccountStableHeaders 在 req 注入 X-Session-ID：按 uid 稳定派生，跨重启
// 固定、账号间互异。uid 为空时不注入（匿名请求无设备标识，上游不要求）。
//
// 只发 X-Session-ID，不发 X-Machine-ID：内置 CLI 的 header 常量表里有
// `SESSION_ID_HEADER = "X-Session-ID"`，而 `X-Machine-ID` 在整个
// codebuddy-lite-wb.mjs 与主进程包里都是 0 次——它不是官方头，发出去就是一条
// 只有网关才有的特征。设备级的稳定标识走上报事件的 machineId（见 desktop.go），
// 那是官方真有的字段。
func (c *Client) injectAccountStableHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || a.UID == "" {
		return
	}
	req.Header.Set("X-Session-ID", deriveAccountStableID(a.UID, "session"))
}

// CommonHeaders 设置所有 API 共享的请求头。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	// Accept 非流式默认 application/json（D6：去掉宽松的 text/plain, */*）。
	// chat 流式路径在 ChatHeaders 覆盖为 event-stream。
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.userAgent(a))
	// x-codebuddy-request: 1（官方客户端风控闸门头，所有 API 请求必带，D1）。
	// 用 setExact 写**小写**键：Header.Set 会把键规范化成 `X-Codebuddy-Request`，
	// 与官方线上的小写形态不一致；而两种写法各写一次会发出去两个同名头。
	setExact(req.Header, "x-codebuddy-request", "1")
	// Accept-Language 按 realm 切（D5）：CN zh-CN，global en-US。官方客户端按账号域
	// 发对应语言标识，对齐避免上游风控按语言缺失误判。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	// X-Machine-ID / X-Session-ID：按 uid 稳定派生的账号级设备头（见
	// injectAccountStableHeaders）。注入在 CommonHeaders——chat 经 ChatHeaders
	// 叠加 CommonHeaders 天然继承；billing 域另行注入，全出站覆盖。
	c.injectAccountStableHeaders(req, a)
	// 账号 UID 进上下文：出站连接池按账号分桶的唯一依据（不上线、不改任何字节）。
	attachAccountUID(req, a)
}

// acceptLanguageFor 按账号 realm 返回 Accept-Language：global → en-US，cn → zh-CN。
func acceptLanguageFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return "en-US"
	}
	return "zh-CN"
}

// injectCodeBuddyRequest 在 req 注入 X-CodeBuddy-Request: 1。
// billing 域未走 CommonHeaders，单独注入保证全出站覆盖（D1）。
func (c *Client) injectCodeBuddyRequest(req *http.Request) {
	setExact(req.Header, "x-codebuddy-request", "1")
}

// ChatMeta 一次 chat 出站的会话头族元数据（issue #35：后台按 X-Conversation-Request-ID
// 聚合请求，官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID）。
// handler 在轮转循环外生成 conversationID / conversationRequestID，循环内每次出站
// 复用同值；TraceID 透传入站值（空 = 回落 conversationRequestID）。
// messageID（消息级，每条独立）由 ChatHeaders 内部生成，无需外部可见。
type ChatMeta struct {
	ConversationID        string // X-Conversation-ID：body 提取的入站值，空则不发（透传优先，不伪造）
	ConversationRequestID string // X-Conversation-Request-ID / X-Root-Request-ID：聚合主键，必发
	TraceID               string // X-Trace-ID：入站透传值，空则回落 conversationRequestID
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
// clientIP 为本次请求的客户端 IP（按参数传递，不读共享字段——避免并发串扰）；
// PassthroughIP=false 或 clientIP 为空时不注入 IP 头。
// meta 为会话头族元数据（纯新增，不改既有头），见 injectConversationHeaders。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	c.CommonHeaders(req, a)
	// Accept 保持 application/json：官方 applyGatewayHeaders 就是这一个值，
	// 不用 Accept 协商 SSE（上游按请求体的 stream=true 决定回什么）。
	// 此前发 "application/json, text/event-stream" 属于多出来的形态差异。
	req.Header.Set("Accept", "application/json")
	// 剥离 REST 通用层头：官方桌面端 chat 的构造（applyGatewayHeaders +
	// buildVlmGatewayHeaders）里**没有** Origin/Referer/Accept-Language/
	// X-Machine-ID/X-Session-ID——它们由 CommonHeaders 为 billing/checkin 等
	// REST 接口准备，chat 不该继承。
	//
	// 注意：dddmiku 2026-10-17 的 12 组对照实验证明这些头**不参与**渠道校验
	// （带齐/去掉都是 PASS），所以剥离不是为了"过校验"，而是减少形态差异、
	// 降低被跨账号关联的暴露面。剥离不影响功能。
	for _, h := range []string{"Origin", "Referer", "Accept-Language", "X-Machine-ID", "X-Session-ID"} {
		req.Header.Del(h)
	}
	// AccessToken 加锁快照：keepalive 定时刷新会在 a.mu 内改写它，锁外直读构成数据竞争
	// （见 auth.AccessTokenValue 注释）。
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	// 无 UID 时不发明 X-No-User-Id：官方客户端个人号两个方向都不发 X-No-* 哨兵，
	// 补一个"我没有 X"的空值反而是伪造指纹（Misaka10091 spec 的实证口径）。
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	// 企业与域头按 realm 分发：CN 走既有分支（EnterpriseID/Domain 原样透传，缺省 X-No-*）；
	// global 账号由 injectGlobalChatHeaders 统一覆写为国际客户端形态
	// （X-No-Enterprise-Id=1 声明无企业 + X-Domain=www.workbuddy.ai 声明国际版域），
	// 且不回退 X-Domain 到登录会话原值——对齐 intl 项目出站头。
	if a != nil && !a.IsGlobal() {
		// 企业号才注入身份头；个人号零注入（同上方 X-No-* 口径）。
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		}
		if d := a.DomainValue(); d != "" {
			req.Header.Set("X-Domain", d)
		}
	} else {
		c.injectGlobalChatHeaders(req, a)
	}
	// 用量归属头：默认伪造 WorkBuddy 桌面端指纹（client_name="SaaS" 还原旧行为）。
	c.injectAttribution(req)
	// 客户端 IP 透传（仅 PassthroughIP=true 且本次请求带 IP）。
	c.injectClientIP(req, clientIP)
	// 设备风控头：auth 每号 > config 全局 > 文件兜底；空则不注入。
	c.injectDeviceToken(req, a)
	// SDK 痕迹头（x-stainless-*）：官方模型客户端是 stainless 生成的 JS SDK，每次
	// chat 都带。见 injectSDKHeaders 注释。
	c.injectSDKHeaders(req)
	// 会话头族（对话/请求/消息/链路族/baggage），见 injectConversationHeaders。
	c.injectConversationHeaders(req, meta)
	// Connection: keep-alive —— 官方客户端（Node）对每条 chat 请求都显式发这个头，
	// 而 Go 的 Transport 对 HTTP/1.1 认为是默认语义、不落字面。抓包逐项对拍时它是
	// 唯一「官方有、我们没有」的头。显式写入不影响 Go 自己的连接复用决策。
	setExact(req.Header, "Connection", "keep-alive")
}

// injectGlobalChatHeaders global 账号（无企业 ID）的 chat 专属声明头，对齐 intl 项目
// （ANALYSIS-global-chat-solutions.md）：
//   - X-No-Enterprise-Id: 1  个人账号无企业 ID，显式声明（避免上游按缺省/可疑判定）
//   - X-Domain: www.workbuddy.ai  显式声明国际版域（与 Origin/Referer 同域）
//
// 仅 global realm 注入；CN 账号走既有 X-No-Department-Info 等分支，零回归。
func (c *Client) injectGlobalChatHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || !a.IsGlobal() {
		return
	}
	req.Header.Set("X-No-Enterprise-Id", "1")
	req.Header.Set("X-Domain", "www.workbuddy.ai")
}

// 官方模型客户端是 stainless 生成的 JavaScript SDK（包版本 6.25.0），每次 chat 都带
// 一组 x-stainless-* 头描述「客户端运行环境」。2026-10-28 抓官方 CLI 2.165.0 实测：
//
//	arch=x64  lang=js  os=Windows  package-version=6.25.0
//	retry-count=0  runtime=node  runtime-version=v22.22.2
//
// 这些值描述的是**客户端**而不是我们的服务器：我们的出站身份就是「Windows 上的
// WorkBuddy 桌面端」，所以与 UA 自洽。缺了它们，UA 自称桌面包却无任何 SDK 痕迹，
// 是可判别的不一致。
//
// 版本相关：package-version 随 CLI 捆绑的 SDK 走，CLI 版本升级时要一并对齐。
const (
	sdkArch           = "x64"
	sdkOS             = "Windows"
	sdkPackageVersion = "6.25.0"
	sdkRuntimeVersion = "v22.22.2"
)

func (c *Client) injectSDKHeaders(req *http.Request) {
	h := req.Header
	setExact(h, "x-stainless-arch", sdkArch)
	setExact(h, "x-stainless-lang", "js")
	setExact(h, "x-stainless-os", sdkOS)
	setExact(h, "x-stainless-package-version", sdkPackageVersion)
	// retry-count：官方每次 HTTP 尝试从 0 起算。网关侧的换号是一组独立的出站请求，
	// 每条都是「第一次尝试」，所以恒 0。
	setExact(h, "x-stainless-retry-count", "0")
	setExact(h, "x-stainless-runtime", "node")
	setExact(h, "x-stainless-runtime-version", sdkRuntimeVersion)
}

// injectConversationHeaders 注入官方客户端会话头族（issue #35 后台聚合）。
// 头族分四层，各司其职：
//   - X-Conversation-ID：会话级，多轮稳定（body 的 conversationId）。空则不发——
//     透传客户端原值优先，客户端没给就不伪造，避免误导后台建错会话。
//   - X-Conversation-Request-ID：**对话轮级聚合主键**，必发。一次 user send 内的
//     所有 tool call/重试/换号/降级复用同一个 → 后台按它聚合成一条（不再碎片化）。
//   - X-Conversation-Message-ID = X-Request-ID：消息级，每条独立，**两者同值**，
//     形态 32 位小写 hex（CLI 内核 `randomUUID().replace(/-/g,"")`）。
//   - X-Root-Request-ID：= conversationRequestID（根请求追踪）。
//   - 链路族：traceparent / b3 / X-Trace-ID / X-B3-TraceId / X-B3-SpanId /
//     X-B3-ParentSpanId / X-B3-Sampled 共用**一个自洽的 OTEL trace**（trace id 随会话
//     稳定、span id 每请求新、parent span 是会话根 span），详见 traceID32 注释。
//   - baggage：`codebuddy.session_id=<会话 id>,codebuddy.conversation_request_id=<轮级 id>`。
//
// traceID32 由会话 id 派生 32 位小写 hex 的 trace id。
//
// 官方发的是**一个自洽的 OTEL trace**：traceparent / b3 / X-Trace-ID / X-B3-TraceId
// 共用同一个 trace id，span id 每请求新，parent span 是会话根 span。
// 2026-10-28 用 CODEBUDDY_BASE_URL 把官方 CLI 2.165.0 指到本地假上游抓包实测：
// 同一进程内两条 chat 的 trace id 相同（677de9f8…）、span id 不同、parent span 相同
// （e7f652b6…）——即 trace 随会话稳定。这里按 conversationID 确定性派生，
// 得到同样的「会话内稳定、会话间互异」。
func traceID32(seed string) string {
	sum := sha256.Sum256([]byte("wb2a:trace:" + seed))
	return hex.EncodeToString(sum[:16])
}

// parentSpanOf 由会话 id 派生 16 位 hex 的会话根 span id（会话内稳定）。
func parentSpanOf(seed string) string {
	sum := sha256.Sum256([]byte("wb2a:pspan:" + seed))
	return hex.EncodeToString(sum[:8])
}

// uuidForm 把 32 位 hex 排成 8-4-4-4-12 的 UUID 形态。
//
// 官方 X-Conversation-ID 是带连字符的 UUID（抓包：01a18013-46ce-7483-bddd-2e90d2c24a59）；
// 我们的内部派生键是 `d-<32hex>`（34 字符，不是 UUID），直接发出去形态就不一致。
// 只在发头时排一次，内部键不动（会话绑定仍按原键）。
func uuidForm(h32 string) string {
	if len(h32) != 32 {
		return h32
	}
	return h32[0:8] + "-" + h32[8:12] + "-" + h32[12:16] + "-" + h32[16:20] + "-" + h32[20:32]
}

// conversationHeaderID 发头用的会话 id：内部派生键 d-<32hex> 转 UUID 形态；
// 客户端透传的原值原样发出（不改别人给的 id）。
func conversationHeaderID(id string) string {
	if strings.HasPrefix(id, "d-") {
		return uuidForm(id[2:])
	}
	return id
}

func (c *Client) injectConversationHeaders(req *http.Request, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		// 零值 meta（直接调 ChatHeaders 的调用方/测试）也要保证聚合主键必发：
		// 本级补一个 32 hex，调用方（handler）已生成稳定的不走到这里。
		convReqID = session.NewMessageID()
	}
	// 消息级 ID：32 位小写 hex（session.NewMessageID）。
	//
	// 形态与关系都按桌面端内置 CLI 内核（`@genie/agent-cli` 的
	// codebuddy-lite-wb.mjs）回正：
	//
	//	let q = W?.messageId ?? randomUUID().replace(/-/g,"")   // 32hex，去掉连字符
	//	N[CONVERSATION_MESSAGE_ID_HEADER] = $.messageId
	//	N[REQUEST_ID_HEADER]              = $.messageId          // 同一个值
	//	… setHeaderCaseInsensitive(REQUEST_ID_HEADER, q)
	//	… setHeaderCaseInsensitive(CONVERSATION_MESSAGE_ID_HEADER, q)
	//
	// 即：两个头**同值**，且是 32 hex。v1.16.0 曾按 app.asar 的
	// applyGatewayHeaders（ACP 元数据层）改成两个独立 UUID v4，那是错层——
	// chat 请求由 CLI 内核发出，不经那条路径。
	messageID := session.NewMessageID()
	// 会话头三元组必须齐全：国际端点收紧通道校验后，缺 X-Conversation-ID 的
	// 请求会直接 400 / 11128（conversation id missing）。入站没带会话 ID 时
	// 本级补一个，不再因为「透传优先」而漏发该头（issue #190）。
	//
	// 补的形态必须是**带连字符的 UUID**，不是 32 hex：X-Conversation-ID 在官方
	// 客户端里是 `session.id`（UUID 形态，抓包 01a18013-46ce-7483-bddd-2e90d2c24a59），
	// 只有轮级/消息级 ID 才是 32 hex。此前这里补的是 NewMessageID()（32 hex），
	// 于是「客户端没给会话 ID」的请求会发一个官方从不出现的形态——身份自检
	// （identity_consistency_test.go）第一次跑就抓到了它。
	conversationID := meta.ConversationID
	if conversationID == "" {
		conversationID = session.NewConversationID()
	}
	convHeaderID := conversationHeaderID(conversationID)
	setExact(req.Header, "X-Conversation-ID", convHeaderID)
	setExact(req.Header, "X-Conversation-Request-ID", convReqID)
	setExact(req.Header, "X-Conversation-Message-ID", messageID)
	setExact(req.Header, "X-Request-ID", messageID)
	// X-Root-Request-ID：CLI 内核在 resolveRootRequestId() 非空时必发，主对话即
	// 「本次 user send 的根请求」= conversationRequestID（抓包实证：用户对话那条带它，
	// 自动生成标题那条不带——与「有根请求才发」的写法一致）。
	setExact(req.Header, "X-Root-Request-ID", convReqID)
	// 链路族：自洽的 OTEL trace（见 traceID32 注释）。
	// 此前直接拿 conversationRequestID 当 trace id、SpanId 取 messageID[:16]、
	// 且没有 traceparent / b3 / ParentSpanId——「有 B3 的分片但没有 trace 上下文」。
	traceID := meta.TraceID
	if len(traceID) != 32 || !validTraceID(traceID) {
		traceID = traceID32(conversationID)
	}
	spanID := session.NewMessageID()[:16] // 每请求新 span（与消息 id 互不关联）
	parentSpanID := parentSpanOf(conversationID)
	setExact(req.Header, "X-Trace-ID", traceID)
	setExact(req.Header, "traceparent", "00-"+traceID+"-"+spanID+"-01")
	setExact(req.Header, "b3", traceID+"-"+spanID+"-1-"+parentSpanID)
	setExact(req.Header, "X-B3-TraceId", traceID)
	setExact(req.Header, "X-B3-ParentSpanId", parentSpanID)
	setExact(req.Header, "X-B3-SpanId", spanID)
	setExact(req.Header, "X-B3-Sampled", "1")
	// baggage：官方把会话 id 与轮级请求 id 作为 W3C baggage 双发（抓包实测
	// `codebuddy.session_id=…,codebuddy.conversation_request_id=…`）。
	setExact(req.Header, "baggage", "codebuddy.session_id="+convHeaderID+
		",codebuddy.conversation_request_id="+convReqID)
}

// validTraceID 判断 B3 TraceId 是否合法：16 或 32 位 hex（大小写均可）。
// 官方客户端生成的 conversationRequestId 是 32 位 hex（UUID 去横线），入站透传值
// 可能是任意形状（含横线/超长/非 hex），直接塞进 B3 头会破坏链路关联（issue #35）。
func validTraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

// attributionClientName 生效的用量归属名：ClientName 非空取之；
// 空默认 "WorkBuddy"（伪造官方桌面端指纹；显式配 "SaaS" 可还原旧行为）。
func (c *Client) attributionClientName() string {
	if c != nil && c.ClientName != "" {
		return c.ClientName
	}
	return "WorkBuddy"
}

// injectAttribution 注入用量归属头（X-Agent-Purpose / X-IDE-* / X-Product）。
// 仅在 chat/completions 路径生效（ChatHeaders 调用）。
//
// 默认（ClientName 空）即对齐官方 WorkBuddy 桌面端指纹：X-Agent-Purpose="conversation"
// + X-IDE-Name/Type/Product="WorkBuddy" + X-IDE-Version=client_version，上游用量归因
// 不再出现 client/agentPurpose 为空的「网关特征」。显式 ClientName="SaaS" 还原旧行为
// （仅 X-Product="SaaS"，不设 X-IDE-*）；配其他值则四头跟随该值。
func (c *Client) injectAttribution(req *http.Request) {
	name := c.attributionClientName()
	if name == "SaaS" {
		req.Header.Set("X-Product", "SaaS")
		return
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	// agent 语义头，取值来自 CLI 内核 constants 与 getCustomHeaders：
	//   - X-Agent-Intent：主对话恒 craft（subagent 才变）
	//   - X-Agent-Type：主对话恒 main
	//   - X-Agent-Runtime：抓包实测官方 CLI 2.165.0 发 **other**（不是常量表里的
	//     local-agent/cloud-agent/agent-agent——那是枚举全集，实际取值由运行时判定，
	//     桌面端内置 CLI 落到 other）。v1.16.1 曾按枚举猜 local-agent，是猜的。
	//   - X-Private-Data：**不是恒 true**。CLI 里是
	//         A ? "false" : "true"
	//     A = resolveModelOptimizationEnabled({productDefaultEnabled:
	//         !productFeature.DefaultModelOptimizationOff})
	//     product.json 的 DefaultModelOptimizationOff = false → 优化开启 → 取 "false"
	//     语义即「数据可用于模型优化」为真。v1.16.0 一律发 true，方向反了。
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Type", "main")
	req.Header.Set("X-Agent-Runtime", "other")
	req.Header.Set("X-Private-Data", "false")
	setExact(req.Header, "X-IDE-Name", name)
	setExact(req.Header, "X-IDE-Type", name)
	setExact(req.Header, "X-IDE-Version", c.clientVersion())
	// X-Product 恒为 "SaaS"：官方桌面端就是这么发的（X-IDE-Name/Type 才是 WorkBuddy，
	// 两者不是同一个语义）。此前这里跟随 name 发 "WorkBuddy"，与同一账号事件上报
	// 通道（desktop.go 全部 SaaS）自相矛盾——同一客户端两条路径报出两个产品标识，
	// 是可判别的内部不一致。
	req.Header.Set("X-Product", "SaaS")
	// x-codebuddy-request 由 CommonHeaders 统一发（req.Header.Set 会把键规范化成
	// `X-Codebuddy-Request`）。这里此前又用 setExact 补了一次小写键，结果是同一个
	// 头以两种大小写各占一个 map 项、**发出去两个** —— 官方只发一个。HTTP 头名
	// 大小写不敏感，服务端读到的形态一样，所以统一走 CommonHeaders 那处即可。
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 参数透传给上游（三等价头）。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段（X-Forwarded-For 首段，回落 X-Real-IP）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

// authServiceUA 桌面端后台进程打包的 axios（1.16.1）在不设 UA 时自带的字符串。
//
// 积分查询（/billing/meter/get-user-resource*）、企业用量、签到状态与签到这几条
// 请求在官方客户端里**不走请求框架**，走的是后台服务 AuthService 自己的 axios
// 实例（authority-snapshot.js 的 `get http`：axios.create，keepAlive、proxy:false）。
// 依据：2026-10-31 读 5.8.3 / 5.9.4 安装包核对（类里搜 buildHeadersWithTuringToken、
// `get http`、buildHeaders）；同类项目真机抓包里这几个请求的 UA 也是这个值。
//
// 此前网关给这些请求发的是单段 `WorkBuddy/<ver>`——那是 banner 接口的显式覆写
// 形态，不是积分/签到的形态。发一个「官方在任何积分请求上都不会出现的 UA」，
// 比「不一致」更糟：它本身就是一个可检索的特征。
const authServiceUA = "axios/1.16.1"

// authServiceHeaders AuthService 那组请求（积分、企业用量、签到）的请求头。
//
// 官方这组头很瘦：Accept、Content-Type、Authorization、X-User-Id、企业号的
// X-Enterprise-Id / X-Tenant-Id、X-Domain、拦截器补的 X-Request-ID，加 axios 自带
// 的 UA 与 Accept-Encoding。**没有**请求框架的 X-CodeBuddy-Request / X-Product /
// X-Requested-With / 完整桌面头族。
// 积分查询另带 Accept-Language、签到另带 X-Device-Token（由调用方按需加）。
func (c *Client) authServiceHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	// 拦截器补的 X-Request-ID：每次请求新生成（官方是 crypto.randomUUID 去连字符）。
	req.Header.Set("X-Request-ID", session.NewMessageID())
	switch {
	case c != nil && c.UserAgent != "":
		// 配置显式覆盖优先（自定义品牌/版本场景）。
		req.Header.Set("User-Agent", c.UserAgent)
	case c == nil || c.attributionClientName() != "SaaS":
		req.Header.Set("User-Agent", authServiceUA)
	}
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	attachAccountUID(req, a)
}

// BillingHeaders billing 接口请求头。
// UA 语义：默认**不设置**（保持现状，Go 客户端自带默认 UA）；仅当显式配置
// c.UserAgent 非空才覆盖——避免默认路径给 billing 引入新的 UA 指纹。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	// AccessToken 加锁快照（同 ChatHeaders：keepalive 可在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	c.injectCodeBuddyRequest(req)
	// Accept-Language 按 realm 切（D5，billing 域未走 CommonHeaders，单独注入）。
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	} else if ua := c.billingUA(); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	// 设备风控头：billing 域（report/travel/balance/checkin）同样注入。
	c.injectDeviceToken(req, a)
	// billing 域不走 CommonHeaders，这里单独挂 UID：连接池按账号分桶对
	// 「账单 / 上报 / 签到」这类短 RPC 同样生效。
	attachAccountUID(req, a)
}

// ReportHeaders `/v2/report` 专属请求头，对齐该通道真实发送方的出站形态：
//
//	POST /v2/report HTTP/1.1
//	Accept: application/json, text/plain, */*
//	Content-Type: application/json;charset=UTF-8
//	X-Request-ID: <32 位小写 hex，每批不同>
//	X-Product: SaaS
//	User-Agent: WorkBuddy/5.11.3
//	Authorization: Bearer <token>
//	X-User-Id: <uid>
//	X-Enterprise-Id / X-Tenant-Id: <企业号才有>
//	X-Domain: www.workbuddy.cn
//	Content-Length: <n>
//	Accept-Encoding: gzip, compress, deflate, br
//	Host: <host>
//	Connection: keep-alive
//
// 与 BillingHeaders 的差异（有意为之，不要合并）：不注入 x-codebuddy-request、
// Accept-Language、X-Device-Token —— 该通道的发送方不发这三个头，它们是该族其它
// 端点（checkin/企业用量）的形态。同理不发 X-Requested-With。
// Accept-Encoding 保持 Go 默认 gzip：手动设置会关闭 Transport 的自动解压，且官方含
// br（Go 标准库无 brotli 解码器），属于已知残留差异。
func (c *Client) ReportHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	// 官方键名是 X-Request-ID（ID 全大写）。Header.Set 会规范化成 X-Request-Id
	// （textproto 规则），线上就与客户端不一致，必须 setExact。
	setExact(req.Header, "X-Request-ID", session.NewMessageID())
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("User-Agent", c.reportUA())
	if a != nil {
		req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
		if a.UID != "" {
			req.Header.Set("X-User-Id", a.UID)
		}
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
			req.Header.Set("X-Tenant-Id", a.EnterpriseID)
		}
		if d := a.DomainValue(); d != "" {
			req.Header.Set("X-Domain", d)
		}
	}
	// 官方显式发 keep-alive；newTransport 禁了 h2，全站 HTTP/1.1，安全。
	req.Header.Set("Connection", "keep-alive")
	attachAccountUID(req, a)
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	// X-Auth-Refresh-Source 对齐官方客户端 refresh 渠道标识 "plugin"（D3）。
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}
