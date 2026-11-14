package upstream

import (
	"regexp"
	"testing"
)

var reHex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TestDesktopChatSequenceWireShape 钉住事件链的形态约束。
//
// 依据是官方 CLI 的真实报文（2026-11-06 抓包，7 批 /v2/report）。此前这里有三个偏差：
//   - messageId 是 `msg-xxx-assistant` 这种自造串（官方是 32 位 hex，且每轮一个、
//     在 send/response/status 三条之间复用）
//   - 事件缺 conversationId / requestId / requestModelId / requestModelName
//   - traceId 直接等于 requestId（官方是独立生成的值）
func TestDesktopChatSequenceWireShape(t *testing.T) {
	const (
		conv = "01a1aebe-4002-707d-b5fa-c9e6ba561930"
		req  = "01a1aebe41ad7f3982cede6c5da5a6b5"
		msg  = "01a1aebe45277249ba9f0eb5b4d5b0c1"
	)
	events := DesktopChatSequence(conv, req, msg, "", "")

	byCode := map[string]DesktopEvent{}
	for _, ev := range events {
		byCode[ev["eventCode"].(string)] = ev
	}

	want := []string{"agent_task_created", "chat_message_send", "chat_request_send",
		"chat_message_response", "chat_message_status", "chat_request_response"}
	for _, c := range want {
		if _, ok := byCode[c]; !ok {
			t.Fatalf("缺事件 %s", c)
		}
	}

	// 会话级上下文：除 agent_task_created（UI 事件，字段未验证）外都要有
	for _, c := range want[1:] {
		ev := byCode[c]
		if ev["conversationId"] != conv {
			t.Errorf("%s conversationId=%v want %s", c, ev["conversationId"], conv)
		}
		if ev["requestId"] != req {
			t.Errorf("%s requestId=%v want %s", c, ev["requestId"], req)
		}
		if ev["rootRequestId"] != req {
			t.Errorf("%s rootRequestId 应等于 requestId", c)
		}
		if ev["parentConversationId"] != conv {
			t.Errorf("%s parentConversationId 应等于 conversationId", c)
		}
		if ev["codebuddy.session_id"] != conv || ev["codebuddy.conversation_request_id"] != req {
			t.Errorf("%s 缺 codebuddy.* 上下文", c)
		}
		if ev["agentName"] != "cli" || ev["agentType"] != "main" {
			t.Errorf("%s agentName/agentType=%v/%v", c, ev["agentName"], ev["agentType"])
		}
		tr, _ := ev["traceId"].(string)
		if !reHex32.MatchString(tr) {
			t.Errorf("%s traceId=%q 应为 32 hex", c, tr)
		}
		if tr == req {
			t.Errorf("%s traceId 不应等于 requestId（官方是独立值）", c)
		}
		// 模型：官方 CN 端实测是 auto / Auto
		if ev["requestModelId"] != "auto" || ev["requestModelName"] != "Auto" {
			t.Errorf("%s requestModelId/Name=%v/%v want auto/Auto", c,
				ev["requestModelId"], ev["requestModelName"])
		}
	}

	// messageId：每轮一个，send/response/status 复用同一个，且为 32 hex
	for _, c := range []string{"chat_message_send", "chat_message_response", "chat_message_status"} {
		got, _ := byCode[c]["messageId"].(string)
		if got != msg {
			t.Errorf("%s messageId=%q want %q", c, got, msg)
		}
		if !reHex32.MatchString(got) {
			t.Errorf("%s messageId=%q 不是 32 位小写 hex", c, got)
		}
	}
	if _, ok := byCode["chat_request_send"]["messageId"]; ok {
		t.Error("chat_request_send 官方不带 messageId")
	}

	// 各事件的专有字段（官方实测）
	// agent_task_created 属桌面 UI 事件，字段取自渲染层构造处（见 desktop.go 注释）
	for _, k := range []string{"is_worktree_task", "is_from_code", "task_target", "workspace_type"} {
		if _, ok := byCode["agent_task_created"][k]; !ok {
			t.Errorf("agent_task_created 缺字段 %s", k)
		}
	}
	// agent_task_created 是桌面 UI 事件：官方报文里没有 CLI 上下文，
	// 也没有 traceId 系列；buddyId/buddyName 由公共指纹注入，不在事件载荷里重复。
	for _, k := range []string{"buddyId", "buddyName", "traceId", "rootRequestId",
		"parentConversationId", "agentName", "agentType",
		"codebuddy.session_id", "codebuddy.conversation_request_id"} {
		if _, ok := byCode["agent_task_created"][k]; ok {
			t.Errorf("agent_task_created 不应有 %s（官方 UI 事件不带这些字段）", k)
		}
	}
	// 官方实测：agent_task_created 的 messageId 与 requestId 同值。
	if got := byCode["agent_task_created"]["messageId"]; got != req {
		t.Errorf("agent_task_created messageId=%v want requestId %q", got, req)
	}

	need := map[string][]string{
		"chat_message_send":     {"historyCount", "isContextTruncated", "currentStepCount", "totalStepCount", "compactionCount"},
		"chat_request_send":     {"mode", "inputLength", "isPlan", "isMaxMode", "vcsType", "vcsRepo", "vcsBranchName", "vcsRevId", "recommendId", "skillId"},
		"chat_message_response": {"responseModelId", "totalStepCount", "inputToken", "outputToken", "totalToken", "finishReason", "firstTokenAt"},
		"chat_message_status":   {"messageErrorCode"},
		"chat_request_response": {"mode", "toolCallCount", "reason", "contentFilterMessage", "expertId", "presentAt"},
	}
	for c, keys := range need {
		for _, k := range keys {
			if _, ok := byCode[c][k]; !ok {
				t.Errorf("%s 缺字段 %s", c, k)
			}
		}
	}

	// 官方 chat_request_send 没有这些字段，早期我们多发过
	for _, k := range []string{"maxToken", "maxSteps", "temperature", "maxRetries", "skillCount", "totalCount"} {
		if _, ok := byCode["chat_request_send"][k]; ok {
			t.Errorf("chat_request_send 不应有 %s（官方报文里没有）", k)
		}
	}
}

// TestDesktopTaskEventsShape 钉住「任务完成 / 回复完成」两个事件的实测形态。
func TestDesktopTaskEventsShape(t *testing.T) {
	const (
		conv = "0f6e2a1c-3b7d-4c8e-9a21-5d4f6e7a8b9c"
		req  = "01a1b0baeedc79ad89dc35fe769911fc"
	)

	done := DesktopTaskCompletedEvent(conv, 2, 223696)
	if done["eventCode"] != "agent_task_completed" || done["mode"] != "LOCAL" {
		t.Errorf("agent_task_completed 头字段=%v/%v", done["eventCode"], done["mode"])
	}
	if done["task_id"] != conv {
		t.Errorf("task_id=%v want conversationId", done["task_id"])
	}
	if done["total_steps"] != 2 || done["duration_ms"] != int64(223696) {
		t.Errorf("steps/duration=%v/%v", done["total_steps"], done["duration_ms"])
	}
	for _, k := range []string{"requestId", "messageId", "conversationId"} {
		if _, ok := done[k]; ok {
			t.Errorf("agent_task_completed 不应有 %s（官方只有 task_id）", k)
		}
	}

	resp := DesktopChatResponseCompleteEvent(conv, req)
	if resp["conversationId"] != conv || resp["requestId"] != req {
		t.Errorf("chat_response_complete 标识=%v/%v", resp["conversationId"], resp["requestId"])
	}
	if resp["source"] != "work" || resp["artifactType"] != "" {
		t.Errorf("source/artifactType=%v/%v want work/\"\"", resp["source"], resp["artifactType"])
	}
	for _, k := range []string{"inputToken", "outputToken", "finishReason"} {
		if _, ok := resp[k]; ok {
			t.Errorf("chat_response_complete 不应有 %s（官方不带 token 与 finishReason）", k)
		}
	}
}
