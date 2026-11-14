package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// wantDesktopUA 默认配置下的桌面 UA（版本源统一后由 clientVersion/cliVersion 拼出，
// 不再是硬编码常量；测试用它做期望值，避免把某一具体版本号写死两份）。
var wantDesktopUA = (&Client{}).desktopUA()

// TestReportDesktopEventFingerprint 断言桌面指纹上报：走 chatBase、UA 为桌面形状、
// 事件数组自动注入 workbuddy-desktop 公共指纹（覆盖同名业务键）。
func TestReportDesktopEventFingerprint(t *testing.T) {
	var got []map[string]any
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/report" {
			t.Errorf("path=%s want /v2/report", r.URL.Path)
		}
		gotUA = r.Header.Get("User-Agent")
		if r.Header.Get("X-User-Id") != "u-dt" {
			t.Errorf("X-User-Id=%q want u-dt", r.Header.Get("X-User-Id"))
		}
		if r.Header.Get("X-Product") != "SaaS" {
			t.Errorf("X-Product=%q want SaaS", r.Header.Get("X-Product"))
		}
		// X-Domain 必须是裸主机名（官方全部 auth.domain），不能是带 scheme 的完整 URL。
		if got := r.Header.Get("X-Domain"); got != "www.codebuddy.cn" {
			t.Errorf("X-Domain=%q want www.codebuddy.cn（裸主机名，非完整 URL）", got)
		}
		// X-Request-ID 恒 32 位小写 hex（官方客户端形态）。此前是
		// deriveID(36hex)+纳秒后 6 位的 42 字符混合串——长度和字符集都偏离，
		// 且与同批 DesktopChatWithExpert 的纯 19 位纳秒数字并存。
		if id := hdrExact(r.Header, "X-Request-ID"); !isLowerHexN(id, 32) {
			t.Errorf("X-Request-ID=%q 不是 32 位小写 hex（官方形态）", id)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("body must be array: %v", err)
		}
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	err := c.ReportDesktopEvent(&auth.Auth{AccessToken: "at", UID: "u-dt", Nickname: "RenJie", Domain: "www.codebuddy.cn"},
		DesktopEvent{"eventCode": "agent_task_created", "mode": "craft"})
	if err != nil {
		t.Fatalf("report desktop: %v", err)
	}
	if gotUA != wantDesktopUA {
		t.Errorf("UA=%q want %q", gotUA, wantDesktopUA)
	}
	if len(got) != 1 {
		t.Fatalf("events=%d want 1", len(got))
	}
	ev := got[0]
	if ev["extName"] != "workbuddy-desktop" {
		t.Errorf("extName=%v want workbuddy-desktop", ev["extName"])
	}
	if ev["ideType"] != "WorkBuddy" || ev["ideName"] != "WorkBuddy" {
		t.Errorf("ideType/ideName=%v/%v want WorkBuddy", ev["ideType"], ev["ideName"])
	}
	if ev["userId"] != "u-dt" {
		t.Errorf("userId=%v want u-dt", ev["userId"])
	}
	if ev["userNickname"] != "RenJie" {
		t.Errorf("userNickname=%v want RenJie", ev["userNickname"])
	}
	// buddyId / buddyName：官方每条事件都带（值为空串），属于公共指纹，
	// 不是桌面渲染层的私有字段。
	if v, ok := ev["buddyId"]; !ok || v != "" {
		t.Errorf("buddyId=%v (present=%v) want empty string", v, ok)
	}
	if v, ok := ev["buddyName"]; !ok || v != "" {
		t.Errorf("buddyName=%v (present=%v) want empty string", v, ok)
	}
	// cpuModel：官方桌面指纹字段，与核数同一条目派生（自洽）。
	if _, _, _, wantModel := desktopHardware(&auth.Auth{UID: "u-dt"}); ev["cpuModel"] != wantModel {
		t.Errorf("cpuModel=%v want %v", ev["cpuModel"], wantModel)
	}
	if ev["mode"] != "craft" {
		t.Errorf("业务字段 mode=%v 被指纹覆盖或缺失", ev["mode"])
	}
	// machineId 应为 uid 稳定派生（36 hex）。
	if len(ev["machineId"].(string)) != 36 {
		t.Errorf("machineId 长度=%d want 36", len(ev["machineId"].(string)))
	}
}

// TestDesktopChatSequenceShape 断言完整对话事件链的事件码顺序与关键字段。
func TestDesktopChatSequenceShape(t *testing.T) {
	const (
		conv = "01a1aebe-4002-707d-b5fa-c9e6ba561930"
		req  = "01a1aebe41ad7f3982cede6c5da5a6b5"
		msg  = "01a1aebe45277249ba9f0eb5b4d5b0c1"
	)
	events := DesktopChatSequence(conv, req, msg, "", "")
	wantCodes := []string{
		"agent_task_created", "chat_message_send", "chat_request_send",
		"chat_message_response", "chat_message_status", "chat_request_response",
	}
	if len(events) != len(wantCodes) {
		t.Fatalf("events=%d want %d", len(events), len(wantCodes))
	}
	for i, want := range wantCodes {
		if events[i]["eventCode"] != want {
			t.Errorf("events[%d].eventCode=%v want %v", i, events[i]["eventCode"], want)
		}
	}
	resp := events[3]
	if resp["isSuccessful"] != true {
		t.Errorf("chat_message_response.isSuccessful=%v want true（RichMeow 判据）", resp["isSuccessful"])
	}
	if resp["finishReason"] != "stop" {
		t.Errorf("finishReason=%v want stop", resp["finishReason"])
	}
	if resp["conversationId"] != conv {
		t.Errorf("conversationId=%v want %s", resp["conversationId"], conv)
	}
}

// TestSetAppearanceTheme 断言主题设置端点形状（点亮 Hp_Appearance 的 API）。
func TestSetAppearanceTheme(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/user-asset/appearance/set" {
			t.Errorf("path=%s want /v2/user-asset/appearance/set", r.URL.Path)
		}
		if r.Header.Get("User-Agent") != wantDesktopUA {
			t.Errorf("UA=%q want desktop UA", r.Header.Get("User-Agent"))
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &got)
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	if err := c.SetAppearanceTheme(&auth.Auth{AccessToken: "at", UID: "u1"}, "theme-tkmw7j"); err != nil {
		t.Fatalf("set theme: %v", err)
	}
	if got["kind"] != "theme" || got["resource_key"] != "theme-tkmw7j" {
		t.Errorf("body=%v want kind=theme resource_key=theme-tkmw7j", got)
	}
}

// isLowerHexN 判定 s 是否为 n 位小写 hex——官方客户端所有 ID 的固定形态
// （X-Request-ID / X-Conversation-* 恒 32 位，设备标识 36/40 位）。
func isLowerHexN(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// TestDesktopChatWithExpertRequestIDShape 专家对话通道（DesktopChatWithExpert）的
// X-Request-ID 也必须是 32 位小写 hex。此前这里是纯 19 位纳秒数字，而桌面上报通道
// 是 36hex+纳秒后 6 位的 42 字符混合串——同一客户端两条通道各发一种非官方形态。
// 顺带钉住这条路径本来就带的 X-Agent-Intent/X-Agent-Type（主 chat 路径还缺）。
func TestDesktopChatWithExpertRequestIDShape(t *testing.T) {
	var gotID, gotIntent, gotType string
	var gotConvID, gotConvReqID, gotRootID, gotMsgID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/completions" {
			t.Errorf("path=%s want /v2/chat/completions", r.URL.Path)
		}
		gotID = hdrExact(r.Header, "X-Request-ID")
		gotIntent = r.Header.Get("X-Agent-Intent")
		gotType = r.Header.Get("X-Agent-Type")
		gotConvID = hdrExact(r.Header, "X-Conversation-ID")
		gotConvReqID = hdrExact(r.Header, "X-Conversation-Request-ID")
		gotRootID = hdrExact(r.Header, "X-Root-Request-ID")
		gotMsgID = hdrExact(r.Header, "X-Conversation-Message-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"cmb-0123456789abcdef0123456789abcdef\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"2\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	_, reqID, err := c.DesktopChatWithExpert(&auth.Auth{UID: "u-dt", AccessToken: "at", ExpiresAt: 9999999999}, "ex_test", false)
	if err != nil {
		t.Fatalf("DesktopChatWithExpert: %v", err)
	}
	if !isLowerHexN(gotID, 32) {
		t.Errorf("X-Request-ID=%q 不是 32 位小写 hex（官方形态）", gotID)
	}
	if reqID != "cmb-0123456789abcdef0123456789abcdef" {
		t.Errorf("解析出的服务端 requestId=%q want cmb-0123…", reqID)
	}
	if gotIntent != "craft" {
		t.Errorf("X-Agent-Intent=%q want craft", gotIntent)
	}
	if gotType != "main" {
		t.Errorf("X-Agent-Type=%q want main", gotType)
	}
	// 会话头族必须齐全：上游按「三件套/四件套是否齐」做渠道校验，缺任一即
	// 400 / 11128 "conversation id missing"。这条手搓头的路径此前缺
	// X-Conversation-Request-ID 与 X-Root-Request-ID 两个。
	if gotConvID == "" {
		t.Error("缺 X-Conversation-ID")
	}
	if !isLowerHexN(gotConvReqID, 32) {
		t.Errorf("X-Conversation-Request-ID=%q 不是 32 位小写 hex", gotConvReqID)
	}
	if gotRootID != gotConvReqID {
		t.Errorf("X-Root-Request-ID=%q 应与 X-Conversation-Request-ID=%q 同值（主对话即根请求）", gotRootID, gotConvReqID)
	}
	if gotMsgID != gotID {
		t.Errorf("X-Conversation-Message-ID=%q 应与 X-Request-ID=%q 同值（CLI 内核两个头吃同一个值）", gotMsgID, gotID)
	}
	if gotConvReqID == gotMsgID {
		t.Error("轮级 ID 与消息级 ID 撞成同一个值：官方是两次独立生成（轮级早于消息级）")
	}
}

// TestDesktopHardwarePerAccount 硬件参数按账号派生：同账号跨调用稳定、账号间分散。
//
// 此前 cpuCores/memorySize/osVersion 是全局硬编码（20 核 / 24GB / 10.0.26220），
// 多个账号向同一后台汇报同一台机器——这是比「参数真假」更直接的多账号关联信号。
func TestDesktopHardwarePerAccount(t *testing.T) {
	c1, m1, o1, cm1 := desktopHardware(&auth.Auth{UID: "u-alpha"})
	c2, m2, o2, cm2 := desktopHardware(&auth.Auth{UID: "u-alpha"})
	if c1 != c2 || m1 != m2 || o1 != o2 || cm1 != cm2 {
		t.Errorf("同账号应稳定：%d/%d/%s/%s vs %d/%d/%s/%s", c1, m1, o1, cm1, c2, m2, o2, cm2)
	}
	// cpuModel 必须是真实 CPU 的型号串。此前这里断言「含 Core(TM)」，
	// 那是硬编码 9 条 Intel 时的假设；现在配置表取自真实 dxdiag 报告，
	// 还包含 AMD Ryzen、Intel Xeon、Celeron 等，断言改成「属于已知厂商」。
	if !strings.Contains(cm1, "Intel(R)") && !strings.HasPrefix(cm1, "AMD ") {
		t.Errorf("cpuModel=%q 不像真实 CPU 型号串（应为 Intel(R)… 或 AMD …）", cm1)
	}

	seen := map[string]bool{}
	models := map[string]bool{}
	for i := 0; i < 12; i++ {
		c, m, o, cm := desktopHardware(&auth.Auth{UID: fmt.Sprintf("uid-%d", i)})
		seen[fmt.Sprintf("%d/%d/%s", c, m, o)] = true
		models[cm] = true
		if c < 2 || m < 4 {
			t.Errorf("派生出的参数不合理：cpu=%d mem=%d", c, m)
		}
		if !strings.HasPrefix(o, "10.0.") {
			t.Errorf("osVersion=%q 不是 Windows 10/11 build 形态", o)
		}
		if cm == "" {
			t.Error("cpuModel 不能为空（官方桌面指纹带这个字段）")
		}
	}
	if len(seen) < 2 {
		t.Errorf("12 个账号只出现 %d 种硬件组合，派生没起作用", len(seen))
	}
	if len(models) < 2 {
		t.Errorf("12 个账号只出现 %d 种 CPU 型号，派生没起作用", len(models))
	}
}

// TestDesktopFingerprintHasCPUModel 桌面事件指纹必须带 cpuModel，且与核数同一条目派生。
func TestDesktopFingerprintHasCPUModel(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{UID: "u-fp", Nickname: "n"}
	fp := c.desktopFingerprint(a)
	_, _, _, wantModel := desktopHardware(a)
	if got := fp["cpuModel"]; got != wantModel {
		t.Errorf("cpuModel=%v want %v", got, wantModel)
	}
	if fp["cpuCores"] == nil || fp["memorySize"] == nil || fp["osVersion"] == nil {
		t.Error("硬件三件套不应因新增 cpuModel 而丢失")
	}
}

// TestSetDomainHeaderBareHost X-Domain 必须是裸主机名（官方 29 处全部 auth.domain），
// 不能是带 scheme 的完整 URL；域为空时不发这个头（官方 `r.domain &&` 的写法）。
func TestSetDomainHeaderBareHost(t *testing.T) {
	h := http.Header{}
	setDomainHeader(h, &auth.Auth{Domain: "www.codebuddy.cn"})
	if got := h.Get("X-Domain"); got != "www.codebuddy.cn" {
		t.Errorf("X-Domain=%q want www.codebuddy.cn", got)
	}
	if strings.Contains(h.Get("X-Domain"), "://") {
		t.Error("X-Domain 不该带 scheme（此前发的是完整 URL）")
	}
	h2 := http.Header{}
	setDomainHeader(h2, &auth.Auth{})
	if _, ok := h2["X-Domain"]; ok {
		t.Error("域为空时不该发 X-Domain")
	}
}

// TestMarketExpertProtect 内容保护判定：云端专家（is_cloud=1）或付费资产
// （pay_mode 非 0）才声明保护；字段缺失时落 0，不带头（官方对占位专家同样不带）。
func TestMarketExpertProtect(t *testing.T) {
	cases := []struct {
		name string
		e    MarketExpert
		want bool
	}{
		{"云端单专家", MarketExpert{IsCloud: 1}, true},
		{"付费资产", MarketExpert{PayMode: 2}, true},
		{"云端且付费", MarketExpert{IsCloud: 1, PayMode: 1}, true},
		{"免费且非云端", MarketExpert{IsCloud: 0, PayMode: 0}, false},
		{"字段缺失（占位专家）", MarketExpert{}, false},
	}
	for _, c := range cases {
		if got := c.e.Protect(); got != c.want {
			t.Errorf("%s: Protect()=%v want %v", c.name, got, c.want)
		}
	}
}

// TestDesktopChatWithExpertModerationHeader 声明保护时才发 X-Moderation-Type，
// 且值是与官方一致的字母面量 `["protected"]`（安装包常量 Ok = '["protected"]'，
// 不是 JSON 序列化出来的）。
func TestDesktopChatWithExpertModerationHeader(t *testing.T) {
	for _, protect := range []bool{false, true} {
		var got string
		var present bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, present = r.Header["X-Moderation-Type"]
			got = hdrExact(r.Header, "X-Moderation-Type")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"cmb-0123456789abcdef0123456789abcdef\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"2\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}))
		srvURL := srv.URL
		c := &Client{HTTP: srv.Client(), ChatBaseCN: srvURL}
		if _, _, err := c.DesktopChatWithExpert(&auth.Auth{UID: "u-mod", AccessToken: "at", ExpiresAt: 9999999999}, "ex_mod", protect); err != nil {
			srv.Close()
			t.Fatalf("protect=%v: %v", protect, err)
		}
		srv.Close()

		if !protect {
			if present {
				t.Errorf("protect=false 时不该发 X-Moderation-Type，实际 %q", got)
			}
			continue
		}
		if got != `["protected"]` {
			t.Errorf("X-Moderation-Type=%q want [\"protected\"]", got)
		}
	}
}

// TestCLIChainFieldsMatchOfficial 断言 CLI 链事件上报后的字段集与官方实测白名单
// **完全一致**（不多不少）。UI 侧事件不在白名单里，不受裁剪。
func TestCLIChainFieldsMatchOfficial(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 上报前会先取一次本机出口 IP（clientIp 字段），这条不带请求体，要单独应答。
		if r.URL.Path == reportClientIPPath {
			w.Write([]byte(`{"ip":"203.0.113.9"}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("body must be array: %v", err)
		}
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	const (
		conv = "01a1b18f-e32c-742e-96b6-4c531adf8cec"
		req  = "01a1b191ff2a7c7b1e5f0c3f2d1e0a1b"
		msg  = "01a1b19200112233445566778899aabb"
	)
	trace := session.NewMessageID()
	events := append(DesktopChatSequence(conv, req, msg, "auto", "Auto"),
		DesktopToolActionEvent(conv, req, msg, trace, "call_1", "Bash", "failed", "main",
			1, 2, false, "99999", "key", "boom", "auto", "Auto"),
		DesktopTaskFailedCreditRefundEvent(conv, req, msg, "task failed"),
	)

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	if err := c.ReportDesktopEvent(&auth.Auth{AccessToken: "at", UID: "u-dt", Domain: "www.codebuddy.cn"}, events...); err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(got) != len(events) {
		t.Fatalf("上报 %d 条，期望 %d", len(got), len(events))
	}
	for _, ev := range got {
		code, _ := ev["eventCode"].(string)
		allow, ok := cliOfficialFields[code]
		if !ok {
			// 不在实测样本里的事件不参与裁剪：UI 侧事件（agent_task_created 等）
			// 与条件触发事件（chat_request_response 只在失败分支发）都属此类。
			// 生产代码对它们不做裁剪（白名单未命中即原样发出）。
			continue
		}
		allowed := map[string]bool{}
		for _, f := range allow {
			allowed[f] = true
		}
		for k := range ev {
			if !allowed[k] {
				t.Errorf("%s 多发字段 %q（官方没有）", code, k)
			}
		}
		for _, f := range allow {
			if _, ok := ev[f]; !ok {
				t.Errorf("%s 缺字段 %q（官方有）", code, f)
			}
		}
	}
}

// TestUIEventKeepsBuddyFields UI 侧事件带 buddyId/buddyName（官方 UI 报文里有），
// 不应被 CLI 白名单误裁。
func TestUIEventKeepsBuddyFields(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &got)
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	if err := c.ReportDesktopEvent(&auth.Auth{AccessToken: "at", UID: "u-dt", Domain: "www.codebuddy.cn"},
		DesktopEvent{"eventCode": "agent_task_created", "mode": "craft"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events", len(got))
	}
	for _, k := range []string{"buddyId", "buddyName"} {
		if _, ok := got[0][k]; !ok {
			t.Errorf("UI 事件不该丢 %s", k)
		}
	}
}

// TestDeviceIdentifiersAreUUID 断言 machineId / sessionId 都是官方实测的
// UUID v4 形态（8-4-4-4-12、version 位 4、variant 位 8-b）。
// 此前产出的是 36 位无横线 hex —— 长度相同但字符集形态不同。
func TestDeviceIdentifiersAreUUID(t *testing.T) {
	uuidV4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	c := &Client{}
	fp := c.desktopFingerprint(&auth.Auth{UID: "u-uuid", Nickname: "n"})
	for _, k := range []string{"machineId", "sessionId"} {
		v, _ := fp[k].(string)
		if !uuidV4.MatchString(v) {
			t.Errorf("%s=%q 不是 UUID v4 形态", k, v)
		}
	}
}

// TestSessionIDLifecycle sessionId 属于「一次客户端运行」：同进程内稳定，
// 且与账号无关（同一进程的所有账号共享一个）；machineId 属于「一台设备」：按账号固定。
func TestSessionIDLifecycle(t *testing.T) {
	c := &Client{}
	fp1 := c.desktopFingerprint(&auth.Auth{UID: "u-a"})
	fp2 := c.desktopFingerprint(&auth.Auth{UID: "u-b"})
	if fp1["sessionId"] != fp2["sessionId"] {
		t.Error("同进程内 sessionId 应一致（一个客户端运行一个）")
	}
	if fp1["machineId"] == fp2["machineId"] {
		t.Error("不同账号的 machineId 不应相同（那是设备级标识）")
	}
	fp3 := c.desktopFingerprint(&auth.Auth{UID: "u-a"})
	if fp1["machineId"] != fp3["machineId"] {
		t.Error("同一账号的 machineId 应稳定")
	}
}

// buddyapp 五连事件的**字段类型**必须与上游 schema 对齐，不是"值看起来对"就行。
//
// 2026-11-13 实测：`buddyapp_enter_click` 的 isFirstPage 发字符串 "1" 时，整条事件被
// 上游的格式校验判为 invalid（400 code=10001，invalidIndices=[2]），同一请求里另外 4 条
// 事件照常接收；把该字段改成布尔 true 后立即 code=0。此前线上发的是字符串——老账号是
// 校验加严之前点亮的历史结果，新账号则一直卡在 0/1。
//
// 这条断言的价值在于：即使有人把 `"1"` 改回来，值也不变（都是"1"的真值），只有类型变了，
// 肉眼看 diff 极易放过。
func TestBuddyAppSequenceFieldTypes(t *testing.T) {
	evs := DesktopBuddyAppSequence("cb_x", "某助手")
	if len(evs) != 5 {
		t.Fatalf("事件数应为 5，实际 %d", len(evs))
	}
	if code := evs[2]["eventCode"]; code != "buddyapp_enter_click" {
		t.Fatalf("索引 2 应是 buddyapp_enter_click，实际 %v", code)
	}
	if v, ok := evs[2]["isFirstPage"]; !ok || v != true {
		t.Errorf("isFirstPage 必须是布尔 true（上游按 schema 校验类型），实际 %#v", v)
	}
	// position 反过来必须是数字：改成字符串同样会被判 invalid（已验证）。
	if v, ok := evs[2]["position"]; !ok || v != 2 {
		t.Errorf("position 必须是数字 2，实际 %#v", v)
	}
	for i, ev := range evs {
		if _, ok := ev["eventCode"].(string); !ok {
			t.Errorf("索引 %d 缺 eventCode 字符串", i)
		}
	}
}
