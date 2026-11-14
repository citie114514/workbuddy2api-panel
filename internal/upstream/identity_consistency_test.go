package upstream

import (
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 本文件是「出站身份自洽」的护栏，不是功能测试。
//
// 为什么要它：网关现在有 57 个出站头、9 条硬件配置、4 类 ID、3 个版本号，全靠人肉
// 保持彼此自洽。一次扩族、一次补头，就很容易写出「UA 报 5.9.3、X-IDE-Version 报
// 5.9.2」或「轮级 ID 与消息级 ID 撞成同一个值」这种**只有上游看得见**的破绽——
// 今天改限流码族和专家路径时就各差点犯一次。
//
// 它只读本地构造结果，不发任何网络请求。

var (
	reLowerHex32     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	reUUIDWithHyphen = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// TestChatIdentitySelfConsistent 校验一次对话请求的身份参数彼此不打架。
func TestChatIdentitySelfConsistent(t *testing.T) {
	c := &Client{}
	a := testAuth()
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.ChatHeaders(req, a, "", ChatMeta{})

	// ① 版本号三处一致：UA 的 `WorkBuddy/<ver>` 段、`CLI/<ver>` 段、X-IDE-Version。
	//    官方 buildUserAgent 与 getCustomHeaders 用的是同一个 product.json 版本，
	//    我们这里由 clientVersion()/cliVersion() 统一喂，必须证明没被谁硬编码绕过。
	ua := hdrExact(req.Header, "User-Agent")
	parts := strings.Fields(ua)
	if len(parts) != 3 {
		t.Fatalf("User-Agent=%q 应为三段 `WorkBuddy/<v> <app>/<v> CLI/<cli>`", ua)
	}
	ideVer := hdrExact(req.Header, "X-IDE-Version")
	if !strings.HasPrefix(parts[0], "WorkBuddy/") {
		t.Errorf("UA 首段=%q 应为 WorkBuddy/<ver>", parts[0])
	}
	if got := strings.TrimPrefix(parts[0], "WorkBuddy/"); got != ideVer {
		t.Errorf("UA 版本 %q 与 X-IDE-Version %q 不一致（同一请求里两个版本号=直接破绽）", got, ideVer)
	}
	if !strings.HasPrefix(parts[2], "CLI/") {
		t.Errorf("UA 第三段=%q 应为 CLI/<ver>", parts[2])
	}
	if strings.TrimPrefix(parts[1], "WorkBuddy/") != ideVer {
		t.Errorf("UA 第二段=%q 的版本应与首段一致", parts[1])
	}

	// ② 会话四件套齐全且非空：上游按「头族齐不齐」做渠道校验，缺任一即 400/11128。
	for _, k := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID", "X-Root-Request-ID"} {
		if hdrExact(req.Header, k) == "" {
			t.Errorf("缺会话头 %s", k)
		}
	}

	// ③ ID 关系（官方 CLI 内核的确定性关系，写错就是自相矛盾）：
	//    X-Request-ID == X-Conversation-Message-ID（同一个 messageId）
	//    X-Root-Request-ID == X-Conversation-Request-ID（主对话的根请求=本轮请求）
	//    轮级 ≠ 消息级（两次独立生成）
	msgID := hdrExact(req.Header, "X-Conversation-Message-ID")
	reqID := hdrExact(req.Header, "X-Request-ID")
	convReqID := hdrExact(req.Header, "X-Conversation-Request-ID")
	rootID := hdrExact(req.Header, "X-Root-Request-ID")
	if msgID != reqID {
		t.Errorf("X-Request-ID=%q 应等于 X-Conversation-Message-ID=%q（CLI 内核两个头吃同一个值）", reqID, msgID)
	}
	if rootID != convReqID {
		t.Errorf("X-Root-Request-ID=%q 应等于 X-Conversation-Request-ID=%q（主对话即根请求）", rootID, convReqID)
	}
	if convReqID == msgID {
		t.Error("轮级 ID 与消息级 ID 撞成同一个值：官方是两次独立生成")
	}

	// ④ ID 形态：消息级/轮级是 32 位小写 hex 且版本位是 7（UUIDv7）；
	//    会话级是带连字符的 UUID。官方 CLI 断言 conversationRequestId 必须是
	//    lowercase UUIDv7 编码成 32 hex——版本位不是 7 就是可检索的形态差异。
	for _, kv := range [][2]string{{"X-Conversation-Message-ID", msgID}, {"X-Request-ID", reqID}, {"X-Conversation-Request-ID", convReqID}, {"X-Root-Request-ID", rootID}} {
		if !reLowerHex32.MatchString(kv[1]) {
			t.Errorf("%s=%q 不是 32 位小写 hex", kv[0], kv[1])
			continue
		}
		if kv[1][12] != '7' {
			t.Errorf("%s=%q 第 13 位应为 7（UUIDv7 版本位），实际 %q", kv[0], kv[1], kv[1][12])
		}
	}
	// 会话级 ID 的版本位也必须是 7：抓官方内置 CLI 实测是
	// `01a1a770-b339-7b06-80ab-574f992a9397`（v7，时间戳打头）。此前这里是 v4。
	if conv := hdrExact(req.Header, "X-Conversation-ID"); !reUUIDWithHyphen.MatchString(conv) {
		t.Errorf("X-Conversation-ID=%q 应为带连字符的 UUID 形态", conv)
	} else if conv[14] != '7' {
		t.Errorf("X-Conversation-ID=%q 版本位应为 7（UUIDv7，与官方一致），实际 %q", conv, conv[14])
	}

	// ⑤ baggage 与 X-Conversation-ID 同源：官方把会话 id 与轮级 id 作为 W3C
	//    baggage 双发，两处取值必须一致，否则链路聚合对不上。
	bag := hdrExact(req.Header, "baggage")
	if !strings.Contains(bag, "codebuddy.session_id="+hdrExact(req.Header, "X-Conversation-ID")) {
		t.Errorf("baggage=%q 的 session_id 与 X-Conversation-ID 不一致", bag)
	}
	if !strings.Contains(bag, "codebuddy.conversation_request_id="+convReqID) {
		t.Errorf("baggage=%q 的 conversation_request_id 与轮级 ID 不一致", bag)
	}

	// ⑥ 链路族自洽：traceparent 的 trace-id 段 == X-Trace-ID，b3 首段同值。
	tp := hdrExact(req.Header, "traceparent")
	traceID := hdrExact(req.Header, "X-Trace-ID")
	if !strings.Contains(tp, "-"+traceID+"-") {
		t.Errorf("traceparent=%q 里的 trace-id 与 X-Trace-ID=%q 不一致", tp, traceID)
	}
	if b3 := hdrExact(req.Header, "b3"); !strings.HasPrefix(b3, traceID+"-") {
		t.Errorf("b3=%q 首段应与 X-Trace-ID=%q 同值", b3, traceID)
	}
}

// TestDesktopFingerprintSelfConsistent 校验设备指纹内部自洽。
//
// 官方 OSEventProcessor 的取值是同一台机器的真实属性（os=process.platform、
// cpuModel=os.cpus()[0].model、cpuCores=cpus().length、memorySize=总内存）。所以
// 「os 是 Windows 却报 Apple M2」「4 核配 i9」这类组合在真机上不可能出现，
// 一旦出现就是自己造的矛盾。
func TestDesktopFingerprintSelfConsistent(t *testing.T) {
	c := &Client{}
	// 覆盖足够多的 uid，把硬件表各分支都走一遍。
	for i := 0; i < 40; i++ {
		uid := "u-selfcheck-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		a := &auth.Auth{UID: uid}
		fp := c.desktopFingerprint(a)

		osName, _ := fp["os"].(string)
		cpuModel, _ := fp["cpuModel"].(string)
		osVersion, _ := fp["osVersion"].(string)
		cpuCores, _ := fp["cpuCores"].(int)
		memGB, _ := fp["memorySize"].(int)

		// os 与 cpuModel 必须同源：Windows 不可能报 Apple 芯片，macOS 不可能报 Intel/AMD。
		isApple := strings.Contains(cpuModel, "Apple")
		switch osName {
		case "win32":
			if isApple {
				t.Errorf("uid=%s: os=win32 却报 cpuModel=%q（不可能的组合）", uid, cpuModel)
			}
			if !strings.HasPrefix(osVersion, "10.") {
				t.Errorf("uid=%s: os=win32 的 osVersion=%q 应为 `10.x` 形式", uid, osVersion)
			}
		case "darwin":
			if !isApple {
				t.Errorf("uid=%s: os=darwin 却报 cpuModel=%q（应为 Apple 芯片）", uid, cpuModel)
			}
		default:
			t.Errorf("uid=%s: os=%q 不是 process.platform 的合法取值", uid, osName)
		}

		// 核数与内存必须落在真实存在过的组合里，且不与 cpuModel 矛盾。
		if cpuCores < 4 || cpuCores > 32 {
			t.Errorf("uid=%s: cpuCores=%d 不在常见区间", uid, cpuCores)
		}
		if memGB < 8 || memGB > 128 {
			t.Errorf("uid=%s: memorySize=%dGB 不在常见区间", uid, memGB)
		}
		if strings.Contains(cpuModel, "i9") && memGB < 32 {
			t.Errorf("uid=%s: %q 配 %dGB 内存，不像真机", uid, cpuModel, memGB)
		}

		// 版本号与对话头同源。
		if v, _ := fp["ideVersion"].(string); v != c.clientVersion() {
			t.Errorf("uid=%s: 上报 ideVersion=%q 与 clientVersion()=%q 不一致", uid, v, c.clientVersion())
		}
		if v, _ := fp["extVersion"].(string); v != c.clientVersion() {
			t.Errorf("uid=%s: 上报 extVersion=%q 与 clientVersion()=%q 不一致", uid, v, c.clientVersion())
		}
	}
}

// TestDeviceIDIsolation 设备标识按账号隔离：同账号稳定、跨账号互异。
//
// 全池共用一套 machineId 是最直接的多账号关联信号；反过来每次重启都换一个，
// 又不符合「固定设备」的语义。两个方向都钉住。
func TestDeviceIDIsolation(t *testing.T) {
	c := &Client{}
	seen := map[string]string{}
	for i := 0; i < 20; i++ {
		uid := "u-iso-" + string(rune('a'+i))
		a := &auth.Auth{UID: uid}
		fp1 := c.desktopFingerprint(a)
		fp2 := c.desktopFingerprint(a)
		m1, _ := fp1["machineId"].(string)
		m2, _ := fp2["machineId"].(string)
		if m1 == "" || m1 != m2 {
			t.Errorf("uid=%s: machineId 不稳定（%q vs %q）", uid, m1, m2)
		}
		if len(m1) != 36 {
			t.Errorf("uid=%s: machineId=%q 长度应为 36（官方 macOS 是 36 位 IOPlatformUUID）", uid, m1)
		}
		if prev, dup := seen[m1]; dup {
			t.Errorf("machineId 撞车：%s 与 %s 都是 %q（多账号共用设备=关联信号）", uid, prev, m1)
		}
		seen[m1] = uid
	}
}
