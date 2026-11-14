package session

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// imagePartSig 计算 part 原文摘要（sha256 前 8 hex），供期望值精确构造——
// 签名算法变更时测试期望值随此 helper 单点同步。
func imagePartSig(t *testing.T, part string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(part))
	return hex.EncodeToString(sum[:4])
}

// TestResolveConversationID 覆盖 conversationId 提取的 snake/camel/缺失三态：
//   - metadata.conversation_id / metadata.conversationId → 取值
//   - 顶层 conversation_id / conversationId → 取值（snake 优先同 ExtractKey）
//   - 缺失 / 只有 user_id → ""（会话头族语义只认对话 ID，绝不回落 user_id）
func TestResolveConversationID(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"metadata snake_case", `{"metadata":{"conversation_id":"conv-1"}}`, "conv-1"},
		{"metadata camelCase", `{"metadata":{"conversationId":"conv-2"}}`, "conv-2"},
		{"top-level snake_case", `{"conversation_id":"conv-3"}`, "conv-3"},
		{"top-level camelCase", `{"conversationId":"conv-4"}`, "conv-4"},
		{"snake wins over camel", `{"conversation_id":"conv-s","conversationId":"conv-c"}`, "conv-s"},
		{"missing", `{"model":"glm-5.2"}`, ""},
		{"empty body", ``, ""},
		{"broken json", `{broken`, ""},
		{"metadata user_id only", `{"metadata":{"user_id":"u1"}}`, ""},
		{"top-level user_id only", `{"user_id":"u1"}`, ""},
		{"empty string value", `{"conversationId":""}`, ""},
		{"non-string value", `{"conversationId":123}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveConversationID([]byte(c.body)); got != c.want {
				t.Errorf("ResolveConversationID(%q) = %q want %q", c.body, got, c.want)
			}
		})
	}
}

// TestNewMessageIDFormat 32 位 hex 且不为空、两次调用大概率不同（随机性冒烟）。
func TestNewMessageIDFormat(t *testing.T) {
	for i := 0; i < 50; i++ {
		id := NewMessageID()
		if len(id) != 32 {
			t.Fatalf("NewMessageID() = %q len=%d want 32", id, len(id))
		}
		for _, ch := range id {
			if !strings.ContainsRune("0123456789abcdef", ch) {
				t.Fatalf("NewMessageID() = %q has non-hex char %q", id, ch)
			}
		}
	}
}

func TestNewConversationIDShape(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := NewConversationID()
		// 版本位 7（UUIDv7）、variant 8/9/a/b，带连字符。
		// 抓官方内置 CLI 实测：X-Conversation-ID = 01a1a770-b339-7b06-80ab-574f992a9397
		if len(id) != 36 || id[14] != '7' || !strings.ContainsRune("89ab", rune(id[19])) {
			t.Fatalf("NewConversationID() = %q, want UUID v7 shape", id)
		}
		for _, pos := range []int{8, 13, 18, 23} {
			if id[pos] != '-' {
				t.Fatalf("NewConversationID() = %q, missing hyphen at %d", id, pos)
			}
		}
		if seen[id] {
			t.Fatalf("duplicate conversation ID: %q", id)
		}
		seen[id] = true
	}
}

// TestRequestIDForKeyStability 同 key 恒稳定、异 key 各不同、空 key 每次新值。
func TestRequestIDForKeyStability(t *testing.T) {
	// 预热清空包级缓存，避免其他测试污染 key（测试隔离）。
	k1a := RequestIDForKey("conv-a")
	k1b := RequestIDForKey("conv-a")
	if k1a != k1b {
		t.Errorf("same key should be stable: %q vs %q", k1a, k1b)
	}
	k2 := RequestIDForKey("conv-b")
	if k1a == k2 {
		t.Errorf("different keys should differ: %q", k1a)
	}
	// 空 key：每次调用生成新值（无会话则无"会话内稳定"语义）。
	e1 := RequestIDForKey("")
	e2 := RequestIDForKey("")
	if e1 == e2 {
		t.Errorf("empty key should yield fresh values each call: %q", e1)
	}
	// 稳定值自身也须是 32 hex（可作 B3 TraceId 直接使用）。
	for _, id := range []string{k1a, k2, e1} {
		if len(id) != 32 {
			t.Errorf("RequestIDForKey value %q len=%d want 32", id, len(id))
		}
	}
}

// TestTurnKeyExtraction 轮级兜底键的提取：取**最后一条** user 消息的「序号+文本」，
// 轮内追加 assistant/tool 消息不改变键；无 user / 无文本 / 坏 JSON 一律空串。
func TestTurnKeyExtraction(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"single user", `{"messages":[{"role":"user","content":"你好"}]}`, "u0:你好"},
		{"last user wins", `{"messages":[{"role":"user","content":"第一问"},{"role":"assistant","content":"答"},{"role":"user","content":"第二问"}]}`, "u2:第二问"},
		// agent 多步：轮内追加 assistant/tool 消息，末条 user 位置与内容不变 → 同键。
		{"agent step keeps same key", `{"messages":[{"role":"user","content":"任务"},{"role":"assistant","tool_calls":[{"id":"c1"}]},{"role":"tool","content":"结果"}]}`, "u0:任务"},
		{"multimodal parts text joined", `{"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:x"}}]}]}`, "u0:看图\n[image_url:" + imagePartSig(t, `{"type":"image_url","image_url":{"url":"data:x"}}`) + "]"},
		{"no user message", `{"messages":[{"role":"system","content":"sys"}]}`, ""},
		{"empty messages", `{"messages":[]}`, ""},
		{"messages key absent", `{"model":"glm-5.2"}`, ""},
		{"broken json", `{broken`, ""},
		{"empty body", ``, ""},
		{"empty content", `{"messages":[{"role":"user","content":""}]}`, ""},
		{"null content", `{"messages":[{"role":"user","content":null}]}`, ""},
		// 纯图片 content 按内容签名派生非空轮级键（G1 修复，原为 ""）；键含 image part 摘要。
		{"image only content", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, "u0:[image_url:" + imagePartSig(t, `{"type":"image_url","image_url":{"url":"x"}}`) + "]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TurnKey([]byte(c.body)); got != c.want {
				t.Errorf("TurnKey(%q) = %q want %q", c.body, got, c.want)
			}
		})
	}
}

// TestTurnRequestIDDerivation 轮级 ID 是纯派生：同键恒同值、异键异值、空键每次新值、
// 形态恒 32 hex。
func TestTurnRequestIDDerivation(t *testing.T) {
	a1 := TurnRequestID("u0:同一个问题")
	a2 := TurnRequestID("u0:同一个问题")
	if a1 != a2 {
		t.Errorf("same turn key should derive same id: %q vs %q", a1, a2)
	}
	if b := TurnRequestID("u0:另一个问题"); b == a1 {
		t.Errorf("different turn keys should derive different ids: %q", b)
	}
	// 同文本但序号不同（不同轮里内容相同的提问）也要分开。
	if c := TurnRequestID("u2:同一个问题"); c == a1 {
		t.Errorf("same text at different position should differ: %q", c)
	}
	// 空键：无轮可聚合 → 每次新值（保持原有请求级独立行为）。
	if e1, e2 := TurnRequestID(""), TurnRequestID(""); e1 == e2 {
		t.Errorf("empty turn key should yield fresh values each call: %q", e1)
	}
	for _, id := range []string{a1, TurnRequestID(""), TurnRequestID("x")} {
		if len(id) != 32 {
			t.Errorf("TurnRequestID value %q len=%d want 32", id, len(id))
		}
	}
}

// TestDerivedIDsAreUUIDv7 会话头族的 ID 必须是 UUIDv7 形态。
//
// 依据：官方 CLI 内核里 assertClientConversationRequestId 明确断言
// "conversationRequestId must be a lowercase UUIDv7 encoded as 32 hexadecimal
// characters"，且依赖 uuidv7 包；抓包实测我们此前发的是 ver=8/9/a（sha256 派生的
// 第 13 位随机），属于形态不一致。
func TestDerivedIDsAreUUIDv7(t *testing.T) {
	check := func(name, id string) {
		t.Helper()
		if len(id) != 32 {
			t.Fatalf("%s: len=%d want 32 (%q)", name, len(id), id)
		}
		if id[12] != '7' {
			t.Errorf("%s: version nibble=%q want '7' (%q)", name, id[12], id)
		}
		if v := id[16]; v != '8' && v != '9' && v != 'a' && v != 'b' {
			t.Errorf("%s: variant nibble=%q want 8/9/a/b (%q)", name, v, id)
		}
	}
	for i := 0; i < 32; i++ {
		check("NewMessageID", NewMessageID())
	}
	check("RequestIDForKey", RequestIDForKey("session-abc"))
	check("TurnRequestID", TurnRequestID("turn-abc"))
}

// TestDerivedIDsStayStable 打版本位不能破坏「同一会话同值」：缓存槽依赖这一点。
func TestDerivedIDsStayStable(t *testing.T) {
	if a, b := RequestIDForKey("k1"), RequestIDForKey("k1"); a != b {
		t.Fatalf("RequestIDForKey 不稳定: %q vs %q", a, b)
	}
	if a, b := TurnRequestID("t1"), TurnRequestID("t1"); a != b {
		t.Fatalf("TurnRequestID 不稳定: %q vs %q", a, b)
	}
	if RequestIDForKey("k1") == RequestIDForKey("k2") {
		t.Fatal("不同键派生出同一个 ID")
	}
}

// TestDerivedIDPseudoTimeInWindow 伪时间必须落在锚点窗口内（否则 ID 会像来自 1970 年）。
func TestDerivedIDPseudoTimeInWindow(t *testing.T) {
	for i := 0; i < 64; i++ {
		id := NewMessageID()
		raw, err := hex.DecodeString(id)
		if err != nil || len(raw) != 16 {
			t.Fatalf("ID 不是 32 位 hex: %q (%v)", id, err)
		}
		ms := int64(raw[0])<<40 | int64(raw[1])<<32 | int64(raw[2])<<24 |
			int64(raw[3])<<16 | int64(raw[4])<<8 | int64(raw[5])
		ts := time.UnixMilli(ms).UTC()
		if ts.Before(v7Epoch) || !ts.Before(v7Epoch.Add(v7Window)) {
			t.Fatalf("伪时间越界: %v（id=%q）", ts, id)
		}
	}
}
