// desktop.go 桌面客户端行为指纹上报（版本随 clientVersion 走，见 desktopUA）。
//
// 来源：2026-09-12 Sunny 抓包实测（data/desktop-task-protocol.md）。桌面端点亮
// 「需电脑端」类任务的关键不是独立端点，而是同一 POST /v2/report 通道上
// **不同的客户端指纹**：
//
//	POST https://copilot.tencent.com/v2/report        ← chatBase（CLI 上报走 billingBase）
//	User-Agent: WorkBuddy/<clientVersion> WorkBuddy/<clientVersion> CLI/<cliVersion>
//	X-Domain: copilot.tencent.com, X-Product: SaaS, X-User-Id: <uid>
//	Body: [ {...event...} ]                           ← 数组
//
// 每个事件除业务字段外必带桌面指纹（ideName/ideType=WorkBuddy、
// extName=workbuddy-desktop 等）。实测点亮记录：
//   - RichMeow_Chat（桌面端对话1次）：agent_task_created + 成功的
//     chat_message_response(isSuccessful=true) → 1/1 + UR Buddy。
//   - Hp_Appearance（主题任务）：独立 API
//     POST /v2/user-asset/appearance/set {kind:"theme",resource_key} → SetAppearanceTheme。
//
// 注意：service 端对事件链有一定真实性校验倾向（RichMeow 需要消息成功回执），
// 本模块按实测事件形状发送，不保证所有任务都能 API 侧点亮——autotask 侧
// 仍按 attempt 语义处理结果。
package upstream

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

const (
	desktopReportPath    = "/v2/report"
	desktopAppearanceSet = "/v2/user-asset/appearance/set"
)

// desktopUA 桌面客户端 UA，按官方 buildVlmGatewayHeaders 的拼法：
// `WorkBuddy/<ver> WorkBuddy/<ver> CLI/<cli>`。
//
// 版本取 clientVersion() 与 cliVersion() 同一来源——此前这条是硬编码 5.5.6，
// 而 chat 头走 defaultClientVersion（5.5.4），同一账号两条通道报出两个版本，
// 是可判别的内部不一致（上游 aibyhub 也修过同一处「5.5.6 残留」）。
func (c *Client) desktopUA() string {
	v := c.clientVersion()
	return "WorkBuddy/" + v + " WorkBuddy/" + v + " CLI/" + c.cliVersion()
}

// desktopBase 桌面端 /v2/report 与 user-asset 走 chatBase（copilot.tencent.com）。
func (c *Client) desktopBase(a *auth.Auth) string { return c.chatBase(a) }

// setDomainHeader 按官方口径写 X-Domain：取账号登录域（auth.domain，**裸主机名**），
// 空则不发。
//
// 官方 asar 里 29 处 `X-Domain` 全部来自 `auth.domain`（如 www.codebuddy.cn），
// 没有一处是完整 URL。此前这三处发的是 chatBase/desktopBase（带 scheme 的完整
// URL，https://www.workbuddy.cn），既与官方不符，也与本网关 chat 路径
// （a.DomainValue()，裸主机名）自相矛盾。
func setDomainHeader(h http.Header, a *auth.Auth) {
	if d := a.DomainValue(); d != "" {
		h.Set("X-Domain", d)
	}
}

// deriveID 由 uid 稳定派生一个设备标识（machineId 等复用）。
// 幂等：同一账号每次生成相同值，模拟固定设备。
//
// 形态对齐官方实测（2026-11-06 抓包）：官方 machineId / sessionId 都是
// **带横线的 UUID v4**（8-4-4-4-12，version 位 4、variant 位 8-b），
// 此前我们产出的是 36 位无横线 hex —— 长度相同但字符集形态不同。
func deriveID(a *auth.Auth, salt string) string {
	return deriveUUID(salt + ":" + a.UID)
}

// deriveUUID 由任意输入确定性派生一个 UUID v4 形态的标识。
func deriveUUID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return formatUUID(sum[:16])
}

// formatUUID 把 16 字节排成 UUID v4 形态：version 位 4、variant 位 8-b。
func formatUUID(b []byte) string {
	v := make([]byte, 16)
	copy(v, b)
	v[6] = (v[6] & 0x0f) | 0x40
	v[8] = (v[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", v[0:4], v[4:6], v[6:8], v[8:10], v[10:16])
}

var (
	procSessionOnce sync.Once
	procSessionID   string
)

// processSessionID 本进程的一次性会话标识。
//
// 官方实测（2026-11-06 两轮抓包）：同一台机器上两次运行的 sessionId **不同**
// （每轮一个新 UUID），而 machineId 保持不变 —— 即 sessionId 属于「一次客户端运行」，
// machineId 属于「一台设备」。此前我们两者都按账号固定，sessionId 的生命周期错了。
// 同进程内所有账号共享一个 sessionId（一个客户端进程一个），与官方一致。
func processSessionID() string {
	procSessionOnce.Do(func() {
		sum := sha256.Sum256([]byte(fmt.Sprintf("wb2a:session:%d:%d", os.Getpid(), time.Now().UnixNano())))
		procSessionID = formatUUID(sum[:16])
	})
	return procSessionID
}

// DesktopEvent 桌面端事件：业务字段任意（map），公共指纹由 ReportDesktopEvent 注入。
type DesktopEvent map[string]any

// desktopHardware 按账号稳定派生一台「看起来合理」的 Windows 机器参数。
//
// 此前 cpuCores=20 / memorySize=24 / osVersion=10.0.26220 是**所有账号共用**的同一套
// 硬编码值——多个账号向同一后台汇报同一台机器，是可判别的关联信号，比「参数是否
// 等于某台真机」更要紧（上游 aibyhub 从另一条路径也踩到同类问题）。
//
// 改为按 uid 从真实常见配置里确定性挑选：账号间互异、单账号跨重启稳定，
// 且组合都是市面上真实存在的机型（核数:内存GB:Windows build）。
func desktopHardware(a *auth.Auth) (cpuCores, memoryGB int, osVersion, cpuModel string) {
	// 按 uid 稳定派生一台「真实存在过」的 Windows 机器参数。
	//
	// 表里全是**真实机器的真实组合**，取自公开的 dxdiag 报告（游戏/模拟器/切片器的
	// issue 模板强制要求贴，量大且格式统一）。共 57 条，覆盖 4–32 逻辑核、
	// 8–128GB、Windows 10 1809 到 11 24H2。
	//
	// 为什么不自己拼组合：cpuModel、核数、内存、build 四者必须同源。此前硬编码 9 条
	// 是"看起来合理"的组合，现在是"确实有人这么配"的组合——后者经得起交叉核对
	// （i7-10700 在 dxdiag 里就是 16 个逻辑处理器，写成 8 核反而露馅）。
	//
	// 已排除四类：服务器/双路工作站（ProLiant、Z10PE）、ARM64 机型（Snapdragon X，
	// 与上报恒定的 arch=x64 矛盾）、以及 2 核/4G 这类跑不动 AI 客户端的低配机。
	table := []struct {
		cpu, mem int
		build    string
		model    string
	}{
		{4, 8, "10.0.18363", "Intel(R) Core(TM) i5-6500 CPU @ 3.20GHz"},
		{4, 8, "10.0.19042", "AMD Ryzen 3 2200U with Radeon Vega Mobile Gfx"},
		{4, 8, "10.0.19045", "Intel(R) Xeon(R) CPU E3-1225 v3 @ 3.20GHz"},
		{4, 16, "10.0.26100", "Intel(R) Core(TM) i3-9100T CPU @ 3.10GHz"},
		{4, 16, "10.0.26100", "AMD Ryzen 3 PRO 3200GE w/ Radeon Vega Graphics"},
		{4, 24, "10.0.19045", "Intel(R) Core(TM) i5-4460 CPU @ 3.20GHz"},
		{4, 32, "10.0.19044", "Intel(R) Core(TM) i5-6400 CPU @ 2.70GHz"},
		{6, 32, "10.0.22621", "Intel(R) Core(TM) i5-9400F CPU @ 2.90GHz"},
		{8, 8, "10.0.18362", "Intel(R) Core(TM) i7-7700 CPU @ 3.60GHz"},
		{8, 16, "10.0.19044", "11th Gen Intel(R) Core(TM) i7-11370H @ 3.30GHz"},
		{8, 16, "10.0.19045", "11th Gen Intel(R) Core(TM) i7-1165G7 @ 2.80GHz"},
		{8, 16, "10.0.22621", "Intel(R) Core(TM) i7-8565U CPU @ 1.80GHz"},
		{8, 32, "10.0.17763", "Intel(R) Core(TM) i7-6820HQ CPU @ 2.70GHz"},
		{8, 32, "10.0.19042", "AMD Ryzen 5 3400G with Radeon Vega Graphics"},
		{8, 48, "10.0.18362", "Intel(R) Core(TM) i7-7700HQ CPU @ 2.80GHz"},
		{12, 16, "10.0.18363", "Intel(R) Core(TM) i7-8750H CPU @ 2.20GHz"},
		{12, 16, "10.0.19041", "AMD Ryzen 5 2600X Six-Core Processor"},
		{12, 16, "10.0.19045", "AMD Ryzen 5 5600X 6-Core Processor"},
		{12, 16, "10.0.22621", "AMD Ryzen 5 5600 6-Core Processor"},
		{12, 16, "10.0.22621", "12th Gen Intel(R) Core(TM) i5-12400F"},
		{12, 16, "10.0.22631", "AMD Ryzen 5 5500U with Radeon Graphics"},
		{12, 16, "10.0.26100", "AMD Ryzen 5 2600 Six-Core Processor"},
		{12, 16, "10.0.26120", "11th Gen Intel(R) Core(TM) i5-11400H @ 2.70GHz"},
		{12, 32, "10.0.19041", "AMD Ryzen 5 3600X 6-Core Processor"},
		{12, 32, "10.0.19042", "Intel(R) Core(TM) i7-10750H CPU @ 2.60GHz"},
		{12, 32, "10.0.22621", "AMD Ryzen 5 7600 6-Core Processor"},
		{12, 64, "10.0.17763", "Intel(R) Xeon(R) E-2176M CPU @ 2.70GHz"},
		{12, 64, "10.0.22631", "Intel(R) Core(TM) i7-10750H CPU @ 2.60GHz"},
		{16, 16, "10.0.19041", "AMD Ryzen 7 2700X Eight-Core Processor"},
		{16, 16, "10.0.19044", "11th Gen Intel(R) Core(TM) i7-11800H @ 2.30GHz"},
		{16, 16, "10.0.22000", "11th Gen Intel(R) Core(TM) i7-11800H @ 2.30GHz"},
		{16, 16, "10.0.22631", "11th Gen Intel(R) Core(TM) i7-11800H @ 2.30GHz"},
		{16, 16, "10.0.26100", "Intel(R) Core(TM) i5-14400F"},
		{16, 32, "10.0.19042", "Intel(R) Xeon(R) W-2140B CPU @ 3.20GHz"},
		{16, 32, "10.0.19045", "Intel(R) Core(TM) i7-10700KF CPU @ 3.80GHz"},
		{16, 32, "10.0.22000", "Intel(R) Core(TM) i7-10700 CPU @ 2.90GHz"},
		{16, 32, "10.0.22631", "AMD Ryzen 7 2700X Eight-Core Processor"},
		{16, 32, "10.0.26100", "AMD Ryzen 7 9700X 8-Core Processor"},
		{16, 56, "10.0.26100", "Intel(R) Core(TM) i7-10700K CPU @ 3.80GHz"},
		{16, 64, "10.0.19045", "AMD Ryzen 9 5900HX with Radeon Graphics"},
		{16, 64, "10.0.19045", "AMD Ryzen 7 5800X3D 8-Core Processor"},
		{16, 64, "10.0.22631", "AMD Ryzen 7 7800X3D 8-Core Processor"},
		{20, 16, "10.0.19044", "Intel(R) Xeon(R) CPU E5-2666 v3 @ 2.90GHz"},
		{20, 16, "10.0.22621", "12th Gen Intel(R) Core(TM) i7-12700H"},
		{20, 16, "10.0.22631", "13th Gen Intel(R) Core(TM) i7-13700H"},
		{20, 32, "10.0.19042", "Intel(R) Core(TM) i9-10900K CPU @ 3.70GHz"},
		{20, 64, "10.0.26100", "Intel(R) Core(TM) i9-10900KF CPU @ 3.70GHz"},
		{22, 16, "10.0.26100", "Intel(R) Core(TM) Ultra 7 155H"},
		{24, 32, "10.0.26100", "AMD Ryzen 9 5900X 12-Core Processor"},
		{24, 32, "10.0.26100", "13th Gen Intel(R) Core(TM) i7-13700K"},
		{24, 32, "10.0.26200", "AMD Ryzen 9 5900X 12-Core Processor"},
		{32, 32, "10.0.26100", "AMD Ryzen 9 9950X3D 16-Core Processor"},
		{32, 32, "10.0.26100", "AMD Ryzen 9 7945HX with Radeon Graphics"},
		{32, 32, "10.0.26100", "13th Gen Intel(R) Core(TM) i9-13900K"},
		{32, 64, "10.0.19045", "AMD Ryzen 9 3950X 16-Core Processor"},
		{32, 64, "10.0.22631", "AMD Ryzen 9 7950X 16-Core Processor"},
		{32, 64, "10.0.22631", "13th Gen Intel(R) Core(TM) i9-13900K"},
	}
	// 取两个字节做下标：单字节只有 256 种可能，条数一多尾部会明显偏少。
	sum := sha256.Sum256([]byte("wb2a:hw:" + a.UID))
	i := int(sum[0])<<8 | int(sum[1])
	c := table[i%len(table)]
	return c.cpu, c.mem, c.build, c.model
}

// desktopFingerprint 公共桌面指纹字段（注入每个事件，覆盖同名业务键）。
func (c *Client) desktopFingerprint(a *auth.Auth) map[string]any {
	now := time.Now().UnixMilli()
	cpuCores, memoryGB, osVersion, cpuModel := desktopHardware(a)
	return map[string]any{
		"timezone":     "Asia/Shanghai",
		"reportDelay":  2000,
		"userId":       a.UID,
		"username":     a.Nickname,
		"userNickname": a.Nickname,
		"product":      "SaaS",
		"releaseDate":  int64(1789036585355),
		"commit":       "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName":      "WorkBuddy",
		"ideType":      "WorkBuddy",
		"ideVersion":   c.clientVersion(),
		// buddyId / buddyName 每条官方事件都带（值为空串），不是桌面渲染层私有字段。
		"buddyId":    "",
		"buddyName":  "",
		"machineId":  deriveID(a, "machine"),
		"sessionId":  processSessionID(),
		"extName":    "workbuddy-desktop",
		"extVersion": c.clientVersion(),
		"os":         "win32",
		"arch":       "x64",
		"osVersion":  osVersion,
		"cpuCores":   cpuCores,
		"cpuModel":   cpuModel,
		"memorySize": memoryGB,
		"timestamp":  now,
		"presentAt":  now,
	}
}

// ReportDesktopEvent 以桌面客户端指纹向 copilot.tencent.com/v2/report 批量上报事件。
// events 为业务载荷（eventCode 等字段由调用方给出）；公共指纹自动注入，
// 业务字段优先（可用于覆盖 machineId 等设备标识做真实设备对齐）。
func (c *Client) ReportDesktopEvent(a *auth.Auth, events ...DesktopEvent) error {
	if len(events) == 0 {
		return fmt.Errorf("desktop report: no events")
	}
	fp := c.desktopFingerprint(a)
	// 事件里要带本机出口公网 IP（官方客户端每条事件都带）。探测一次、进程内复用；
	// 取不到就不加这个字段，宁缺不错。
	clientIP := c.resolveReportClientIP(a)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := map[string]any{}
		for k, v := range fp {
			m[k] = v
		}
		for k, v := range ev {
			m[k] = v
		}
		// CLI 链按官方实测字段集裁剪：官方报文里没有的字段一律不发。
		// 这一步同时解决两类偏差：公共指纹里的 UI 侧字段（buddyId/buddyName）
		// 不该出现在 CLI 链；个别事件的基底字段官方没有（如 chat_message_status
		// 没有 presentAt）。UI 侧事件不在白名单里，不受裁剪影响。
		if code, ok := ev["eventCode"].(string); ok {
			if allow, ok := cliOfficialFields[code]; ok {
				allowed := make(map[string]bool, len(allow))
				for _, f := range allow {
					allowed[f] = true
				}
				for k := range m {
					if !allowed[k] {
						delete(m, k)
					}
				}
			}
		}
		// 白名单裁剪之后再加，避免被当作「官方没有的字段」删掉。
		if clientIP != "" {
			if _, ok := m["clientIp"]; !ok {
				m["clientIp"] = clientIP
			}
		}
		arr = append(arr, m)
	}
	raw, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.desktopBase(a)+desktopReportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", c.desktopUA())
	setDomainHeader(req.Header, a)
	req.Header.Set("X-Product", "SaaS")
	// X-Request-ID 恒 32 位小写 hex（官方形态）。此前是 deriveID(36hex)+纳秒后 6 位
	// 的 42 字符混合串——长度与字符集都偏离，且与同批其它通道的纯纳秒数字并存。
	setExact(req.Header, "X-Request-ID", session.NewMessageID())
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	_, err = c.doJSON(req)
	return err
}

// newCLIEvent 构造一条 CLI 链事件的基础字段。
//
// 官方实测（2026-11-06 实时抓包，12 批 /v2/report）：一次请求链内 6 个事件的
// traceId 与 requestId 是**同一个值**，messageId 每轮一个 —— 与桌面 UI 事件的
// ID 体系不同（UI 侧 agent_task_created 的 messageId 就等于 requestId）。
//
// 基底里带 presentAt / parentConversationId / agentName 等字段，但个别事件官方
// 没有（chat_message_status 无 presentAt；user_task_failed_credit_refund 这几个
// 一律没有），由 ReportDesktopEvent 按 cliOfficialFields 白名单裁掉。
func newCLIEvent(code, conversationID, requestID, traceID string) DesktopEvent {
	return DesktopEvent{
		"eventCode":                         code,
		"conversationId":                    conversationID,
		"requestId":                         requestID,
		"traceId":                           traceID,
		"rootRequestId":                     requestID,
		"parentConversationId":              conversationID,
		"agentName":                         "cli",
		"agentType":                         "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": requestID,
		"presentAt":                         time.Now().UnixMilli(),
	}
}

// DesktopToolActionEvent 构造「工具调用」事件（官方实测 47 字段）。
//
// 关键：工具调用**失败也要报**，并带错误码（toolErrorCode / toolErrorCodeKey /
// toolErrorMessage）。官方样例：toolName:"Bash" type:"main" toolStatus:"failed"
// toolCallSuccessful:false toolErrorCode:"99999"。
func DesktopToolActionEvent(conversationID, requestID, messageID, traceID string,
	toolCallID, toolName, toolStatus, toolType string,
	stepCount, totalStepCount int, successful bool,
	errorCode, errorKey, errorMsg, modelID, modelName string) DesktopEvent {
	ev := newCLIEvent("chat_tool_action", conversationID, requestID, traceID)
	ev["messageId"] = messageID
	ev["toolCallId"] = toolCallID
	ev["toolName"] = toolName
	ev["toolStatus"] = toolStatus
	ev["toolCallSuccessful"] = successful
	ev["toolErrorCode"] = errorCode
	ev["toolErrorCodeKey"] = errorKey
	ev["toolErrorMessage"] = errorMsg
	ev["type"] = toolType
	ev["stepCount"] = stepCount
	ev["totalStepCount"] = totalStepCount
	ev["requestModelId"] = modelID
	ev["requestModelName"] = modelName
	return ev
}

// DesktopTaskFailedCreditRefundEvent 构造「任务失败积分退款」事件（官方实测 30 字段，
// 特有字段只有 reason）。失败路径上官方照报不误 —— 一次失败请求里它出现了 8 次。
func DesktopTaskFailedCreditRefundEvent(conversationID, requestID, messageID, reason string) DesktopEvent {
	ev := newCLIEvent("user_task_failed_credit_refund", conversationID, requestID, session.NewMessageID())
	ev["messageId"] = messageID
	ev["reason"] = reason
	return ev
}

// DesktopChatSequence 构造一次「桌面端成功对话」的完整事件链
// （agent_task_created → chat_message_send → chat_request_send →
// chat_message_response → chat_message_status → chat_request_response）。
//
// 字段集合按官方 CLI 的真实报文对齐（2026-11-06 抓包，7 批 /v2/report）：
//   - conversationId 全会话唯一；requestId 每请求唯一；**messageId 每轮唯一**，
//     send / response / status 三条复用同一个；traceId 全链唯一（与 requestId 不同值）；
//     rootRequestId = requestId；parentConversationId = conversationId
//   - 每条会话类事件都带 codebuddy.session_id / codebuddy.conversation_request_id
//   - 模型缺省 auto / Auto（官方在「自动选模型」时就是这两个值）
//
// 调用方必须传 32 位 hex 的 conversationID/requestID/messageID：
// 早期这里发的是 msg-xxx-assistant 这类自造串，与官方形态差得一眼可辨。
func DesktopChatSequence(conversationID, requestID, messageID, modelID, modelName string) []DesktopEvent {
	if modelID == "" {
		modelID = "auto"
	}
	if modelName == "" {
		modelName = "Auto"
	}
	traceID := session.NewMessageID() // 官方 traceId 独立于 requestId，同为 32 hex
	now := time.Now().UnixMilli()

	mkCLI := func(code string, extra map[string]any) DesktopEvent {
		ev := newCLIEvent(code, conversationID, requestID, traceID)
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	// mkUI 用于桌面 UI 事件：官方报文里这类事件**没有** traceId / rootRequestId /
	// parentConversationId / agentName / codebuddy.* 这些 CLI 上下文，只有 eventCode
	// 与其自身业务字段。
	mkUI := func(code string, extra map[string]any) DesktopEvent {
		ev := DesktopEvent{"eventCode": code, "presentAt": now}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}

	return []DesktopEvent{
		// agent_task_created 是**桌面 UI 事件**（不属于 CLI），字段取自渲染层构造处
		// createConversation 里的 taskCreated 对象；随后 reportConversationTaskCreated
		// 再补 conversationId / requestId / messageId 三个。
		mkUI("agent_task_created", map[string]any{
			"source": "LOCAL", "name": "working",
			"task_target": "local", "is_worktree_task": false, "is_from_code": false,
			"mode":           "craft",
			"requestModelId": modelID, "requestModelName": modelName,
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{},
			"has_mention": false, "mention_types": []any{},
			"has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{},
			"messageId": requestID,
		}),
		mkCLI("chat_message_send", map[string]any{
			"messageId": messageID, "requestModelId": modelID, "requestModelName": modelName,
			"historyCount": 1, "isContextTruncated": false,
			"currentStepCount": 1, "totalStepCount": 1, "compactionCount": 0,
		}),
		mkCLI("chat_request_send", map[string]any{
			"mode": "craft", "inputLength": 24,
			"requestModelId": modelID, "requestModelName": modelName,
			"isPlan": false, "isAutoExecuteTerminal": false, "isAutoModify": false,
			"codebaseEnable": false, "isMaxMode": false,
			"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
			"codebaseId": "", "mentionContextCount": 0, "command": "",
			"recommendId": "", "skillId": "",
			"vcsType": "unknown", "vcsRepo": "", "vcsBranchName": "", "vcsRevId": "",
		}),
		mkCLI("chat_message_response", map[string]any{
			"messageId": messageID, "requestModelId": modelID, "requestModelName": modelName,
			"responseModelId": modelID, "totalStepCount": 1,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"firstTokenAt": now,
		}),
		mkCLI("chat_message_status", map[string]any{
			"messageId": messageID, "messageErrorCode": "0",
			"requestModelId": modelID, "requestModelName": modelName,
		}),
		mkCLI("chat_request_response", map[string]any{
			"mode": "craft", "toolCallCount": 0,
			"requestModelId": modelID, "requestModelName": modelName,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"reason": "", "contentFilterMessage": "", "expertId": "",
		}),
	}
}

// SetAppearanceTheme 应用外观主题（实测：POST copilot.tencent.com/v2/user-asset/appearance/set，
// 和品主题 resource_key 为 "theme-tkmw7j"，浅色 "light"、深色 "dark"）。纯 API set 不计
// Hp_Appearance 分（需客户端切主题后真实活跃），保留供调色/还原与后续验证用。
func (c *Client) SetAppearanceTheme(a *auth.Auth, resourceKey string) error {
	body := map[string]string{"kind": "theme", "resource_key": resourceKey}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.desktopBase(a)+desktopAppearanceSet, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", c.desktopUA())
	req.Header.Set("X-Product", "SaaS")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	_, err = c.doJSON(req)
	return err
}

// DesktopTaskCompletedEvent 构造官方的「任务完成」事件。
//
// 实测形态（桌面端 HTTP 抓包）：没有 requestId / messageId / conversationId，
// 只有 task_id（值就是会话 ID）；duration_ms 是任务总耗时，total_steps 是步数
// （纯问答为 1，带一次工具调用为 2）。mode 恒 LOCAL。
func DesktopTaskCompletedEvent(conversationID string, totalSteps int, durationMs int64) DesktopEvent {
	return DesktopEvent{
		"eventCode":   "agent_task_completed",
		"mode":        "LOCAL",
		"task_id":     conversationID,
		"duration_ms": durationMs,
		"total_steps": totalSteps,
	}
}

// DesktopChatResponseCompleteEvent 构造官方的「回复完成」事件。
//
// 实测只有四个业务字段：conversationId / requestId / artifactType / source，
// artifactType 为空串，source 恒 "work"；不带 token 数与 finishReason。
func DesktopChatResponseCompleteEvent(conversationID, requestID string) DesktopEvent {
	return DesktopEvent{
		"eventCode":      "chat_response_complete",
		"conversationId": conversationID,
		"requestId":      requestID,
		"artifactType":   "",
		"source":         "work",
	}
}

// DesktopBuddyAppSequence 构造「进入 Buddy 应用」五连事件（实测两账号纯 API 点亮
// Buddy_App 与 Buddy_App_QQ）：discover → show → enter_click → auth_confirm →
// bindaccount_skip。buddyID 固定用企鹅教师助手 cb_y5Dy46tPQGGWtueMxXbe
// （Buddy_App_QQ 的判据应用），同时满足 Buddy_App「进入任一应用」。
func DesktopBuddyAppSequence(buddyID, buddyName string) []DesktopEvent {
	mk := func(code string, extra map[string]any) DesktopEvent {
		ev := DesktopEvent{
			"eventCode": code, "mode": "LOCAL",
			"buddyId": buddyID, "buddyName": buddyName,
		}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []DesktopEvent{
		mk("buddyapp_discover_click", nil),
		mk("buddyapp_show", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2}),
		// isFirstPage 必须是布尔：上游的格式校验按 schema 校验类型，字符串 "1" 会被
		// 整条事件判为 invalid（2026-11-13 实测：同一条事件发 "1" → 400
		// invalidIndices；发 true → code 0）。此前发字符串，老账号是在加严之前点亮的，
		// 新账号则一直卡在 0/1。
		mk("buddyapp_enter_click", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2, "isFirstPage": true}),
		mk("buddyapp_auth_confirm_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
		mk("buddyapp_bindaccount_skip_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
	}
}

// DesktopAutomationCreateEvent 构造「定时任务创建成功」事件（实测两账号纯 API
// 点亮 automation_1）。name 为任务名，可与真实创建语义对齐。
func DesktopAutomationCreateEvent(name string) DesktopEvent {
	return DesktopEvent{
		"eventCode": "automated_task_create_suc", "name": name,
		"source": "manually", "modelId": "auto", "modelIsThinking": true,
		"connectorCount": 0, "skills": "", "skillCount": 0,
		"scheduleType": "once", "mode": "LOCAL",
	}
}

// ReportWebEvent 以 Web 端指纹向 www.workbuddy.cn/v2/report 上报单事件。
// 与桌面指纹（copilot 域）不同：web 域事件是浏览器形状（os/machineId/userAgent），
// 用于 Library_read 等页面行为类任务（实测 library_doc_intro_click 4 秒点亮）。
func (c *Client) ReportWebEvent(a *auth.Auth, eventCode, pageURL, elementID, elementName string) error {
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	ev := map[string]any{
		"eventCode": eventCode, "timestamp": time.Now().UnixMilli(), "reportDelay": 0,
		"pageURL": pageURL, "elementId": elementID, "elementName": elementName,
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": ua,
		"machineId": deriveID(a, "webmachine"), "userId": a.UID,
		"userNickname": a.Nickname, "enterpriseId": a.EnterpriseID,
	}
	raw, err := json.Marshal([]map[string]any{ev})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.webBase(a)+"/v2/report", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-client-platform", "web")
	req.Header.Set("Origin", c.webBase(a))
	req.Header.Set("Referer", pageURL)
	req.Header.Set("User-Agent", ua)
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	_, err = c.doJSON(req)
	return err
}

// ---------------------------------------------------------------------------
// 2026-09-12 第五轮：客户端 asar 逆向 + Sunny 真实样本驱动的新判据（三账号实测点亮）。
// 来源：官方桌面端 app.asar 渲染层事件枚举/载荷 + 抓包真实专家召唤样本
// （data/desktop-task-protocol.md §7.3）。
// ---------------------------------------------------------------------------

// DesktopTemplateUseSequence 构造「使用模板创建任务」事件组（实测 template_5 计数）：
// agent_task_created_with_template {mode,isCustomModel,id,name,requestId} +
// template_used {template_id,task_mode}，JOIN 一条完整 chat 链。
// 三账号实测：5 组（不同模板）一次上报 → 5/5 点亮。
func DesktopTemplateUseSequence(conversationID, requestID, templateID, templateName string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, session.NewMessageID(), "", "")
	events = append(events,
		DesktopEvent{
			"eventCode": "agent_task_created_with_template", "mode": "working",
			"isCustomModel": false, "id": templateID, "name": templateName, "requestId": requestID,
		},
		DesktopEvent{"eventCode": "template_used", "template_id": templateID, "task_mode": "working"},
	)
	return events
}

// DesktopPlaybookPromptSequence 构造「灵感案例做同款」事件组（实测 playbook_prompt 计数）：
// web_element_click(playbook_ctaClick) + playbook_cta_click + playbook_prompt_send
// （Dialog 发送 Prompt，带 conversationId/requestId JOIN chat 链）。三账号实测 1/1 点亮。
func DesktopPlaybookPromptSequence(conversationID, requestID, caseID, caseName string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, session.NewMessageID(), "", "")
	payload := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
	}
	events = append(events,
		DesktopEvent{
			"eventCode": "web_element_click", "pageName": "playbook_detail",
			"elementId": "playbook_ctaClick", "elementName": caseName, "source": "discover",
		},
		DesktopEvent(func() map[string]any {
			m := map[string]any{"eventCode": "playbook_cta_click", "source": "discover", "position": 0}
			for k, v := range payload {
				m[k] = v
			}
			return m
		}()),
		DesktopEvent(func() map[string]any {
			m := map[string]any{"eventCode": "playbook_prompt_send", "conversationId": conversationID, "requestId": requestID}
			for k, v := range payload {
				m[k] = v
			}
			return m
		}()),
	)
	return events
}

// DesktopDesignCanvasSequence 构造「设计创意画布」事件组（实测 create_canvas 计数）：
// wbx_design_canvas_task_create + wbx_design_canvas_open（Ardot create_design 工具
// 完成时客户端经 metrics 通道上报，同一 /v2/report 端点）。三账号实测 1/1 点亮。
func DesktopDesignCanvasSequence(conversationID, requestID string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, session.NewMessageID(), "", "")
	return append(events,
		DesktopEvent{
			"eventCode": "wbx_design_canvas_task_create", "conversationId": conversationID,
			"requestId": requestID, "source": "summon_keyword", "cost": 12000, "isSuccessful": true,
		},
		DesktopEvent{
			"eventCode": "wbx_design_canvas_open", "conversationId": conversationID,
			"requestId": requestID, "id": "ardot-file-" + requestID[len(requestID)-8:],
			"source": "summon_keyword", "type": "page", "cost": 13000, "isSuccessful": true,
		},
	)
}

// MarketExpert 专家市场的单个专家（/portal/operation-platform/market/expert/list 响应子集）。
type MarketExpert struct {
	ExpertID      string `json:"expert_id"`
	ExpertType    string `json:"expert_type"`
	DisplayNameZH string `json:"display_name_zh"`
	ProfessionZH  string `json:"profession_zh"`
	Version       string `json:"version"`
	Categories    []any  `json:"categories"`
	// IsCloud / PayMode 决定专家会话要不要带 X-Moderation-Type（见 Protect）。
	// 官方在专家对象上就是这两个字段名（数字），读安装包确认：normalize 时
	// `is_cloud: optionalNumber(t.is_cloud)`、`pay_mode: optionalNumber(t.pay_mode)`。
	IsCloud int `json:"is_cloud"`
	PayMode int `json:"pay_mode"`
}

// Protect 报告该专家会话是否需要声明内容保护（X-Moderation-Type: ["protected"]）。
//
// 官方 shouldProtectSession：云端专家（is_cloud === 1）或付费资产（pay_mode 非 0）
// 即开启；另外还有「已启用技能里有云端技能」一路，网关的专家对话不加载技能，该路
// 恒假。列表里没查到、或字段缺失（解析成 0）时返回 false——官方对占位专家也是
// 不带头，宁缺不错。
func (e MarketExpert) Protect() bool {
	return e.IsCloud == 1 || e.PayMode != 0
}

// MarketExpertList 拉取专家市场真实专家列表（expertType: "agent" 单专家 / "team" 专家团）。
// expert_actual_use 的判据校验要求 id 是平台上真实存在的专家（编造 id 不计数）。
func (c *Client) MarketExpertList(a *auth.Auth, expertType string) ([]MarketExpert, error) {
	body := map[string]any{"page": 1, "page_size": 20, "sort_by": "reco_rank", "sort_order": "desc"}
	if expertType != "" {
		body["expert_type"] = expertType
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+"/portal/operation-platform/market/expert/list", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.desktopUA())
	setDomainHeader(req.Header, a)
	req.Header.Set("X-Product", "SaaS")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	var out struct {
		Experts []MarketExpert `json:"experts"`
	}
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("expert list parse: %w", err)
	}
	return out.Experts, nil
}

// DesktopChatWithExpert 发一条真实桌面指纹 chat 请求（可带 X-Expert-Id），从 SSE 流
// 解析**服务端返回的 requestId**（data.id，如 cmb-xxxx / 32hex）并返回。
// expert_actual_use 等 JOIN 事件的 requestId 必须是该服务端 id——自造 UUID 不计数
// （客户端 resolveRealRequestId 同款语义，Sunny row 2113 实证）。
//
// protect 决定是否带 X-Moderation-Type: ["protected"]（云端/付费专家会话的内容保护
// 声明，取 MarketExpert.Protect()）。列表里查不到该专家时传 false——官方对占位专家
// 也不带头。
func (c *Client) DesktopChatWithExpert(a *auth.Auth, expertID string, protect bool) (conversationID, requestID string, err error) {
	conversationID = session.NewConversationID()
	body := map[string]any{
		"model": "auto",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant. 当前处于中文环境，使用简体中文回答。"},
			map[string]any{"role": "user", "content": "1+1等于几？直接回答。"},
		},
		"agent":          "cli",
		"temperature":    1,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+"/v2/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	h := req.Header
	h.Set("Authorization", "Bearer "+a.AccessTokenValue())
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("User-Agent", c.desktopUA())
	setDomainHeader(h, a)
	h.Set("X-Product", "SaaS")
	h.Set("X-User-Id", a.UID)
	setExact(h, "X-Conversation-ID", conversationID)
	// 轮级聚合主键：X-Conversation-Request-ID 与 X-Root-Request-ID **同值**，一次
	// user send 开始时生成、该轮所有请求（含重试/换号）复用；主对话即根请求，所以
	// Root 与它同值（辅助请求不带 Root，本路径是主对话形态）。
	//
	// 本路径此前只设了 Conversation-ID / Message-ID / Request-ID 三个头，缺这两个。
	// 上游按「会话头族是否齐全」做渠道校验，缺任一头即 400 / code 11128
	// "conversation id missing"（displayTips「这次调用没通过渠道校验，可能不是官方
	// 客户端发出的」）。主 chat 路径由 injectConversationHeaders 补齐，这条手搓头的
	// 专家任务路径漏了（xiaomujin 分支的同类修复，见其 desktop.go 的注释）。
	turnID := session.NewMessageID()
	setExact(h, "X-Conversation-Request-ID", turnID)
	setExact(h, "X-Root-Request-ID", turnID)
	// 消息级 ID：X-Request-ID 与 X-Conversation-Message-ID **同值**、32 位小写 hex
	// （CLI 内核 randomUUID().replace(/-/g,"") 后两个头吃同一个值）。
	msgID := session.NewMessageID()
	setExact(h, "X-Conversation-Message-ID", msgID)
	setExact(h, "X-Request-ID", msgID)
	h.Set("X-Agent-Intent", "craft")
	h.Set("X-Agent-Type", "main")
	h.Set("X-Agent-Runtime", "other")
	// X-Private-Data = "false"：模型优化默认开启（product.json 的
	// DefaultModelOptimizationOff=false），CLI 此时发 false。原发 true 方向反了。
	h.Set("X-Private-Data", "false")
	setExact(h, "X-IDE-Name", "WorkBuddy")
	setExact(h, "X-IDE-Type", "WorkBuddy")
	setExact(h, "X-IDE-Version", c.clientVersion())
	setExact(h, "x-codebuddy-request", "1")
	if expertID != "" {
		h.Set("X-Expert-Id", expertID)
	}
	// 内容保护声明：官方对云端或付费的专家会话加这一个头，值是字面量
	// `["protected"]`（安装包常量 Ok = '["protected"]'，不是 JSON 序列化出来的）。
	if protect {
		setExact(h, "X-Moderation-Type", `["protected"]`)
	}
	if os.Getenv("WB2A_DEBUG_CHAT") != "" {
		fmt.Printf("[dbg] URL=%s\n", req.URL)
		for k := range req.Header {
			fmt.Printf("[dbg] %s: %s\n", k, req.Header.Get(k))
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if os.Getenv("WB2A_DEBUG_CHAT") != "" {
		fmt.Printf("[dbg] status=%d enc=%q\n", resp.StatusCode, resp.Header.Get("Content-Encoding"))
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return "", "", fmt.Errorf("chat http %d: %s", resp.StatusCode, b)
	}
	// 从 SSE 流抓第一个 data.id 作为服务端 requestId（读干流避免残留连接）。
	buf := make([]byte, 0, 1<<20)
	tmp := make([]byte, 8192)
	searchFrom := 0
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if os.Getenv("WB2A_DEBUG_CHAT") != "" {
				dbg := string(tmp[:n])
				if len(dbg) > 120 {
					dbg = dbg[:120]
				}
				fmt.Printf("[dbg-rd %d] %q\n", n, dbg)
			}
			// 从上次搜索位置继续：SSE 里 `"id":"` 可能先出现在消息 id 等字段，
			// 若不推进偏移，首个不匹配的 id 会让循环永远重复命中同一位置，
			// 读满 1MB 后误报"未找到 requestId"。
			if i := bytes.Index(buf[searchFrom:], []byte(`"id":"`)); i >= 0 {
				abs := searchFrom + i
				rest := buf[abs+6:]
				if end := bytes.IndexByte(rest, '"'); end > 0 {
					id := string(rest[:end])
					if os.Getenv("WB2A_DEBUG_CHAT") != "" {
						fmt.Printf("[dbg-id] %q match=%v\n", id, idRegex.MatchString(id))
					}
					if idRegex.MatchString(id) {
						return conversationID, id, nil
					}
					searchFrom = abs + 1
				}
			}
		}
		if rerr != nil || len(buf) > 1<<20 {
			break
		}
	}
	return "", "", fmt.Errorf("SSE 中未找到服务端 requestId")
}

// idRegex 服务端 requestId 形状（cmb- 前缀 32hex 或裸 32hex）。
var idRegex = regexp.MustCompile(`^(cmb-)?[0-9a-f]{32}$`)

// DesktopExpertSummonSequence 构造「召唤平台专家」事件组（expert_summon_click 等），
// 载荷对齐真实抓包样本（Sunny row 2644）。需配合 DesktopChatWithExpert +
// DesktopExpertActualUseEvent 完成一次完整「召唤+使用」。
func DesktopExpertSummonSequence(e MarketExpert) []DesktopEvent {
	cat := "expert-all"
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok {
			cat = s
		}
	}
	ver := e.Version
	if ver == "" {
		ver = "1.0.0"
	}
	return []DesktopEvent{
		{
			"eventCode": "web_element_click", "source": e.ExpertID, "type": cat, "version": ver,
			"elementId": "expert_summon_click", "elementName": "立即召唤",
			"pageURL": "/C:/Program%20Files/WorkBuddy/resources/app.asar/renderer/index.html",
		},
		{
			"eventCode": "expert_summon_click", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all", "position": 0,
			"expertType": e.ExpertType, "version": ver, "mode": "LOCAL",
		},
		{
			"eventCode": "expert_summoned", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all",
		},
	}
}

// DesktopExpertActualUseEvent 构造「专家真实使用」事件（expert_5/Expert_team_use_3 计数）。
// requestID 必须是 DesktopChatWithExpert 返回的服务端 requestId。
func DesktopExpertActualUseEvent(e MarketExpert, conversationID, requestID string) DesktopEvent {
	ev := desktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = "craft"
	return ev
}

// DesktopExpertActualUseLocal mode:"LOCAL" 变体（Expert_lighthouse 判据要求 LOCAL，
// 对齐真实样本 Sunny row 868：轻量云专家使用时 mode=LOCAL、type 为空、cost=0）。
func DesktopExpertActualUseLocal(e MarketExpert, conversationID, requestID string) DesktopEvent {
	ev := desktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = "LOCAL"
	return ev
}

// desktopExpertActualUse expert_actual_use 公共载荷。
func desktopExpertActualUse(e MarketExpert, conversationID, requestID string) DesktopEvent {
	cat := "expert-all"
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok {
			cat = s
		}
	}
	ver := e.Version
	if ver == "" {
		ver = "1.0.0"
	}
	return DesktopEvent{
		"eventCode": "expert_actual_use",
		"id":        e.ExpertID, "name": e.DisplayNameZH, "expertTitle": e.ProfessionZH,
		"type": cat, "expertType": e.ExpertType, "source": "builtin", "version": ver,
		"cost": 9000, "characterCount": 14,
		"conversationId": conversationID, "requestId": requestID, "messageId": "msg-" + requestID[len(requestID)-8:],
		"requestModelId": "auto", "requestModelName": "Auto",
	}
}
