package upstream

// cliOfficialFields 官方 CLI 上报事件（/v2/report）的字段集，6 个事件码。
//
// 用途：注入公共指纹后按它裁剪 —— 官方报文里没有的字段一律不发。
// 为什么用白名单而不是逐条删：UI 侧事件与 CLI 链的公共字段集**不同**
// （例：buddyId/buddyName 只有 UI 事件有；chat_message_status 没有 presentAt），
// 逐条打补丁迟早会再犯「把 UI 侧字段塞进 CLI 链」的错。
//
// 注：设备级标识字段（官方报文里有、但只在该字段自有的部署里注入）
// 不列入本表 —— 公开仓库不出现该字段名，部署侧自行按需注入。
//
// 2026-11-14 补 `clientIp`：该字段由上报服务在批量发送前统一写入每条事件
// （值取自本机出口公网 IP），不是单个事件的私有字段，因此六个事件码一并加入。
// 注入点在发送前、白名单裁剪之后（见 client_ip.go / desktop.go）。
//
// 键 = eventCode，值 = 该事件的完整字段名列表。
var cliOfficialFields = map[string][]string{
	"chat_message_response": {
		"agentName", "agentType", "arch", "cachedMissTokens", "cachedTokens", "cachedWriteTokens",
		"codebuddy.conversation_request_id", "codebuddy.session_id", "commit", "conversationId", "cpuCores", "cpuModel",
		"eventCode", "extName", "extVersion", "finishReason", "firstTokenAt", "ideName",
		"ideType", "ideVersion", "inputToken", "isSuccessful", "machineId", "memorySize",
		"messageErrorCode", "messageId", "os", "osVersion", "outputToken", "parentConversationId",
		"presentAt", "product", "releaseDate", "reportDelay", "requestId", "requestModelId",
		"requestModelName", "responseModelId", "rootRequestId", "sessionId", "timestamp", "timezone",
		"totalStepCount", "totalToken", "traceId", "userId", "userNickname", "username", "clientIp",
	},
	"chat_message_send": {
		"agentName", "agentType", "arch", "codebuddy.conversation_request_id", "codebuddy.session_id", "commit",
		"compactionCount", "conversationId", "cpuCores", "cpuModel", "currentStepCount", "eventCode",
		"extName", "extVersion", "historyCount", "ideName", "ideType", "ideVersion",
		"isContextTruncated", "machineId", "memorySize", "messageId", "os", "osVersion",
		"parentConversationId", "presentAt", "product", "releaseDate", "reportDelay", "requestId",
		"requestModelId", "requestModelName", "rootRequestId", "sessionId", "timestamp", "timezone",
		"totalStepCount", "traceId", "userId", "userNickname", "username", "clientIp",
	},
	"chat_message_status": {
		"agentName", "agentType", "arch", "codebuddy.conversation_request_id", "codebuddy.session_id", "commit",
		"conversationId", "cpuCores", "cpuModel", "eventCode", "extName", "extVersion",
		"ideName", "ideType", "ideVersion", "machineId", "memorySize", "messageErrorCode",
		"messageId", "os", "osVersion", "parentConversationId", "product", "releaseDate",
		"reportDelay", "requestId", "requestModelId", "requestModelName", "rootRequestId", "sessionId",
		"timestamp", "timezone", "traceId", "userId", "userNickname", "username", "clientIp",
	},
	"chat_request_send": {
		"agentName", "agentType", "arch", "codebaseEnable", "codebaseId", "codebuddy.conversation_request_id",
		"codebuddy.session_id", "command", "commit", "conversationId", "cpuCores", "cpuModel",
		"eventCode", "extName", "extVersion", "ideName", "ideType", "ideVersion",
		"inputLength", "isAutoExecuteTerminal", "isAutoModify", "isMaxMode", "isPlan", "knowledgeId",
		"knowledgeName", "machineId", "memorySize", "mentionContextCount", "mentionContexts", "mode",
		"os", "osVersion", "parentConversationId", "presentAt", "product", "recommendId",
		"releaseDate", "reportDelay", "requestId", "requestModelId", "requestModelName", "rootRequestId",
		"sessionId", "skillId", "timestamp", "timezone", "traceId", "userId",
		"userNickname", "username", "vcsBranchName", "vcsRepo", "vcsRevId", "vcsType", "clientIp",
	},
	"chat_tool_action": {
		"agentName", "agentType", "arch", "codebuddy.conversation_request_id", "codebuddy.session_id", "commit",
		"conversationId", "cpuCores", "cpuModel", "eventCode", "extName", "extVersion",
		"ideName", "ideType", "ideVersion", "machineId", "memorySize", "messageId",
		"os", "osVersion", "parentConversationId", "presentAt", "product", "releaseDate",
		"reportDelay", "requestId", "requestModelId", "requestModelName", "rootRequestId", "sessionId",
		"stepCount", "timestamp", "timezone", "toolCallId", "toolCallSuccessful", "toolErrorCode",
		"toolErrorCodeKey", "toolErrorMessage", "toolName", "toolStatus", "totalStepCount", "traceId",
		"type", "userId", "userNickname", "username", "clientIp",
	},
	"user_task_failed_credit_refund": {
		"arch", "codebuddy.conversation_request_id", "codebuddy.session_id", "commit", "conversationId", "cpuCores",
		"cpuModel", "eventCode", "extName", "extVersion", "ideName", "ideType",
		"ideVersion", "machineId", "memorySize", "os", "osVersion", "product",
		"reason", "releaseDate", "reportDelay", "requestId", "rootRequestId", "sessionId",
		"timestamp", "timezone", "userId", "userNickname", "username", "clientIp",
	},
}
