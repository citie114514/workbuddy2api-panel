// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/alert"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// Config handler 依赖。
type Config struct {
	Pool               *pool.Pool
	Upstream           *upstream.Client
	APIKey             string // 空 = 不鉴权（静态值；与 Live 同时给出时 Live 优先）
	MaxRotate          int    // 单请求最多换号次数，默认 3
	TimeoutRotateExtra int    // 传输层超时额外加转预算；0 关闭，不扩大 MaxRotate
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m
	// RestrictionHold 11140 风控连坐的全池暂停时长（0 = 只保留「命中即停手」那道闸）。
	// 上游把「账号存在异常行为」判在设备与出口上，不是判在单个账号上：一个请求
	// 顺着轮转能把池子挨个撞一遍（社区复盘：一次任务 7 个号全封）。
	RestrictionHold time.Duration
	// MaxPayloadBytes 入站请求体安全上限（0 = 用内置 64MiB）。护栏不是内容策略：
	// 远高于真实会话（长会话多图 base64 实测几 MB），只挡能把进程读死的上传。
	MaxPayloadBytes int64

	// Panel 管理面板 handler（可选；nil = 不挂载）。挂载在 /panel/ 前缀下，
	// 面板自带 Bearer 鉴权（同一 api_key）与内嵌静态资源，主路由只做转发。
	Panel http.Handler

	// Live 运行期可变配置（面板在线改 api_key / soft_rate / 脱敏开关时立即生效）。
	// nil 时回退静态字段（测试与裸用场景）。
	Live *livecfg.Holder

	// PromptMode "custom"（网关用自有提示词替换 system）/ "passthrough"（透传）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string

	// GlobalEnabled global realm 路由开关（config global.enabled，缺省 true）。
	// handler 侧第三道闸（与 main 注入 auth 开关、upstream.GlobalEnabled 呼应）：
	// false（显式逃生门）时即便 auth realm=global 也不提供 global: 模型名
	// （modelList 不列 global 名单）。
	GlobalEnabled bool

	// ModelsHidden 模型黑名单原始条目（config models.hidden；Live 为 nil 的
	// 裸用/测试路径经 loadLive() 归一化后使用）。
	ModelsHidden []string

	// Usage 逐请求用量记录器（可选；nil = 不记录）。
	// 在 recordAttempt 这一唯一汇聚点调用，因此流式/非流式、成功/失败都会计入，
	// 且与 pool 的每账号累计器同源，两条口径不会漂移。
	Usage *usage.Recorder

	// RequestLog 请求指标与脱敏 JSONL 归档（可选；nil = 不记录）。
	RequestLog *reqlog.Recorder

	// RecordClientInfo 是否在请求日志里记录调用来源（客户端 IP / User-Agent）。
	// 来自 logging.request_client_info（缺省 true）；关闭时 reqlog 事件的来源字段
	// 保持为空，归档与面板都不出现来源信息。
	RecordClientInfo bool

	// DebugLogOutboundBody 4xx 时把出站请求体写进日志（排障开关，缺省 false）。
	// 来自 features.debug_log_outbound_body；Live 非空时以热更新快照为准。
	DebugLogOutboundBody bool

	// Alert 风控信号投递器（可选；nil = 不告警）。构造在 main，配置改动需重启生效。
	Alert *alert.Notifier
}

// loadLive 返回当前运行期快照；Live 为 nil 时用静态字段合成。
func (h *Handler) loadLive() livecfg.Snapshot {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load()
	}
	return livecfg.Snapshot{
		APIKey:               h.cfg.APIKey,
		SoftCooldown:         h.cfg.SoftCooldown,
		RecordClientInfo:     h.cfg.RecordClientInfo,
		DebugLogOutboundBody: h.cfg.DebugLogOutboundBody,
		HiddenModels:         livecfg.NormalizeHiddenModels(h.cfg.ModelsHidden),
	}
}

// softCooldown 返回当前生效的软冷却基数（热改优先，<=0 回退默认）。
func (h *Handler) softCooldown() time.Duration {
	if d := h.loadLive().SoftCooldown; d > 0 {
		return d
	}
	if h.cfg.SoftCooldown > 0 {
		return h.cfg.SoftCooldown
	}
	return 600 * time.Second
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
	// wafIP WAF IP 级拦截状态机（fail-fast，wafip.go）：短窗多号 WAF 403 →
	// 激活期轮转遇 WAF 403 直接终止（不放大请求量）。进程内状态、重启清零。
	wafIP wafIPGate
	// restrict 11140 风控连坐保护（restrict.go）：同一窗口内多个账号被限制
	// 就全池暂停派发。状态在进程内、重启清零——「暂停」是「现在先别打」。
	restrict restrictionGate
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "custom" // 缺省 custom：网关自有提示词
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	// 联网搜索（Anthropic Messages 兼容形态，见 messages.go）。路径刻意不占用
	// /v1/messages——那是 Anthropic 模型对话的标准路径，留给未来的 chat 转发。
	h.mux.HandleFunc("POST /v1/search/messages", h.withAuth(h.messages))
	// Anthropic Messages 对话端点（见 anthropic.go）。上游注释里"留给未来的 chat 转发"
	// 就是这里：/v1/messages 走 Anthropic 入站 → 翻译成 chat/completions → 再翻回
	// Anthropic 响应/事件流，Claude Code 等客户端零改动接入。
	h.mux.HandleFunc("POST /v1/messages", h.withAnthropicAuth(h.anthropicMessages))
	h.mux.HandleFunc("POST /messages", h.withAnthropicAuth(h.anthropicMessages))
	// OpenAI Responses 端点（见 responses.go）：Codex 用 POST /v1/responses。
	// GET 回 426 让 Codex 一次握手就回落 HTTPS，不必空转重连。
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responses))
	h.mux.HandleFunc("GET /v1/responses", h.withAuth(h.responsesWebsocketUnsupported))
	h.mux.HandleFunc("GET /v1/models", h.withDualAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.Panel != nil {
		h.mux.Handle("/panel/", cfg.Panel) // /panel → /panel/ 由 ServeMux 自动重定向
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.cfg.RequestLog != nil && h.tracedRequest(r) {
		trace := &requestTrace{id: reqlog.NewRequestID(), start: time.Now(), path: r.URL.Path}
		if isSearchPath(r.URL.Path) {
			// 搜索不是模型调用，但面板「模型」列空着无法区分这类流量；
			// 用产品名标注（与 chat 的 model 字段并列展示）。
			trace.model = searchToolLabel
		}
		if h.loadLive().RecordClientInfo {
			trace.captureClientInfo(r)
		}
		r = r.WithContext(context.WithValue(r.Context(), requestTraceKey{}, trace))
		obs := &responseObserver{ResponseWriter: w}
		w.Header().Set("X-Request-Id", trace.id)
		h.cfg.RequestLog.Begin()
		defer func() {
			status := obs.status
			if status == 0 {
				status = http.StatusOK
			}
			h.cfg.RequestLog.Record(trace.event(status))
		}()
		h.mux.ServeHTTP(obs, r)
		return
	}
	h.mux.ServeHTTP(w, r)
}

// tracedRequest 判断该请求是否需要请求日志记账（指标 + JSONL 归档）。
//
// 搜索与 chat 共用同一份账本：搜索同样消耗账号额度、同样会因上游/账号问题失败，
// 不记账等于在面板「请求记录」里完全看不见搜索流量（此前正是如此——只挂
// /v1/chat/completions 一个硬编码路径）。
func (h *Handler) tracedRequest(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/v1/chat/completions",
		"/v1/messages", "/messages",
		"/v1/responses":
		return true
	}
	return isSearchPath(r.URL.Path)
}

// isSearchPath 判断是否为搜索端点。
func isSearchPath(path string) bool {
	return path == "/v1/search/messages"
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, h.loadLive().APIKey) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

// withDualAuth 同时认 Bearer（OpenAI 客户端）与 x-api-key（Anthropic SDK）两种携带方式。
//
// 为什么需要：路由挂载不对称。/v1/messages 与 /messages 挂 withAnthropicAuth（认
// x-api-key），而 GET /v1/models 只挂 withAuth（只认 Bearer）。Anthropic 官方 SDK 的
// 模型发现默认带 x-api-key，模型发现又是它的标准启动动作 —— 同一把 key 能调通 messages
// 却列不出模型，只能收到 401。
//
// 实现只在 Authorization 为空时把 x-api-key 提升到标准头，后半段仍走同一套
// httpauth.VerifyBearer：不引入第二条比较路径，常量时间比较的性质不变。
//
// 401 错误体按请求方协议分形：带 x-api-key 或 anthropic-version 的回 Anthropic 口径，
// 否则维持 OpenAI 口径。若图省事直接换用 withAnthropicAuth，用 Bearer 拉模型的 OpenAI
// 客户端在 401 时会收到 Anthropic 形状的错误体，把「密钥不对」误报成「网关坏了」。
func (h *Handler) withDualAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		viaAnthropic := r.Header.Get("x-api-key") != "" || r.Header.Get("anthropic-version") != ""
		if r.Header.Get("Authorization") == "" {
			if k := r.Header.Get("x-api-key"); k != "" {
				r.Header.Set("Authorization", "Bearer "+k)
			}
		}
		if !httpauth.VerifyBearer(r, h.loadLive().APIKey) {
			if viaAnthropic {
				writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "missing or invalid API key")
				return
			}
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// realm_servable 域可服务维度：不改判活语义（存在性探活保持不变），
	// 只新增 CN/global 各自可达性供双域部署运维观察（任一域不可用单独告警）。
	realmServable := map[string]bool{
		"cn":     h.cfg.Pool.ServableForRealm("cn"),
		"global": h.cfg.Pool.ServableForRealm("global"),
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy":        healthy,
		"total":          total,
		"service":        ServiceName,
		"realm_servable": realmServable,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	// cost_explore 探索台账（issue #136 §5 可观测性）：累计探索事件数 + 各
	// (域, 模型) 的最近探索时刻（键 "realm|model"）。与 accounts[].model_costs
	// 行对照即可读出「探索→毕业」全链路（单一事实来源，不做双表示）。零回归只增键。
	exploreEvents, exploreLast := h.cfg.Pool.CostExploreStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       h.cfg.Pool.List(),
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
		// realm_totals 按域分组的计数汇总（双 realm 并存时运维一眼看到各域可用性）：
		// 只新增字段，既有 total/healthy/cooling/disabled/in_flight_full 汇总键不变（零回归）。
		"realm_totals": map[string]map[string]int{
			"cn":     countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("cn")),
			"global": countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("global")),
		},
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
		// model_locks 当前有未过期模型级限流的 (域, 模型) 全清单：账号池视图回答
		// 「哪些号不能用」，本键回答「哪些模型不能用、锁了几个号、还要锁多久」。
		// 与 ModelBlocked（请求失败时的单模型判定）互补；无锁时为 null。零回归只增键。
		"model_locks": h.cfg.Pool.ModelLockView(),
		// credit_floor 生效的积分保底值（0 = 关闭）。与 accounts[].credits +
		// model_costs 对照即可判定「某号为何对某模型不出票」。零值也显式写出
		// （运维口径：缺失会让人误以为没记录）。
		"credit_floor": h.cfg.Pool.CreditFloor(),
		// cost_explore 事件与 per-model 时间戳（时间值由 encoding/json 写 RFC3339）。
		"cost_explore": map[string]any{
			"events_total": exploreEvents,
			"per_model":    exploreLast,
		},
	})
}

// countsMapFrom 把 CountsDetailed 五元组打包成 /status 的域分组建模。
func countsMapFrom(total, healthy, cooling, disabled, inFlightFull int) map[string]int {
	return map[string]int{
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
	}
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	// dynamicModelsTTL 模型目录缓存时长。曾是 1h；缩到 10min 对齐「面板实时、
	// API 缓存」的漂移痛点（PR #38 报告）：目录新增模型时面板立即可见，公开
	// /v1/models 最多滞后一个 TTL。再短就不值得——每次失效都是 2 次上游探测。
	dynamicModelsTTL        = 10 * time.Minute
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：纯动态（缓存 10min），失败/无号返回空列表（无静态兜底——
// 拉不出目录即意味着上游不可用，假名单只会让客户端选到 11102 的模型）。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// fmtCreditsPrefix 从上游 credits 原文提取倍率并格式化为 "[x0.05 credit]"。
// 上游格式不统一："x0.05 credits" / "x0.29" / "x0.00 credits" 等，
// 统一提取 x数字 部分，去 "credits" 后缀。
func fmtCreditsPrefix(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "credits")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "[" + s + " credit]"
}

// applyModelInfoFields 把上游模型对象全字段（ModelInfo）按「空值省略」写出规则
// 合入 /v1/models 条目：name/description/credits/tags/vendor/能力旗标/
// max_allowed_size/reasoning_effort/reasoning_summary。CN 动态分支与 global
// 探测命中分支共用（两域模型对象同构），保证输出字段集一致。
// 不覆盖 id/object/created/owned_by 及调用方先前写好的基础字段；上游未下发的
// 字段（零值）整体省略——不编造。
func applyModelInfoFields(entry map[string]any, mi upstream.ModelInfo) map[string]any {
	if mi.Name != "" {
		entry["name"] = mi.Name
	}
	if mi.Description != "" {
		// 积分倍率前缀：从 "x0.05 credits" / "x0.29" 等格式提取纯数字，
		// 统一为 "[x0.05 credit]" 前缀拼入 description，方便下游面板直接展示。
		if mi.Credits != "" {
			entry["description"] = fmtCreditsPrefix(mi.Credits) + " " + mi.Description
		} else {
			entry["description"] = mi.Description // descriptionZh 中文描述
		}
	}
	if mi.Credits != "" {
		entry["credits"] = mi.Credits // 积分倍率原文（如 "x0.05"），仅展示
	}
	if len(mi.Tags) > 0 {
		entry["tags"] = mi.Tags
	}
	if mi.Vendor != "" {
		entry["vendor"] = mi.Vendor
	}
	if mi.IsDefault {
		entry["is_default"] = true
	}
	if mi.SupportsImages {
		entry["supports_images"] = true // 多模态能力透出
	}
	if mi.SupportsReasoning {
		entry["supports_reasoning"] = true
		if mi.CanDisableThinking {
			entry["can_disable_thinking"] = true
		}
	}
	if mi.SupportsToolCall {
		entry["supports_tool_call"] = true
	}
	if mi.OnlyReasoning {
		entry["only_reasoning"] = true
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	if mi.ReasoningEffort != "" {
		entry["reasoning_effort"] = mi.ReasoningEffort
	}
	if mi.ReasoningSummary != "" {
		entry["reasoning_summary"] = mi.ReasoningSummary
	}
	return entry
}

// modelList 模型列表：CN 模型输出统一加 "cn:" 前缀（gateway 路由协议，与 resolveModel
// 对称）；global.enabled=true 时追加 global: 前缀的国际版名单。
// 纯动态：动态拉取失败/无号 → 该域空列表，无静态兜底。
func (h *Handler) modelList() []map[string]any {
	out := make([]map[string]any, 0)
	// models.hidden 黑名单：快照外提，避免逐条目重复 Load。
	hidden := h.loadLive().HiddenModels
	for _, mi := range h.fetchDynamicModels() {
		if _, ok := hidden["cn:"+mi.ID]; ok {
			continue
		}
		entry := map[string]any{
			"id":       "cn:" + mi.ID,
			"object":   "model",
			"created":  1753600000,
			"owned_by": "workbuddy",
		}
		// context_length / max_output_tokens 四级查找（upstream.model_catalog）：
		// 上游动态值（maxInputTokens/maxOutputTokens）权威 → 静态种子表 →
		// model.json 本地缓存 → models.dev 按需拉取（异步不阻塞本次响应，拉到后
		// 写 model.json 供下次命中）→ 1M 兜底 / max_output_tokens 省略。
		// 上游零值不再透出假 131072（误导 Codex/ZCode 等按 context_length 提前
		// 截断、白白丢上下文）。
		entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, h.cfg.Upstream.HTTP)
		if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, h.cfg.Upstream.HTTP); ok {
			entry["max_output_tokens"] = mo
		}
		// 上游模型对象全字段透出（name/描述/标签/倍率/能力旗标等，空值省略）。
		entry = applyModelInfoFields(entry, mi)
		// effort 能力透出——远端 supportedEfforts 权威，缺失落到 CN 静态兜底表
		// （客户端可发现档位，不再盲传）。无档位 → 省略字段。
		if efforts, def := upstream.EffortListing("cn", mi.ID, mi.Efforts, mi.DefaultEffort); efforts != nil {
			entry["reasoning_supported_efforts"] = efforts
			if def != "" {
				entry["reasoning_default_effort"] = def
			}
		}
		out = append(out, entry)
	}
	// global 模型名单：仅 GlobalEnabled=true 时列出（逃生门）。
	// 名单 = 纯动态探测结果（fetchGlobalModels，失败/无号 → 空）。
	if h.cfg.GlobalEnabled {
		// global 域 effort 能力三级查找：探测下发桶（权威）→ 静态兜底表 → 省略。
		// 先 fetchGlobalModels（内部探测并落 effort 桶），再按 id 取快照。
		globalIDs, globalAccount := h.fetchGlobalModels()
		// 探测对象形态的全字段条目（与 fetchGlobalModels 共享同一次探测缓存）：
		// 命中 id 才透出富字段；窄表/失败 → nil，按裸 ID 条目输出（不编造字段）。
		// globalAccount 为 nil（无 global 号）时返回 nil，跳过富字段映射。
		globalInfos := map[string]upstream.ModelInfo{}
		for _, mi := range h.cfg.Upstream.FetchGlobalModelInfos(globalAccount) {
			globalInfos[mi.ID] = mi
		}
		globalEfforts, globalDefaults := h.cfg.Upstream.GlobalEffortSnapshot()
		for _, id := range globalIDs {
			if _, ok := hidden["global:"+id]; ok {
				continue
			}
			entry := map[string]any{
				"id":       "global:" + id,
				"object":   "model",
				"created":  1753600000,
				"owned_by": "workbuddy",
			}
			// context_length / max_output_tokens 四级查找（与 CN 动态分支同口径）。
			var remoteCtx, remoteOut int64
			if mi, ok := globalInfos[id]; ok {
				entry = applyModelInfoFields(entry, mi)
				remoteCtx, remoteOut = mi.ContextWindow, mi.MaxTokens
			}
			entry["context_length"] = upstream.ContextWindowListingV4(id, remoteCtx, h.cfg.Upstream.HTTP)
			if mo, ok := upstream.MaxOutputTokensListingV4(id, remoteOut, h.cfg.Upstream.HTTP); ok {
				entry["max_output_tokens"] = mo
			}
			if efforts, def := upstream.EffortListing("global", id, globalEfforts[id], globalDefaults[id]); efforts != nil {
				entry["reasoning_supported_efforts"] = efforts
				if def != "" {
					entry["reasoning_default_effort"] = def
				}
			}
			out = append(out, entry)
		}
	}
	return out
}

// fetchGlobalModels 返回 global 模型名单（纯动态探测结果）及被探测账号。
// 缓存/失败回落封在 upstream.FetchGlobalModels（内部 1h + 5min 负缓存）。
// 本方法只负责"何时探测"：池中无 global 账号 → 空名单 + nil 账号（零上游调用）。
// 返回的 acct 供调用方在同一账号上取富 ModelInfo（FetchGlobalModelInfos 与
// FetchGlobalModels 共享缓存，不会触发第二次上游探测）。
// GlobalEnabled=false 时 modelList 已不进入本分支（逃生门在调用方 gate）。
func (h *Handler) fetchGlobalModels() ([]string, *auth.Auth) {
	acct := h.cfg.Pool.PickExcludingForRealm(nil, "", "global")
	if acct == nil {
		return nil, nil
	}
	return h.cfg.Upstream.FetchGlobalModels(acct), acct
}

// fetchDynamicModels 从第一个可用 CN 账号拉模型列表（含 contextWindow/maxTokens），
// 缓存 10min。
// 选号与 /panel/api/models 完全同口径（AvailableUIDsForRealm("cn") 首个 + AuthByUID），
// 而非 Pool.Pick()：Pick 无 realm 过滤，混合池里可能选中 global 号去打 CN 端点，
// 表现为偶发失败/面板与 /v1/models 两套目录（PR #38 报告并给出的选号修复）。
// 缓存 + 5min 负缓存按既有语义**保留**（#38 原案整体删除缓存被拒）：公开端点逐请求
// 实时拉取 = 每次 2 个上游探测，客户端周期性刷新模型列表会持续打上游；上游故障时
// 无冷却窗口，客户端重试即放大请求量——负缓存正是为此设计（见 handler_test 吸收
// 上游 9832283 的注释）；且 cachedModelsSnapshot（gateway_hint 判定）依赖缓存写入。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	uids := h.cfg.Pool.AvailableUIDsForRealm("cn")
	if len(uids) == 0 {
		return nil
	}
	acct := h.cfg.Pool.AuthByUID(uids[0])
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败只进负缓存（5min lastFail），不 NoteError：NoteError 喂的是 chat
		// 熔断器，models 端点偶发 5xx 跨界惩罚 chat 通道健康的账号；
		// models 拉取失败 ≠ 账号 chat 不可用。
		// 只在真正打上游失败时记一行（负缓存期内前面已直接 return，不会刷屏）：
		// 此前这里静默，CN 域不可达时 /v1/models 只剩 global 模型却毫无线索。
		if err == nil {
			err = fmt.Errorf("models api returned empty list")
		}
		log.Printf("WARN: [server] /v1/models: CN 模型目录拉取失败（%s 内不再重试，cn: 模型暂不列出）: %v",
			modelsFetchFailCooldown, err)
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

// cachedModelsSnapshot 只读模型目录缓存（TTL 内快照）；缓存冷/空 → nil。
// 不发起任何上游调用（hint 判定用：错误路径加一次 FetchModels 网络调用既拖慢
// 错误响应、又污染上游调用语义）。
func cachedModelsSnapshot() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	if len(dynamicModelsCache.ids) == 0 || time.Since(dynamicModelsCache.fetched) >= dynamicModelsTTL {
		return nil
	}
	return dynamicModelsCache.ids
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// 客户端 IP 提取（按请求传递到 ChatStream，不透传时 upstream 侧忽略）；
	// 消除早年共享字段方案的并发交叉污染（issue：ClientIP 竞态）。
	clientIP := upstream.ExtractClientIP(r)
	// 请求体无大小上限（max_body_mb 已移除，对齐上游）：完整读入，超限类问题交由
	// 上游自然返回错误（其响应经既有错误分类链路透出，信息量更大）。#41 的截断
	// 防御语义保留在读错误路径——移除预拦截后，截断只可能来自客户端自己断流，
	// 读 body 出错就地 400，不把半截 JSON 喂上游 unmarshal 报 unexpected EOF 冤枉罚号。
	body, berr := h.readBody(w, r)
	if berr != nil {
		h.writeBodyError(w, berr, false)
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// 工具名名单：上游在没有 tools 的请求里会把工具调用吐成原生标记文本
	// （见 upstream/dsml.go），修复层用它做严格判定。名单来自「本请求声明的 tools」
	// 与「会话历史里出现过的工具名」两个来源——实测 tools 声明会在中转环节丢失，
	// 只认前者会让修复层在最需要它的场景下失效。
	// 在提示词/模型名改写之前取（那些改写不动 tools / messages[].tool_calls 子树）。
	declaredTools := upstream.ToolNameAllowlist(body)

	// realm 前缀解析（D6）：model 名可能带 "[realm:]" 前缀。剥出 realm + bareModel，
	// bareModel 用于选号/粘性/出站 body 重写（前缀是网关侧路由协议，上游只认裸名）。
	// 裸名 → ("cn", 原串)，CN 现状零回归。
	realm, bareModel, realmExplicit := resolveModelDetail(peek.Model)
	modelRate := ""
	if h.cfg.Upstream != nil {
		modelRate = h.cfg.Upstream.ModelRate(realm, bareModel)
	}
	// 跨域回退（issue #135）：只有裸模型名 + 显式开启 pool.cross_domain_fallback 时生效。
	// 语义 = 「裸名不绑定单一域」：首选域（裸名默认 cn）选不到可用号（账号级冷却/熔断/
	// 在途占满，或该域账号对这个模型 11102 后进入模型级负缓存）时，改用另一域再选一次；
	// 显式 cn:/global: 前缀保持原语义，永不回退。
	crossRealm := !realmExplicit && h.cfg.Pool.CrossDomainFallback()

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	if tr := requestTraceFrom(r); tr != nil {
		tr.stat = st
		// 来源在 ServeHTTP 入口采集（此时才知道开关与请求头），此处转交给统计对象，
		// 让 stdout 流水行与归档事件共用同一份来源值，两处不会漂移。
		st.clientIP, st.userAgent = tr.clientIP, tr.userAgent
	}
	defer st.done()

	// 11140 风控连坐暂停（见 restrict.go）：同一窗口里已经有两个不同账号被上游判
	// 「存在异常行为」，说明拦的是设备/出口而不是某个号。此期间**不派发**——快速失败，
	// 不拿池子剩下的健康号去试，否则只会把封禁面继续扩大。
	if d := h.restrict.remaining(); d > 0 {
		secs := int(d.Round(time.Second) / time.Second)
		// Retry-After 让客户端知道等多久，而不是自己一秒重试一次继续撞风控。
		w.Header().Set("Retry-After", retryAfterHeader(d))
		msg := fmt.Sprintf("upstream restricted several accounts for unusual activity (403 / code 11140); dispatch paused for %ds to avoid further bans — do not rotate accounts or retry in a loop", secs)
		downgraded := writeOpenAIErrorHint(w, http.StatusServiceUnavailable, "account_restricted_hold", msg,
			"wait for the restriction hold to expire or appeal to WorkBuddy; rotating accounts during a restriction wave gets more of them banned")
		st.status = http.StatusServiceUnavailable
		st.outcome = reqlog.OutcomeHTTPError
		recordErrorOutcome(st, http.StatusServiceUnavailable, downgraded)
		return
	}

	// models.hidden 黑名单：点名调用直接拒绝，不打上游、不占选号。
	// 与 /v1/models 的列表过滤对称（只藏列表不够，知道名字仍可硬调）。
	if _, hide := h.loadLive().HiddenModels[realm+":"+bareModel]; hide {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "model_unavailable",
			"model is hidden by gateway configuration, try another model", "")
		st.status = http.StatusBadRequest
		st.outcome = reqlog.OutcomeHTTPError
		return
	}

	// 模型级挂死熔断：该 (域, 模型) 近期连续停滞且仍在熔断窗口内 → 入口直接失败快返，
	// 不再逐号白等一整个超时。带 Retry-After，客户端等待后重试即可。
	if until, blocked := h.cfg.Pool.ModelHangBlocked(realm, bareModel); blocked {
		wait := time.Until(until)
		if wait < time.Second {
			wait = time.Second
		}
		// 不在这里累计观测：熔断期内被拒的请求不是新的停滞证据，累计会把窗口
		// 无限续期，熔断就永远不会关闭。
		w.Header().Set("Retry-After", retryAfterHeader(wait))
		writeOpenAIErrorHint(w, http.StatusServiceUnavailable, "model_temporarily_down",
			"model is temporarily unresponsive upstream, retry after the indicated delay", "")
		st.status = http.StatusServiceUnavailable
		st.outcome = reqlog.OutcomeHTTPError
		log.Printf("WARN: [server] model hang circuit open model=%s realm=%s: reject request until %s",
			bareModel, realm, until.Format(time.RFC3339))
		return
	}

	tried := map[string]bool{}
	var lastErr error
	// modelBlock 记录本次是否因「模型级冷却」而选不到号（下方 acct==nil 分支填充）。
	// 默认零值 Blocked=false = 按既有的"没有可用账号"口径报错。
	var modelBlock pool.ModelBlockStatus
	// fallbackTried 模型付费回退只做一次：免费额度型 6004 → 付费兄弟型号
	// （如 global:deepseek-v4.1-flash → -sg）。回退型号再阻塞就走既有 model_blocked
	// 错误口径，不做 "-sg-sg" 链式递归。
	fallbackTried := false
	// promptTooLongRepairs 11115 可修复重发的次数（见下方 ErrPromptTooLong 分支）。
	// 上游的越界口径是 prompt_tokens + 输出预留 > 模型窗口，预留取决于请求体有没有
	// 显式 max_tokens；不带 max_tokens 的客户端会凭空背上模型默认预留而被顶成 400。
	promptTooLongRepairs := 0

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	// ExtractKey 与粘性开关解耦（issue #35 侧）：关闭粘性时会话头族的聚合主键仍按
	// 会话级（RequestIDForKey(sessKey)），不悄悄退化成轮级——提取本身与粘性无关。
	sessKey := session.ExtractKey(body)
	stickyUID := ""
	if h.cfg.Session != nil && sessKey != "" {
		// 按模型解析：绑定号在**当前模型**被 6004 限额时视为不可用 → 重新分配，
		// 而不是钉在限额号上反复失败（"限额后换不动号"的正解）。
		resolve := h.cfg.Session.ResolveForModel
		if h.cfg.Pool.StrategyForRealm(realm) != pool.WeightedRandom {
			resolve = h.cfg.Session.LookupForModel
		}
		if uid, ok := resolve(sessKey, peek.Model); ok {
			stickyUID = uid
		}
	}

	// 轮级聚合键：按 body 里最后一条 user 消息派生（同轮内所有上游调用同键，
	// 换 user 消息换键）。#170 起带会话键的客户端也统一走轮级（对齐官方桌面 CLI
	// 的 X-Conversation-Request-ID 轮级语义——TraceStartHook 每次 USER_PROMPT_SUBMIT
	// 清空重生成），故不再限 sessKey=="" 才计算；sessKey 由下方派生处以复合键方式
	// 入键（防不同会话同轮文本互撞）。
	// 必须在下方 prompt.Rewrite 之前取——改写会动 messages 内容，之后取会让键漂移。
	turnKey := session.TurnKey(body)

	// gateway_hint 判定所需的请求形态（image_url part）：在改写前取（与 turnKey
	// 同理）。11133「模型不支持图片」指向的前提。
	reqHasImage := hasImagePart(body)

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}
	// failKeepBinding 供**请求级错误**使用：只释放在途租约，**不解绑会话粘性**。
	//
	// 这些错误与账号无关——同一个 body 换任何号都是同样的结果（内容拦截 / prompt 超长 /
	// 图片无效 / 参数错 / 工具序列不匹配），账号本身仍健康，它的前缀缓存也还热着。
	// 一旦解绑，客户端下次重试会被分配到另一个号上，而缓存按账号隔离：十万到几十万
	// token 的前缀要整段重算。表现就是「报错一次之后缓存一直热不起来」——首字延迟从
	// 秒级涨到十几秒，且报错越密集越会连环换号，每一跳都是全量 miss。
	// 账号级故障（401/403/429 冷却、熔断、连败、传输层失败）仍走 fail 解绑，那些确实该换号。
	failKeepBinding := func() {
		releaseHeld()
	}
	// ttfb 首 token 等待（仅流式有观测；非流式传 0 = 无观测，速率不扣减）。
	// 显式入参而不是读 st.ttfb：后者在流式分支里是**调用之后**才赋值的，
	// 靠顺序传递会让将来重排代码时静默把速率算回旧的错口径。
	recordAttempt := func(uid string, delta pool.TokenUsageDelta, credit float64, hasCredit bool, started time.Time, ttfb time.Duration) {
		st.attempts++
		if delta.HasPromptTokens {
			st.promptTokens = delta.PromptTokens
		}
		if delta.HasCompletionTokens {
			st.completionTokens = delta.CompletionTokens
		}
		if delta.HasTotalTokens {
			st.totalTokens = delta.TotalTokens
		} else if delta.HasPromptTokens || delta.HasCompletionTokens {
			st.totalTokens = st.promptTokens + st.completionTokens
		}
		delta.Model = bareModel
		if delta.Model == "" {
			delta.Model = peek.Model
		}
		latency := time.Since(started)
		latencyMs := latency.Milliseconds()
		if latencyMs < 1 {
			latencyMs = 1
		}
		delta.HasLatencyMs = true
		delta.LatencyMs = latencyMs

		// HasCompletionTokens 是「本次有没有 token 观测」的独立判断，与速率怎么算
		// 无关，所以留在调用点；速率本身的边界（TTFB 缺失/超界）收在 tokensPerSecond。
		if delta.HasCompletionTokens {
			if tps, ok := tokensPerSecond(delta.CompletionTokens,
				time.Duration(latencyMs)*time.Millisecond, ttfb); ok {
				delta.HasTokensPerSecond = true
				delta.TokensPerSecond = tps
			}
		}
		h.cfg.Pool.RecordTokenUsage(uid, delta)

		// 用量时序记录。ok 以「上游是否给了 usage」判定：空 delta 意味着这次尝试
		// 没拿到任何 token 统计（传输错误 / >=400 / 解析失败），计为失败尝试。
		// 失败也计入请求数——否则重试放大在「用量」视图里看不见。
		if h.cfg.Usage != nil {
			realm := "cn"
			if a, ok := h.cfg.Pool.Status(uid); ok && a.Realm != "" {
				realm = a.Realm
			}
			h.cfg.Usage.Add(time.Now(), realm, uid, delta.Model, usage.Delta{
				PromptTokens:     delta.PromptTokens,
				HasPromptTokens:  delta.HasPromptTokens,
				CompletionTokens: delta.CompletionTokens,
				HasCompletion:    delta.HasCompletionTokens,
				TotalTokens:      delta.TotalTokens,
				HasTotal:         delta.HasTotalTokens,
				Credit:           credit,
				HasCredit:        hasCredit,
				HasCacheTokens:   st.hasCache,
				CacheHitTokens:   st.cacheHit,
				CacheMissTokens:  st.cacheMiss,
				ModelRate:        modelRate,
				LatencyMs:        delta.LatencyMs,
				HasLatency:       delta.HasLatencyMs,
				TokensPerSecond:  delta.TokensPerSecond,
				HasTPS:           delta.HasTokensPerSecond,
			}, delta.HasTotalTokens || delta.HasCompletionTokens || delta.HasPromptTokens)
		}
	}

	// 系统提示词改写（出站前、轮转前；每个请求一次）。
	//   - custom：用自有提示词替换客户端 system/developer（从源头消灭 system 指纹误报）。
	//   - append：开头连续 system/developer 块后插自有提示词，既有消息逐字不动
	//     （客户端项目规范/工具约定与网关提示词并用，issue #129）。
	//   - passthrough + 降级期：换 Degraded 中性提示词直达，不再先撞 400。
	//   - passthrough / append 非降级期：透传客户端原始 system（append 则再插一条网关 system）。
	// 降级裁决：append 在降级期退化为 replace（Rewrite(Degraded)）——append 带
	// 指纹原文重试是确定性再撞墙，replace 是一次性最小抢救（issue #129 设计 §4）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "append" && h.cfg.PromptText != "" && !h.degrade.Active() {
		body = prompt.Append(body, h.cfg.PromptText)
	} else if (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}

	// outbound model 名重写为 bareModel（D6）：realm 前缀是网关侧路由协议，
	// 上游不认前缀（global 账号也请求裸模型名）。裸名时 bareModel==peek.Model 恒等。
	if bareModel != peek.Model {
		body = rewriteModel(body, bareModel)
	}

	// 会话头族（issue #35）：后台按 X-Conversation-Request-ID（对话轮级）聚合请求，
	// 官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID。此处**轮转
	// 循环外**生成一次，循环内每次出站原样复用 → 换号/重试/降级全部同 ID，后台不再
	// 碎片化（此前网关一个都不发，上游按 HTTP 请求逐条记账，同一对话几十上百个
	// RequestID）。
	//   - conversationID：body 提取（透传客户端原值，缺省空串——不伪造）；
	//   - conversationRequestID：入站 X-Conversation-Request-ID 透传优先，否则按
	//     粘性 key 进程内稳定生成；粘性 key 也空时走轮级兜底（TurnKey/TurnRequestID），
	//     无 user 消息时退化成本请求级随机——轮转内捕获一次即共享；
	//   - messageID 在 ChatHeaders 内每条消息生成（消息级独立，无需外部可见）。
	// ConversationID passes the client dialog id through. Clients that have none
	// (Anthropic / Claude Code traffic) fall back to the session key, otherwise
	// the upstream prompt_cache_key degrades into one constant per account and
	// every client on that account ends up sharing a single, constantly evicted
	// prefix-cache slot.
	convID := session.ResolveConversationID(body)
	if convID == "" {
		convID = sessKey
	}
	chatMeta := upstream.ChatMeta{ConversationID: convID}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = v
	} else if turnKey != "" && sessKey != "" {
		// 轮级复合键：sessKey 入键防跨会话同轮文本互撞（#170 统一轮级）。
		chatMeta.ConversationRequestID = session.TurnRequestID(sessKey + ":" + turnKey)
	} else if turnKey != "" {
		// 无会话键客户端：纯轮级键（既有兜底语义不变，存量会话键值零漂移）。
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	} else if sessKey != "" {
		// 残留空态兜底（无 user 消息/无可签名内容）：会话级聚合，好于请求级随机。
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		// 无会话键也无轮级键：请求级随机（轮转内捕获一次即共享）。
		chatMeta.ConversationRequestID = session.TurnRequestID("")
	}
	chatMeta.TraceID = r.Header.Get("X-Trace-ID")

	// 4xx 排障日志：请求结束时把**最后一次实际发出的**出站 body 写进日志（仅当
	// features.debug_log_outbound_body 打开且最终状态是 4xx）。挂在这里而不是每个
	// 400 出口，是因为 4xx 有六七个返回点，逐个加必然漏。
	if h.loadLive().DebugLogOutboundBody {
		defer func() { h.logOutboundOn4xx(st, body) }()
	}

	timeoutRotateExtra := h.cfg.TimeoutRotateExtra
	// stickyOverflow 表示本次只是从满载的绑定号临时溢出；成功后不改绑。
	stickyOverflow := false
	// preserveCapacityBinding 表示绑定号曾命中 14003 容量短窗口或 6020/6021 渠道
	// 排队（都只说明「此刻容量不够」，账号本身健康）；本轮即使用其他账号临时代理
	// 成功，也保留原会话绑定，待窗口过去后回到原账号。
	preserveCapacityBinding := false
	// 429 软限流不消耗轮换预算（见下方 ErrSoftRate 的 i-- 豁免）：
	//   - attempt 是真实尝试次数，只用于 rotateBackoff 的退避阶梯——豁免掉的
	//     429 也要逐级退避，不能因为 i 不涨就原地高频重试同一个出口；
	//   - softRateSwaps 是豁免次数，用「账号总数 + MaxRotate」作硬上限兜底：
	//     每个账号最多被 429 一次就带上冷却标记、下一次 Pick 自然排除它，理论上
	//     换到没有未冷却号就会收敛；但并发下 Pick 可能把刚冷却的号又捞回来，
	//     没有硬上限会原地打转。
	poolTotal, _, _, _, _ := h.cfg.Pool.CountsDetailed()
	maxSoftRateSwaps := poolTotal + h.cfg.MaxRotate
	softRateSwaps := 0
	// 流式请求从这一刻起开始保活：选号、建连、上游排队与预填全部算在内。
	// 这段是客户端最没耐心的窗口，而流内保活（计时起点是响应头到达）覆盖不到。
	// 详见 streamKeepAlive 的注释与 upstream.DefaultStreamPingInterval。
	var keepAlive *streamKeepAlive
	if peek.Stream {
		keepAlive = newStreamKeepAlive(w)
		w = keepAlive
		keepAlive.start(upstream.DefaultStreamPingInterval)
		defer keepAlive.stop()
	}
	attempt := 0
	for i := 0; i < h.cfg.MaxRotate; i++ {
		attempt++
		stickyOverflow = false
		// 选号：粘性号优先，临时满载与持久不可用分别处理。
		var acct *auth.Auth
		// 14003 保留的是跨请求绑定，不是让本轮反复打同一个繁忙账号。
		// 命中过容量错误的粘性号在本轮后续选号中与普通 tried 账号同样排除。
		if stickyUID != "" && !(preserveCapacityBinding && tried[stickyUID]) {
			var why pool.StickyPickResult
			acct, why = h.cfg.Pool.PickStickyForModel(stickyUID, bareModel)
			switch {
			case acct == nil && why == pool.StickyBusy:
				stickyOverflow = true
			case acct == nil:
				unbindSticky()
			case realm != "" && acct.Realm() != realm && !crossRealm:
				// 跨域回退开启且请求为裸名时，允许绑定号落在任一域。
				unbindSticky()
				acct = nil
			}
		}
		// pickRealm 本轮实际尝试的域（裸名时为 ""，即不限域）。提到循环作用域，
		// 供下方 acct==nil 的模型付费回退判断使用：回退必须落在真正被阻塞的那个域上。
		pickRealm := realm
		if acct == nil {
			// 模型感知 + realm 感知选号：模型非空时启用 6004 模型级冷却豁免
			// （healthyForModel），realm 谓词过滤跨域账号。
			if crossRealm && h.cfg.Pool.ServableForModelRealm(bareModel, realm) == 0 &&
				h.cfg.Pool.ServableForModelRealm(bareModel, otherRealm(realm)) > 0 {
				// 首选域此刻没有真正可服务的号（11102 模型级负缓存 / 账号级冷却 /
				// 在途占满），而另一域有 → 直接换域选号（issue #135）。先看健康度再选，
				// 避免拿全冷却兜底捞回来的旧域账号白撞一次上游。
				pickRealm = otherRealm(realm)
			}
			acct = h.cfg.Pool.PickExcludingForRealm(tried, bareModel, pickRealm)
			if acct == nil && crossRealm {
				// 该域选不到（并发抢名额/轮换耗尽/另一域刚冷却完）→ 另一域再试一次，
				// 保持尽力而为语义；tried 已记录失败号，轮转仍由 MaxRotate 封顶。
				acct = h.cfg.Pool.PickExcludingForRealm(tried, bareModel, otherRealm(pickRealm))
			}
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			// 记下"是不是模型级阻塞"。选号返回 nil 有两种完全不同的成因：
			//   (a) 池子真的没有可用号（账号级冷却/在途占满/积分保底）；
			//   (b) 号都在，但每个号都对这个模型处于模型级冷却（11102/11158/6004）。
			// 两者此前都报 no_healthy_account，导致「模型不可用」被读成「号全挂了」。
			// 在这里取一次快照，供下方错误构造区分（issue #102 附带发现 1）。
			modelBlock = h.cfg.Pool.ModelBlockedForRealm(bareModel, realm)
			// 模型付费回退（默认开启）：模型级**配额**阻塞（6004/6000/6008 免费额度
			// 用尽）且存在付费兄弟型号时，对齐官方客户端改用回退型号重选一次 ——
			// global:deepseek-v4.1-flash 被全池 6004 锁住时官方客户端就是切到
			// -sg（x0.03）继续用的。只对配额类回退：瞬时容量窗口（Capacity）等一会儿
			// 就自愈，换付费型号是白花钱。回退型号也阻塞 → 走既有 model_blocked 口径。
			if modelBlock.Blocked && modelBlock.QuotaOnly && !fallbackTried {
				fbRealm := pickRealm
				fb, fbOK := h.cfg.Pool.ModelFallbackTarget(fbRealm, bareModel)
				if !fbOK && crossRealm {
					// 首选域没有付费兄弟型号 → 另一域再找一次（跨域回退开启时才做）。
					if t, ok2 := h.cfg.Pool.ModelFallbackTarget(otherRealm(fbRealm), bareModel); ok2 {
						fb, fbOK, fbRealm = t, true, otherRealm(fbRealm)
					}
				}
					if fbOK && h.cfg.Pool.ServableForModelRealm(fb, fbRealm) > 0 {
					log.Printf("INFO: [server] model fallback realm=%q model=%s -> %s (all accounts model-blocked: %s)",
						fbRealm, bareModel, fb, strings.TrimSpace(modelBlock.Reason))
					fallbackTried = true
					body = rewriteModel(body, fb)
					bareModel = fb
					st.model = fb // 流水行展示实际服务的模型，而不是请求里写的那个
					if h.cfg.Upstream != nil {
						modelRate = h.cfg.Upstream.ModelRate(fbRealm, fb)
					}
					// 主模型的失败记录清空：6004 罚的是模型不是号，回退型号要全池重选。
					tried = map[string]bool{}
					modelBlock = pool.ModelBlockStatus{}
					continue
				}
			}
			break
		}
		st.uid = acct.UID
		// 同步昵称：请求流水行只写 uid8 时无法直观看是哪一号，昵称随本次选号带入日志行。
		st.nick = acct.Nickname
		if crossRealm && h.cfg.Upstream != nil && acct.Realm() != realm {
			// 跨域回退命中：按实际服务域取积分倍率，用量/账本记的是真实域口径，
			// 而不是请求前缀声明的域。
			modelRate = h.cfg.Upstream.ModelRate(acct.Realm(), bareModel)
		}
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 压线抢不到粘性号名额也属于临时满载，保留原绑定。
			if stickyUID != "" && acct.UID == stickyUID {
				stickyOverflow = true
			}
			if !rotateBackoff(i, r.Context()) {
				// 客户端已断连：换号重试无意义，终止轮转走末端错误透传。
				break
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				// RT 失效但 AT 仍然有效：这次请求照发（不罚号、不换号）。把这种号
				// 判死会禁掉一个完全健康的账号——refresh 归 refresh，可用性看 AT。
				if upstream.SessionDeadButUsable(acct) {
					log.Printf("INFO: [server] chat acct=%s: refresh dead but access token still valid — send with current access token",
						logfmt.Label(acct.UID, acct.Nickname))
				} else {
					lastErr = err
					var ue *upstream.Error
					if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
						h.cfg.Pool.Disable(acct.UID, "refresh session dead")
					} else {
						h.cfg.Pool.NoteError(acct.UID)
					}
					fail(acct.UID)
					if !rotateBackoff(i, r.Context()) {
						break // ctx 取消：终止轮转（refresh 失败换号退避）
					}
					continue
				}
			} else if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("ERR: [server] chat refresh acct=%s: save auth failed: %v", logfmt.Label(acct.UID, acct.Nickname), err)
			}
		}

		// 客户端 IP 按请求传递（PassthroughIP 开启时注入；消除共享字段竞态）。
		attemptStarted := time.Now()
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, body, clientIP, chatMeta)
		// 分类信封一次成型：upstream 已在错误路径返回 *upstream.Error（Kind +
		// Retry-After 头解析）。传输层错误（非 *Error）走抖动换号分支；防御分支
		// （terr 为 nil 但 status>=400，如 ErrNone 兜底）回落本地 Classify，双保险。
		var uerr *upstream.Error
		if errors.As(terr, &uerr) {
			status = uerr.Status
		}
		if uerr == nil && terr != nil {
			// 上游超时 / 停滞：不罚号，按独立小预算有界加转。
			//
			// 超时不是账号的问题：只使用 timeoutRotateExtra 的小额预算换号，
			// 不扩大 MaxRotate，也不喂连败计数或账号冷却。
			//
			// 判定三态：net.Error.Timeout()（ResponseHeaderTimeout / Client.Timeout）、
			// 显式 deadline（DeadlineExceeded / os.ErrDeadlineExceeded）、以及
			// **客户端仍在但 ctx 被取消**——那只能是我们自己的空闲看门狗掐的流，
			// 也就是上游停滞。客户端主动断连时 r.Context() 已取消，走下面的抖动分支。
			if isUpstreamTimeout(terr, r.Context().Err() != nil) {
				// 模型级挂死观测：同一 (域, 模型) 在观察窗内连续停滞说明问题不在账号，
				// 逐号轮转只会把一整份超时成本乘以账号数。达到阈值后入口直接失败快返。
				h.cfg.Pool.NoteModelHang(realm, bareModel)
				recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
				st.status = http.StatusServiceUnavailable
				lastErr = fmt.Errorf("%w: %v", errUpstreamTimeout, terr)
				if timeoutRotateExtra > 0 {
					timeoutRotateExtra--
					fail(acct.UID)
					log.Printf("WARN: [server] upstream timeout acct=%s: %v (extra rotation, account not penalized)",
						logfmt.Label(acct.UID, acct.Nickname), terr)
					if !rotateBackoff(i, r.Context()) {
						break
					}
					continue
				}
				log.Printf("WARN: [server] upstream timeout acct=%s: %v (rotation stopped, account not penalized)",
					logfmt.Label(acct.UID, acct.Nickname), terr)
				fail(acct.UID)
				break
			}
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 连败兜底（issue #114）：喂连败计数——连不上上游是「不知道原因的失败」，
			// 连败 N 次临时出池，单次/偶发不罚（NoteFailures 内部达阈才动作）。
			// 上游 client 已打 transport error 日志。
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			h.cfg.Pool.NoteFailures(acct.UID)
			fail(acct.UID)
			if !rotateBackoff(i, r.Context()) {
				break // ctx 取消：终止轮转（传输层错误换号退避）
			}
			continue
		}
		if status >= 400 {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			st.status = status
			// 上游业务码落请求日志：下次出现同类 400 时，`upstream_code` 直接给出
			// 11101/11128/6004 这类判据，不用再翻容器日志或别家网关的日志。
			st.upstreamCode = upstream.BusinessCode(respBody)
			// 6020/6021 的排队信息落日志：`queue_position` 是判断「排队到底要等多久」
			// 的唯一一手数据——决定要不要在网关内等（官方客户端会查
			// /v2/chat/queue/status 到号重发），而不是只凭一次 429 猜。没有这列，
			// 「命中即 429」和「排队等 ready」哪个对，永远只能靠外部推断。
			if st.upstreamCode == 6020 || st.upstreamCode == 6021 {
				st.queuePos = upstream.QueuePosition(string(respBody))
				st.queueRetrySec = int(upstream.QueueRetryAfter(string(respBody)).Seconds())
			}
			var kind upstream.ErrKind
			if uerr != nil {
				kind = uerr.Kind
			} else {
				kind = upstream.Classify(status, string(respBody))
				uerr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			}
			// 11140 风控限制文书（设备/出口级）：换号会连坐，必须就地停手。
			//
			// 11140 有两种形态，处置方向相反：英文 "request illegal" 是渠道校验/授权
			// （账号级，见 IsAccountRestricted，判在该号上）；中文「账号存在异常行为，
			// 已被限制使用」/ "unusual activity" 是风控限制文书——上游判的是设备与
			// 出口，换任何号都会撞同一堵墙（社区复盘：一次任务 7 个号全封）。
			// 因此这里**不轮转**：停掉该号 → 终止本轮 → 让调用方等，而不是拿池子里
			// 剩下的健康号去试。同一窗口内出现第二个不同账号被限 → 全池暂停派发。
			if upstream.IsRiskRestricted(string(respBody)) {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				h.cfg.Pool.Disable(acct.UID, "account restricted by upstream risk control (11140 账号存在异常行为); appeal in WorkBuddy (submit feedback)")
				failKeepBinding()
				waved := h.restrict.note(acct.UID, h.cfg.RestrictionHold)
				if n := h.cfg.Alert; n != nil && waved {
					n.Report("account_restriction_wave",
						"风控信号：多个账号被上游判「存在异常行为」，已全池暂停派发（轮转已止损）",
						"连坐判据在设备与出口上；此窗口内继续换号只会扩大封禁面。")
				}
				msg := "upstream restricted this account for unusual activity (code 11140); not rotating — rotating accounts during a restriction wave gets more of them banned"
				if waved {
					msg = "upstream restricted several accounts for unusual activity (code 11140); dispatch paused pool-wide to avoid further bans — do not retry in a loop"
				}
				if s := strings.TrimSpace(string(respBody)); s != "" {
					msg = s // 上游原文优先：含 requestId，用户要拿去申诉
				}
				hint := "appeal to WorkBuddy (submit feedback) to lift the restriction; rotating accounts or retrying in a loop will get more accounts banned"
				if d := h.restrict.remaining(); d > 0 {
					hint += "; pool dispatch paused for another " + d.Round(time.Second).String()
				}
				downgraded := writeOpenAIErrorHint(w, http.StatusServiceUnavailable, "account_restricted", msg, hint)
				recordErrorOutcome(st, http.StatusServiceUnavailable, downgraded)
				return
			}
			// 内容拦截误报（passthrough/append 模式首遇）：判定为 system 指纹误报，
			// 触发降级到次日 00:00 CST，换 Degraded 中性提示词同请求内重试（append
			// 降级重试同样退化为 replace——原文在场只会确定性再撞 400）。
			// 第二次仍被拦（用户内容本身触发审核）→ 回内容防火墙错误（见下分支）。
			// 内容问题非账号问题：applyErrorPolicy 不罚账号（见 ErrContentBlocked 分支）。
			if kind == upstream.ErrContentBlocked && (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				releaseHeld()
				log.Printf("content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			if kind == upstream.ErrContentBlocked {
				// 内容命中网关内容防火墙：立即回客户端，**不轮转**——换任何账号都会撞同一
				// 审核，轮转纯属浪费时间。不罚账号（ErrContentBlocked 分支无冷却/熔断/NoteError）。
				// error-passthrough：message 装上游 body 原文（code/msg/requestId 原样），
				// 不再改写成网关固定文案——客户端必须看到真实错误才能排查。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				failKeepBinding()
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					// 空 body 兜底：无上游原文可透传，保留可读分类文案（不编造原文）。
					msg = "content blocked by upstream content firewall"
				}
				downgraded := writeOpenAIErrorHint(w, http.StatusBadRequest, "content_blocked", msg,
					h.hintOf(upstream.ErrContentBlocked, string(respBody), bareModel, reqHasImage, uerr))
				recordErrorOutcome(st, http.StatusBadRequest, downgraded)
				return
			}
			// 11115「prompt is too long」：立即透传上游原文回客户端，**不罚号不轮转**
			// ——上下文超限是请求的问题（同一 body 换任何号都超限，白扔健康号配额；
			// 与 WAF IP fail-fast 同哲学：确定与账号无关的错误直接终止轮转）。
			// applyErrorPolicy ErrPromptTooLong 分支零动作，fail 只释放租约。
			// message 装上游 body 原文（含真实 token 数与上限值——上游原文是最有价值
			// 的错误信息，客户端必须看到，禁止固定词覆盖）。
			if kind == upstream.ErrPromptTooLong {
				// 先尝试**可修复重发**：用上游报文里的 used/limit 反推真实 prompt，
				// 收窄输出上限后重发（最多 promptTooLongMaxRepair 跳）。上游判越界的
				// 口径是 prompt_tokens + **输出预留** > 模型窗口，预留取决于请求体有
				// 没有显式 max_tokens——没给时取模型默认值（实测 global 384000 /
				// cn 128000），于是不带 max_tokens 的 OpenAI 兼容客户端明明还有几十万
				// token 可用也被硬顶成 400。收窄只动**输出上限**，不改任何输入内容。
				// 收窄后仍 11115 → 输入本身贴顶，那时才把上游原文透传（换号/换上限都一样）。
				// 注：ErrPromptTooLong 在 applyErrorPolicy 里零动作，不罚号不冷却。
				budget, repaired := h.planPromptTooLongRepair(string(respBody), body, promptTooLongRepairs)
				if repaired {
					body = upstream.WithMaxTokens(body, budget)
					promptTooLongRepairs++
					failKeepBinding()
					delete(tried, acct.UID) // 重发不该让健康号进 tried 后被排除
					log.Printf("INFO: [server] prompt_too_long repair #%d model=%s -> max_tokens=%d (%s)",
						promptTooLongRepairs, bareModel, budget, upstream.PromptTooLongSummaryOf(string(respBody)))
					if !rotateBackoff(i, r.Context()) {
						break
					}
					// 重发不是「换号轮转」，不吃 MaxRotate 预算：把轮转上限配成 1 的
					// 部署否则永远没有第二次机会。次数由 promptTooLongRepairs 封顶。
					i--
					continue
				}
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				failKeepBinding()
				downgraded := writeOpenAIErrorHint(w, http.StatusBadRequest, "prompt_too_long", promptTooLongMessage(string(respBody)),
					h.hintOf(upstream.ErrPromptTooLong, string(respBody), bareModel, reqHasImage, uerr))
				recordErrorOutcome(st, http.StatusBadRequest, downgraded)
				return
			}
			// 图片格式/数据无效：立即透传上游原文回客户端，不罚号不轮转。
			// 同一 body 换账号仍是同样的解析结果，轮转只会放大无效请求。
			if kind == upstream.ErrImageInvalid {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				failKeepBinding()
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "image request was rejected by upstream"
				}
				downgraded := writeOpenAIErrorHint(w, http.StatusBadRequest, "image_invalid", msg,
					h.hintOf(upstream.ErrImageInvalid, string(respBody), bareModel, reqHasImage, uerr))
				recordErrorOutcome(st, http.StatusBadRequest, downgraded)
				return
			}
			// 请求体解析失败（11101）：与 11115 / 图片无效同一哲学——同一 body 换任何
			// 账号都是同样的解析结果，轮转只会放大无效请求（每号一次上游调用 +
			// rotateBackoff 占用在途名额）。更要紧的是：继续轮转后末端会落到
			// 「其余保持 503」，把确定失败的请求伪装成"账号不可用、稍后再试"，
			// 客户端于是对必然失败的请求无限重试。立即透传上游原文回 400。
			if kind == upstream.ErrBadParams {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				failKeepBinding()
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "chat request body was rejected by upstream"
				}
				downgraded := writeOpenAIErrorHint(w, http.StatusBadRequest, "bad_params", msg,
					h.hintOf(upstream.ErrBadParams, string(respBody), bareModel, reqHasImage, uerr))
				recordErrorOutcome(st, http.StatusBadRequest, downgraded)
				return
			}
			// 11148 工具调用记录损坏：与 ErrBadParams 同待遇。同一 body 换任何账号
			// 都是同样的配对失败（配对是在服务端按消息序列校验的，与账号无关），
			// 轮转只会把无效请求放大 MaxRotate 倍，还会把健康号喂进连败计数。
			// 立即透传上游原文回 400，并给可操作提示（重开会话/修好配对再试）。
			if kind == upstream.ErrToolSequence {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				failKeepBinding()
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "tool_call_sequence_broken: tool_calls and tool results do not match"
				}
				downgraded := writeOpenAIErrorHint(w, http.StatusBadRequest, "tool_sequence_broken", msg,
					h.hintOf(upstream.ErrToolSequence, string(respBody), bareModel, reqHasImage, uerr))
				recordErrorOutcome(st, http.StatusBadRequest, downgraded)
				return
			}
			// 14003 是 (账号,模型) 的暂时性容量窗口：写短冷却后继续换号，
			// 但绑定号只释放在途租约、不解除会话；临时代理成功也不改绑。
			if kind == upstream.ErrModelBusy {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
				if stickyUID != "" && acct.UID == stickyUID {
					preserveCapacityBinding = true
					releaseHeld()
				} else {
					fail(acct.UID)
				}
				// 换号**不退避**：14003 是模型级速率窗口，下一次尝试打的是另一个账号，
				// 等待不减少任何单账号的信令，只把「换号即成功」的救路径拖成秒级——
				// 线上 6 条 14003 的 duration_ms 1762/4060/7589/8023 正是退避叠出来的。
				// 与 6004 同待遇：只确认客户端还在，立刻换号。
				// 换号**不退避**：14003 是模型级速率窗口，下一次尝试打的是另一个账号，
				// 等待不减少任何单账号的信令，只把「换号即成功」的救路径拖成秒级——
				// 线上 6 条 14003 的 duration_ms 1762/4060/7589/8023 正是退避叠出来的。
				// 与 6004 同待遇：只确认客户端还在，立刻换号。
				if r.Context().Err() != nil {
					break
				}
				continue
			}
			// 6020「queue.waiting.title」/ 6021「queue.full.title」排队/满载：
			// **零惩罚不变**（不冷却/不熔断/不喂连败——按限流处置会逐号软冷却，实测
			// 2 秒绞杀整池健康号，2026-10-15 实案），且**命中即终止轮转**，按
			// 429 + Retry-After 透传排队语义（见末端 ErrUpstreamBusy 分支）。
			//
			// 为什么不再换号（v1.16.5 曾改成请求内轮转，此处回退）：排队是**按模型**
			// 分的，不是渠道级。2026-11-02 10:43–10:48 的五分钟里，同一批账号上
			// deepseek-v4.1-flash 139 条全部 200，而 glm-5.3 / kimi-k3.1-pro 的 14 条
			// 全部 6020/6021，且全部 attempts=3（三个不同号都撞墙）、耗时 3.4–5.1s。
			// 同一模型换号一次都救不回来，轮转只是把 ~1 秒的失败拖成 3.5 秒，顺带多
			// 打两轮必然失败的上游请求。模型级排队只能等，或让客户端换模型。
			//
			// 保留 429 而不是回到 503：上游对这两个形态本身回的就是 429（6020 带
			// data.retry_after），改写 503 等于把「稍后再试」翻译成「服务不可用」——
			// SDK 见 429+Retry-After 会自动睡后重发，见 503 有客户端直接中断任务
			// （2026-10-28 实案）。
			if kind == upstream.ErrUpstreamBusy {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
				// 排队/满载与账号无关：只释放本次租约、保留会话绑定（不必改绑，
				// 下轮仍走原号——队列是模型级的，换号不换队列）。
				releaseHeld()
				break
			}
			// lastErr 携带完整 body（uerr.Msg 在 upstream 侧截断 200 字符，透传语义
			// 要求原文全量）+ Kind/RetryAfter（末端映射与冷却时长共用）。
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
			fail(acct.UID)
			// WAF IP 级 fail-fast（优先于 rotateBackoff 退避——IP 级拦截时退避无意义）：
			// 该次 WAF 403 喂入 IP 级状态机，若激活（短窗多号命中，IP 被拦而非账号）
			// 则立即终止轮转——继续换号只会把请求放大 MaxRotate 倍打同一出口 IP，
			// 加重风控。账号级软冷却已在上方 applyErrorPolicy 照常记账。
			if kind == upstream.ErrWafBlock && h.wafIP.noteWaf(acct.UID) {
				// IP 级拦截是这里最严重的信号：不是某个号出问题，而是整个出口
				// 被 WAF 盯上，所有账号都会连带失败。推到告警通道（同类 30 分钟静默）。
				if n := h.cfg.Alert; n != nil {
					n.Report("waf_ip_blocked",
						"风控信号：出口 IP 级 WAF 拦截已激活（多号 60s 内接连 403，轮转已止损）",
						"窗口内不同账号接连命中 403，说明拦的是 IP 不是账号；本轮已终止轮转。")
				}
				break
			}
			if !rotateBackoff(attempt-1, r.Context()) {
				break // ctx 取消：终止轮转（分类错误换号退避）
			}
			// 429 软限流不消耗轮换预算：任意 429 形态都只是「这个号此刻被限」，
			// 换下一个未冷却的号才是正解；把预算耗在限流号上，会让本该成功的请求
			// 提前把 429 交给客户端。豁免次数有硬上限（见循环前的 softRateSwaps）。
			if kind == upstream.ErrSoftRate && softRateSwaps < maxSoftRateSwaps {
				softRateSwaps++
				i-- // 抵消 for 的 i++：本次不计入 MaxRotate
			}
			continue
		}
		// 成功判定与粘性绑定一律**延后到这一跳真正成功之后**（见下方流式/非流式分支）：
		// 上游「200 已开流 + 一帧 error」是真实形态（6004 限流、内容拦截、审核），
		// 此前在读第一帧之前就 NoteSuccess + 清 11102 负缓存 + 绑粘性 → 被限流的号
		// 记成健康、粘性把会话钉死在它身上，后续每一轮都打同一个限流号。
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			// 交棒给流内保活。必须在这里停：StreamHint 会重设响应头，与注释帧
			// goroutine 抢同一张 Header map；两者并发写同一 ResponseWriter 也没有
			// 意义——流内保活在它自己的锁下写，覆盖「已开流、首帧未到」。
			keepAlive.stop()
			stats := newChatStatsReaderSince(rc, st.start)
			// gateway_hint（SSE）：成功状态 200 已开流，中途 error 帧透传时附加
			// hint 字段（hintFn 惰性求值——正常流零开销，只有真撞到 error 帧才
			// 组装请求上下文做判定）。
			// errFrame：上游 error 帧原文（观察者旁路采集），用于流尾的账号处置。
			var errFrame string
			// 标记修复：上游在没有 tools 的请求里会把工具调用吐成原生标记文本，
			// 这里在透传前还原成 delta.tool_calls（见 upstream/dsml.go）。
			// 第二个参数为 true = 名单为空时启用弱判定——tools 声明在链路上丢失是
			// 实测最常见的泄漏成因，只做严格判定等于对主场景不设防。
			repair := upstream.NewMarkupRepair(declaredTools, true)
			// 空回复救援（思考吃光 max_tokens → finish_reason=length + 正文零字节）：
			// 同一额度、关闭思考重发一次。不放大客户端要的上限——超发既是语义违约，
			// 也是新的可判别形态；目标只是把预算从思考手里拿回来。
			// 救援流不接 stats 统计（首跳整条为空、救援流通常极短，少计的 token 可忽略）。
			rescue := func() (io.Reader, func(), error) {
				b2, ok := upstream.DisableThinkingBody(body)
				if !ok {
					return nil, nil, nil // 非 deepseek / 解析失败：放弃救援，行为同接线前
				}
				rc2, st2, _, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, b2, clientIP, chatMeta)
				if terr != nil || rc2 == nil || st2 != http.StatusOK {
					if rc2 != nil {
						rc2.Close()
					}
					if terr != nil {
						return nil, nil, terr
					}
					return nil, nil, nil
				}
				log.Printf("INFO: [server] stream acct=%s model=%s: empty reply truncated by thinking budget — retrying with thinking disabled",
					logfmt.Label(acct.UID, acct.Nickname), bareModel)
				return rc2, func() { rc2.Close() }, nil
			}
			sErr := upstream.StreamHint(w, stats, upstream.FrameHintFunc(func() upstream.HintContext {
				return h.hintContext(bareModel, reqHasImage)
			}), upstream.WithErrorFrameObserver(func(payload string) { errFrame = payload }),
				upstream.WithMarkupRepair(repair), upstream.WithEmptyReplyRescue(rescue))
			// 首帧就被否：上游用 HTTP 200 开流、首帧直接是失败帧（11140 request
			// illegal / 6004 限流等），且 StreamHint 尚未向客户端写出任何字节——
			// 这是唯一还能换号的时机。一旦写出第一字节响应头即提交，只能 200+error
			// 帧透传。换号也没用的类别（6021 队列满 / 体畸形 / 上下文超限）已在
			// sse 侧被 firstFrameRejectable 挡掉，不会走到这里。
			var ff *upstream.FirstFrameRejected
			if errors.As(sErr, &ff) {
				rc.Close()
				h.applyErrorPolicy(acct.UID, ff.Kind, ff.Body, bareModel, nil)
				fail(acct.UID)
				lastErr = &upstream.Error{Kind: ff.Kind, Status: http.StatusOK, Msg: ff.Body}
				if keepAlive.Committed() {
					// 外层保活注释帧已经把响应头提交成 200：此时换号也换不回一个
					// 干净的状态码。上游原文照实写进流内 error 帧收尾，宁可让客户端
					// 看到真实错误，也不要再伪装成「还能换号」。
					writeStreamErrorFrame(w, map[string]any{
						"message": ff.Body,
						"type":    "api_error",
						"code":    ff.Kind.String(),
					})
					st.status = http.StatusBadGateway
					st.outcome = reqlog.OutcomeStreamError
					log.Printf("WARN: [server] stream acct=%s model=%s: first frame rejected kind=%s — header already committed by keep-alive, error frame written",
						logfmt.Label(acct.UID, acct.Nickname), bareModel, ff.Kind)
					return
				}
				log.Printf("INFO: [server] stream acct=%s model=%s: first frame rejected kind=%s — rotating (no byte written yet)",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, ff.Kind)
				if !rotateBackoff(attempt-1, r.Context()) {
					st.status = http.StatusServiceUnavailable
					st.outcome = reqlog.OutcomeStreamError
					return
				}
				continue
			}
			switch {
			case upstream.IsEmptyStreamError(sErr):
				// 上游 200 但空流（0 有效帧）：StreamHint 已写 error 帧 + [DONE]
				// 兜底（HTTP 头已发出只能 200），但这是上游缺陷不是成功——日志/
				// 状态收敛到 502 观测，与非流式 Aggregate 空流→502 upstream_parse
				// 同语义（此前 `_ =` 吞错把失败流记成 200，运维看到假成功）。
				// 只认 IsEmptyStreamError：客户端断连的写失败不误标（人已走，
				// 502 观测没有意义）。
				st.status = http.StatusBadGateway
				st.outcome = reqlog.OutcomeStreamError
				log.Printf("WARN: [server] stream acct=%s model=%s: empty upstream stream (200+0 frames)", logfmt.Label(acct.UID, acct.Nickname), bareModel)
			case errFrame != "":
				// 上游以 error 帧报错（6004 限流 / 内容拦截 / 审核）：按帧内容分类并
				// 处置账号——**不记成功、不清 11102 负缓存、不绑粘性**。此前这些动作
				// 在流开始前就做了，于是一个正在限流的号被当成健康号，粘性还会把
				// 整个会话钉在它身上，后续每轮都失败。
				kind := upstream.FrameKind(errFrame)
				h.applyErrorPolicy(acct.UID, kind, errFrame, bareModel, nil)
				st.status = http.StatusServiceUnavailable
				st.outcome = reqlog.OutcomeStreamError
				log.Printf("WARN: [server] stream acct=%s model=%s: upstream error frame kind=%s payload=%s",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, kind, logfmt.Truncate(errFrame, 200))
			case upstream.IsStreamReadError(sErr) && r.Context().Err() == nil:
				// 上游读中断而客户端仍在线：不是成功，也不是客户端断连。
				st.status = http.StatusBadGateway
				st.outcome = reqlog.OutcomeStreamError
				h.cfg.Pool.NoteFailures(acct.UID)
				fail(acct.UID)
				log.Printf("WARN: [server] stream acct=%s model=%s: upstream stream interrupted: %v",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, sErr)
			case sErr != nil:
				// 客户端写失败（断连）：上游帧无恙，账号健康——账号侧照常记成功
				// （与 default 同语义），请求日志归为 Interrupted（人已走，未完成）。
				st.outcome = reqlog.OutcomeInterrupted
				h.cfg.Pool.NoteSuccess(acct.UID)
				h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
				if sessKey != "" && h.cfg.Session != nil && !stickyOverflow && !preserveCapacityBinding {
					h.cfg.Session.Bind(sessKey, acct.UID)
				}
			default:
				// 真成功：这一跳读完且上游没有报错，才记成功并让粘性跟上。
				st.outcome = reqlog.OutcomeSuccess
				h.cfg.Pool.NoteSuccess(acct.UID)
				// 成功是最强的"没挂"证据：清零该模型的停滞观测与熔断。
				h.cfg.Pool.ClearModelHang(realm, bareModel)
				// 11102 负缓存清命：该账号该模型实测成功，立即解除避让（不必等 TTL 到期）。
				// BlockModelClear 按 "11102" reason 前缀识别，只清 11102 条目、不碰 6004 独立冷却。
				h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
				// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
				// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
				if sessKey != "" && h.cfg.Session != nil && !stickyOverflow && !preserveCapacityBinding {
					h.cfg.Session.Bind(sessKey, acct.UID)
				}
			}
			credit, hasCredit := stats.Credit()
			if hit, miss, ok := stats.CacheTokens(); ok {
				st.cacheHit, st.cacheMiss, st.hasCache = hit, miss, true
			}
			recordAttempt(acct.UID, stats.Usage(), credit, hasCredit, attemptStarted, stats.TTFB())
			// WARN 信号放 recordAttempt 之后：st.promptTokens 此时才是本次的观测值。
			if st.hasCache {
				cacheMissWarn.noteCacheTokens(bareModel, st.promptTokens, st.cacheHit, st.cacheMiss)
			}
			if n := repair.Converted(); n > 0 {
				// 修复命中：上游本来会把工具调用吐成正文标记，这里已还原成 tool_calls。
				// 打一行便于运维确认「这次不再空转」以及统计发生率。
				log.Printf("INFO: [server] stream acct=%s model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, n, len(declaredTools))
			} else if m := repair.Seen(); m > 0 {
				// 识别到标记却没还原：说明判定没过（工具名不合规 / 块内夹带正文 /
				// 参数畸形）。单独记一行，排障时能一眼区分「没识别到」与「识别到但拒绝」。
				log.Printf("WARN: [server] stream acct=%s model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, m, len(declaredTools))
			}
			st.ttfb = stats.TTFB()
			// usage 缺失时保留 chatStat.toks 的 -1 哨兵（观测缺失 → 显示 "-"），
			// 不写入零值——否则「没观测到 usage」被伪造成「测得 0 token」，
			// 与非流式走 completionTokens 返回 -1 的口径不一致。
			if toks, hasUsage := stats.Tokens(); hasUsage {
				st.toks = toks
			}
			// 成本账本：末帧 usage 带 credit 与 token 总数时记录实测单价，
			// 供下次选号把免费/便宜的号排在前面。
			if hasCredit {
				st.credit = credit
				st.hasCredit = true
				if total, tok := stats.TotalTokens(); tok && total > 0 {
					h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
				}
			}
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			// 调用方已断开（ctx 取消 / 服务停机）：这不是「上游解析失败」。
			// 三个动作都不能做：不 fail（fail 会解绑粘性，把人自己取消的请求记成
			// 账号问题）、不记 502（人已走，502 观测没有意义）、不写响应。
			// 只留 499 给可观测（nginx 的 client-closed-request 口径）。
			if r.Context().Err() != nil {
				// 真的写一次 499：请求日志与面板的 status 取自实际写出的 HTTP 状态
				// （responseObserver），只改 st.status 会显示成 200。客户端通常已经走了，
				// 写失败也无害；连接还在时（例如服务停机）对方拿到的是明确的非 2xx。
				st.status = statusClientClosedRequest
				st.outcome = reqlog.OutcomeInterrupted
				w.WriteHeader(statusClientClosedRequest)
				return
			}
			fail(acct.UID)
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			st.outcome = reqlog.OutcomeHTTPError
			return
		}
		// 空回复救援（非流式）：finish_reason=length + 正文零字节 + 无 tool_calls
		// = 思考把整份 max_tokens 吃光。同额度关思考重发一次，只改这一次请求体，
		// 不改客户端看到的上限；救援失败按原响应返回（行为同接线前）。
		if upstream.EmptyTruncatedReply(resp) {
			if b2, ok := upstream.DisableThinkingBody(body); ok {
				if rc2, st2, _, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, b2, clientIP, chatMeta); terr == nil && st2 == http.StatusOK && rc2 != nil {
					resp2, err2 := upstream.Aggregate(rc2)
					rc2.Close()
					if err2 == nil && len(resp2) > 0 {
						resp = resp2
						log.Printf("INFO: [server] non-stream acct=%s model=%s: empty reply truncated by thinking budget — retried with thinking disabled",
							logfmt.Label(acct.UID, acct.Nickname), bareModel)
					}
				} else if rc2 != nil {
					rc2.Close()
				}
			}
		}
		// 标记修复（非流式）：把正文里的原生工具调用标记还原成 message.tool_calls。
		// 只在「没有结构化 tool_calls」且「finish_reason 为 stop」时生效（见 dsml.go）；
		// 名单为空时走弱判定（tools 声明在中转环节丢失是常态）。
		mrepair := upstream.RepairAggregatedResponse(resp, declaredTools, true)
		if n := mrepair.Converted(); n > 0 {
			log.Printf("INFO: [server] non-stream model=%s: repaired %d native tool call(s) from assistant text (tool names known=%d)",
				bareModel, n, len(declaredTools))
		} else if m := mrepair.Seen(); m > 0 {
			log.Printf("WARN: [server] non-stream model=%s: saw %d native tool call block(s) but repaired none (tool names known=%d)",
				bareModel, m, len(declaredTools))
		}
		credit, total, hasCredit := usageCreditTotal(resp)
		if usage, ok := resp["usage"].(map[string]any); ok {
			if hit, miss, okC := cacheTokensFromUsage(usage); okC {
				st.cacheHit, st.cacheMiss, st.hasCache = hit, miss, true
			}
		}
		recordAttempt(acct.UID, usageDeltaFromResponse(resp), credit, hasCredit, attemptStarted, 0)
		if st.hasCache {
			cacheMissWarn.noteCacheTokens(bareModel, st.promptTokens, st.cacheHit, st.cacheMiss)
		}
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.outcome = reqlog.OutcomeSuccess
		st.toks = completionTokens(resp)
		// 非流式同理：聚合成功（无 error 帧、非空流）才算这一跳成功，事后才记成功/绑粘性。
		h.cfg.Pool.NoteSuccess(acct.UID)
		h.cfg.Pool.ClearModelHang(realm, bareModel)
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		if sessKey != "" && h.cfg.Session != nil && !stickyOverflow && !preserveCapacityBinding {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		// 成本账本（非流式）：从聚合响应的 usage 取 credit 与 token 总数。
		if hasCredit {
			st.credit = credit
			st.hasCredit = true
			h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
		}
		return
	}
	// 末端错误透传（error-passthrough）：上游返回的错误原样透传，不再规范化成固定文案。
	// 上游返回（*upstream.Error）→ error.message 装**上游 body 原文**（code/msg/
	// requestId 原样保留）。HTTP 状态码按 OpenAI 兼容口径映射类别：ErrSoftRate → 429
	// （限流语义、客户端应等待重试），其余保持 503。本地调度类错误（无可用账号/
	// 传输层抖动/非上游返回的 lastErr）→ 保留自有文案 no_healthy_account（本地错误
	// 没有上游原文可透传，不编造）。
	// 轮转耗尽时调用方已断开：同上，记 499 而不是「所有账号都不可用」——
	// 后者会让人以为池子挂了，实际上是被调方主动取消。
	if r.Context().Err() != nil {
		st.status = statusClientClosedRequest
		st.outcome = reqlog.OutcomeInterrupted
		w.WriteHeader(statusClientClosedRequest) // 理由同非流式聚合分支：status 取实际写出的值
		return
	}
	status := http.StatusServiceUnavailable
	code := "no_healthy_account"
	msg := "all accounts are temporarily unavailable, please retry later"
	// 上游超时：轮转已在传输层分支止损（见 isUpstreamTimeout），这里给一条**能区分**
	// 的文案，别混进"没有可用账号"——两者的排查方向完全不同。
	if errors.Is(lastErr, errUpstreamTimeout) {
		code = "upstream_timeout"
		msg = "upstream timed out: rotation stopped (another account would hit the same slow upstream), please retry later"
	}
	// gateway_hint（末端透传）：上游错误按 Kind + 原文 + 请求形态判定；本地调度类
	// 错误（无上游原文）固定 no_healthy_account hint。
	hint := upstream.NoHealthyAccountHint()
	// upstreamMsgPassed 记录 error.message 是否已被上游原文占据：模型级阻塞分支
	// 据此决定要不要覆盖 msg（上游原文优先，含 requestId）。
	upstreamMsgPassed := false
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		hint = h.hintOf(ue.Kind, ue.Msg, bareModel, reqHasImage, ue)
		switch ue.Kind {
		case upstream.ErrSoftRate:
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		case upstream.ErrWafBlock:
			if h.wafIP.active() {
				// IP 级拦截措辞（fail-fast 终止路径）：网关出口 IP 被 WAF 拦截、
				// 轮转已止损、窗口过后自动解除。客户端提前重试无意义（换号不换 IP）；
				// 有上游原文时原文优先（下方统一）。
				code = "waf_ip_blocked"
				msg = "waf ip-level block: upstream firewall is blocking the gateway IP, rotation stopped; retry after the block window expires"
			}
		case upstream.ErrUpstreamBusy:
			// 6020/6021 排队/满载（命中即终止轮转：该模型此刻全池同撞一堵墙）。
			// 回 429 + Retry-After 而不是 503——上游对这两个形态本身回的就是 429
			// （6020 带 data.retry_after），透传原状态让下游按 OpenAI 限流语义自助
			// 重试；上游原文（requestId / queue_position）仍在 message 原样透传
			// （下方统一覆盖），供客户端日志定位。
			status = http.StatusTooManyRequests
			code = "upstream_busy"
			msg = "upstream queue is full; retry after a short wait"
		case upstream.ErrModelBusy:
			// 14003 是暂时性容量窗口。若全池仅剩该模型的容量冷却，下方
			// modelBlock 分支补充最早恢复时间；混合其他模型冷却时仍按保守的
			// model_unavailable 处理，不把确定性不可用误报成短暂繁忙。
			// 回 429 而不是 503：上游对它本身回的就是 429，语义是「稍后再试」。
			// 网关改写 503 会被部分客户端当成「服务不可用」而中断任务，只有
			// 429 + Retry-After 才会按 SDK 的退避语义自动重试（与 6020/6021 同口径）。
			status = http.StatusTooManyRequests
			code = "model_busy"
			msg = "model capacity is temporarily busy; retry after a short wait"
		case upstream.ErrModelBlocked:
			// 11102「该后端无此模型」：上游原文（下方统一透传）已经写清了原因，但
			// code 此前停在默认的 no_healthy_account —— 那是"服务端过载"的语义，
			// 客户端据此会不断重试（#81 里就是 503 → 客户端自动重试 5 次），而真正
			// 该做的是换个模型。用 400 + 明确的 code 把不可重试的性质讲清楚。
			code = "model_unavailable"
			status = http.StatusBadRequest
		}
		if s := strings.TrimSpace(ue.Msg); s != "" {
			// 上游原文优先：透传 code/msg/requestId，不拼接本地前缀。
			msg = s
			upstreamMsgPassed = true
		}
	}
	// 模型级阻塞覆盖上面的通用文案（放在最后 = 优先级最高）。
	//
	// 为什么能覆盖 code 而不丢信息：modelBlock.Reason 就是 BlockModelBackoff 存下的
	// 上游原因，已在 hint 里原样带出，所以覆盖 message 不会丢掉上游信息，反而补上
	// 了上游不会告诉客户端的两件事——「有几个号被挡」和「最早什么时候解封」。
	//
	// 用 400 而非 503：这是"你选的这个模型当前不可用"，不是"服务端暂时过载"。
	// 换号重试必然同样失败，客户端不该按可重试错误处理。
	if modelBlock.Blocked {
		if modelBlock.Capacity {
			// 全池只剩 14003 容量短窗口：可重试，但不是模型永久不可用。
			//
			// 用 429 而非 503：14003 是「这一刻这个模型没容量」，上游自己回的也是 429；
			// 503 会被部分客户端判定为服务端故障并中断任务，而 429 + Retry-After 会按
			// SDK 退避语义重试（等待口径取容量窗口最早恢复时间，上方已设）。
			status = http.StatusTooManyRequests
			code = "model_busy"
			if !upstreamMsgPassed {
				msg = "model capacity is temporarily busy on every account; retry after a short wait"
			}
			hint = fmt.Sprintf("model_capacity_busy: %d account(s) in a short capacity window", modelBlock.Count)
		} else if modelBlock.QuotaOnly {
			// 全池清一色是 6004/6000 用量配额：这是限流，不是"模型不存在"，按 429 返回。
			// 不设 Retry-After：配额锁要等到上游重置墙钟才解除，客户端把该头当"睡多久"
			// 的指令时会把整段冷却变成静默挂起。重置时刻放在 gateway_hint 里给人看。
			status = http.StatusTooManyRequests
			code = "model_quota_exhausted"
			if !upstreamMsgPassed {
				msg = "model quota exhausted on every account; wait for the upstream reset before retrying"
			}
			hint = fmt.Sprintf("model_quota: %d account(s) hit the upstream quota for this model", modelBlock.Count)
		} else {
			code = "model_unavailable"
			// 有上游原文时保留它（含 requestId，用户要拿去向上游反馈），只把"有几个号
			// 被挡、最早何时解封"这类本地调度信息放进 gateway_hint。
			if !upstreamMsgPassed {
				msg = "model is unavailable on every account (per-model cooldown), try another model"
			}
			hint = fmt.Sprintf("model_blocked: %d account(s) cooling down this model", modelBlock.Count)
			status = http.StatusBadRequest
		}
		if !modelBlock.Until.IsZero() {
			hint += "; earliest unblock at " + modelBlock.Until.Format(time.RFC3339)
		}
		if s := strings.TrimSpace(modelBlock.Reason); s != "" {
			hint += "; upstream: " + s
		}
	}
	// 容量错误统一携带整数秒 Retry-After；普通/混合模型阻塞不保留该头，
	// 避免 400 model_unavailable 却暗示客户端短时间重试。
	switch code {
	case "model_quota_exhausted":
		// 显式不设 Retry-After（见上）；防御性删掉可能由更早分支留下的头。
		w.Header().Del("Retry-After")
	case "model_busy":
		retryAfter := 10 * time.Second
		if modelBlock.Capacity && !modelBlock.Until.IsZero() {
			retryAfter = time.Until(modelBlock.Until)
		}
		if ue != nil && ue.Kind == upstream.ErrModelBusy && ue.RetryAfter > 0 {
			retryAfter = ue.RetryAfter // 合法响应头优先于本地短窗口
		}
		w.Header().Set("Retry-After", retryAfterHeader(retryAfter))
	case "upstream_busy":
		var retryAfter time.Duration
		if ue != nil {
			retryAfter = upstream.QueueRetryAfter(ue.Msg)
			if ue.RetryAfter > 0 {
				retryAfter = ue.RetryAfter
			}
		}
		if retryAfter <= 0 {
			retryAfter = 5 * time.Second
		}
		if retryAfter > maxRetryAfter {
			// 排队口径是「预计等到什么时候」，脏值/时钟漂移可能把天文数字带进来；
			// 封顶避免客户端被一条头静默挂起半小时以上。
			retryAfter = maxRetryAfter
		}
		w.Header().Set("Retry-After", retryAfterHeader(retryAfter))
	default:
		w.Header().Del("Retry-After")
	}
	downgraded := writeOpenAIErrorHint(w, status, code, msg, hint)
	recordErrorOutcome(st, status, downgraded)
}

// tokensPerSecond 计算吐字速率（token/s），返回 (速率, 是否有意义)。
//
// 分母用「生成耗时」= 端到端耗时 - 首 token 等待（TTFB），不是端到端耗时。
//
// 为什么要减：不减的话，首 token 等待越长、报告速率被压得越低。同一模型换个
// 上游或网络，TTFB 从 0.3s 涨到 2s，速率能凭空掉一半——读起来像"模型变慢了"，
// 其实只是排队久了。issue #34 报的正是这个，维护者也确认「速率计算时并没有减去
// 首token到达时间」。
//
// ttfb<=0 表示没有观测：非流式回复天然没有「首个 data 帧」（日志里那一列记的是
// "-"）。此时不猜、不扣——凭空假定一个 TTFB 会把分母推向零、把速率抬成虚高，
// 比不扣更糟。只有真测到才扣。
//
// 同理，ttfb 不小于总耗时时（时钟粒度、或 TTFB 落在计时终点之后）退回端到端耗时，
// 避免零/负分母。token 数为负哨兵值（-1 = 观测缺失）时返回 false。
//
// 扣除后不足 minGenWindow 也退回端到端（issue #127）：ttfb 量的是「首个 SSE 帧
// 到达」，当上游把整个响应攒到最后一次性下发（假流式/中间层攒批刷新——首帧与
// 末帧几乎同时到）时，total−ttfb 只剩几毫秒，拿它当分母会把几百 token 除成
// 上万 tok/s 的幻数。这种形态下「生成时长」根本不可测，诚实的分母只有端到端。
//
// 用量账本（handler）与控制台流水行（logging.go）都走这一个函数：两处各算一遍时
// 口径漂移过一次（流水行漏扣 TTFB、与面板数字对不上），共用是防再次分叉的唯一办法。
func tokensPerSecond(completionTokens int64, total, ttfb time.Duration) (float64, bool) {
	if completionTokens < 0 || total <= 0 {
		return 0, false
	}
	gen := total
	if ttfb > 0 {
		if g := total - ttfb; g >= minGenWindow {
			gen = g
		}
	}
	return float64(completionTokens) / gen.Seconds(), true
}

// minGenWindow 可信生成窗口的下限：扣除 TTFB 后剩余窗口不足该值即视为「生成
// 时长不可测」，退回端到端耗时（见 tokensPerSecond 注释，issue #127）。
// 真流式下首帧到末帧通常铺满剩余窗口，200ms 远低于正常生成时长，不影响真实
// 快速输出（如缓存命中后的爆发）被如实报告。
const minGenWindow = 200 * time.Millisecond

// promptTooLongMaxRepair 单个请求允许的 11115 可修复重发次数。两跳足够收敛：
// 第一跳用上游 used/limit 反推 prompt 后精确算出预算，第二跳兜住预留口径偏差。
const promptTooLongMaxRepair = 2

// planPromptTooLongRepair 判断本次 11115 是否可修复，可修则返回收窄后的输出上限。
func (h *Handler) planPromptTooLongRepair(respBody string, body []byte, repairs int) (int64, bool) {
	if repairs >= promptTooLongMaxRepair {
		return 0, false
	}
	used, limit, ok := upstream.ParsePromptTooLong(respBody)
	if !ok {
		return 0, false
	}
	sent, explicit := upstream.MaxTokensInBody(body)
	return upstream.PlanPromptTooLongRepair(used, limit, sent, explicit)
}

// promptTooLongMessage 11115 透传 message：上游 body 原文（含真实 token 数/
// 上限值/requestId，客户端自行排查）；空 body 兜底为可读分类短文案（不编造原文）。
func promptTooLongMessage(body string) string {
	if strings.TrimSpace(body) == "" {
		return "prompt is too long"
	}
	return body
}

// usageCreditTotal 从聚合响应取 usage.credit 与 total_tokens（成本台账非流式入口）。
// 任一字段缺失/非法 → ok=false（不记录）。
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	usage, _ := resp["usage"].(map[string]any)
	if usage == nil {
		return 0, 0, false
	}
	c, _ := usage["credit"].(float64)
	t, _ := usage["total_tokens"].(float64)
	if t <= 0 {
		return 0, 0, false
	}
	return c, int(t), true
}

// rotateBackoff 轮转间指数退避 + 抖动（WAF 403 修复 P0-2）：第 i 次轮转失败
// （continue 换号前）等待 backoffAfter(i)（500ms·2^i 封顶 8s，±25% 抖动），
// ctx 取消（客户端断连/优雅停机）返回 false——调用方立即终止轮转（客户端已走，
// 换号重试无意义）。退避是「换号前歇一下」让上游频控窗口滑过；正常单号请求
// （首次成功）不经过本函数，零开销。
func rotateBackoff(i int, ctx context.Context) bool {
	d := backoffAfter(i)
	if d <= 0 {
		return ctx.Err() == nil
	}
	if !sleepCtx(ctx, d) {
		log.Printf("WARN: [server] rotate backoff aborted: ctx cancelled")
		return false
	}
	return true
}

// errUpstreamTimeout 上游超时的哨兵：末端出口据此给出与「没号可用」可区分的文案。
var errUpstreamTimeout = errors.New("upstream timeout")

// isUpstreamTimeout 判断这一跳的失败是否属于「上游超时 / 停滞」。
// 调用方据此走有界加转且不惩罚账号的路径。
// 三态判定见传输层错误分支的注释。
func isUpstreamTimeout(err error, clientGone bool) bool {
	if err == nil {
		return false
	}
	// 建连失败与响应停滞是两类故障。DNS、dial 和 TLS 握手错误即使实现了
	// net.Error.Timeout() 或包裹 deadline，也只能走普通传输错误路径，不能消耗
	// TimeoutRotateExtra，更不能被记成 upstream_timeout。
	if isConnectError(err) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	return !clientGone && errors.Is(err, context.Canceled)
}

// isConnectError 识别 DNS/dial/TLS 建连失败。必须先于通用 net.Error 与
// deadline 判定调用：这些错误的 Timeout() 只说明建连阶段到时，不代表上游已
// 接受请求后停滞。TLSHandshakeTimeout 在标准库中没有独立导出错误类型，使用其
// 固定错误文案识别；普通 ResponseHeaderTimeout 不包含该文案，仍按上游超时处理。
func isConnectError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && strings.EqualFold(opErr.Op, "dial") {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "tls handshake timeout")
}

// statusClientClosedRequest 调用方主动断开（nginx 的非标准 499 口径）。
// Go 的 net/http 没有这个常量：它不是 HTTP 语义状态，而是给日志/观测用的标记，
// 用来把「人走了」和「上游/网关出错」分开——否则取消的请求会被记成 502/503，
// 运维看到的是假故障。
const statusClientClosedRequest = 499

// maxRetryAfter Retry-After 头的本地上限：容量/排队类错误的等待口径是上游
// 「预计如此」，脏值或时钟漂移会把天文数字带进头里；给客户端一个上界，避免
// 一条头把请求静默挂起很久。与上游 6020 的 retry_after=40 量级不冲突。
const maxRetryAfter = 10 * time.Minute

// retryAfterHeader 把等待时长编码为 Retry-After 整数秒：亚秒向上取整，正等待
// 最少返回 1，避免客户端把 500ms 理解成立即重试。
func retryAfterHeader(d time.Duration) string {
	seconds := d / time.Second
	if d%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(int64(seconds), 10)
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify / ChatStreamContext 的 *Error 信封），
// 此处不再按原始 status 二次判断。仅在 chatCompletions 轮转循环内调用：内容拦截
// 会立即 400 返回，其余种类 continue 换号（continue 前由 rotateBackoff 退避）。
//
// 十条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 优先对齐上游重置墙钟（带「将在 … 重置」时 6004 走模型级豁免、
//     非 6004 走账号级，均不指数堆加）；无重置时间才走有界退避。冷却时长优先采信
//     Retry-After 头（uerr.RetryAfter，body 文案墙钟之外的头形态来源）。
//   - ErrWafBlock → 账号级软冷却：**不 Disable**——WAF 403 是 IP/指纹维频控信号，
//     罚过即走、到期自愈。时长优先 Retry-After 头；缺失按 wafCooldownBase(60s)
//     起 · softStreak 指数、封顶 soft_rate_max 的既有 CooldownSoftRate 有界退避。
//     基数经 jitterDur 抖动（防多账号同相位冷却到期再聚团）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → 不罚账号；passthrough 首遇触发降级重试，最终仍拦则回 400。
//   - ErrBadParams → 不罚账号，且与 ErrPromptTooLong/ErrImageInvalid 同待遇：
//     调用方在轮转循环内即刻 400 透传原文终止（换号必然同样失败）。
//   - ErrPromptTooLong → 11115：请求的问题不是账号的问题。零动作（不冷却/不熔断/
//     不 NoteError、不喂连败），chatCompletions 已直接透传原文返回不轮转。
//   - ErrImageInvalid → 图片格式/数据无效：请求的问题不是账号的问题（同一 body
//     换任何号都会得到相同的解析错误）。零动作（不冷却/不熔断/不 NoteError、
//     不喂连败），chatCompletions 已直接透传原文返回不轮转。
//   - ErrModelBlocked → BlockModelBackoff：(账号, 模型) 11102/11158 负缓存避让。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断；ErrClient
//     额外喂连败计数（NoteFailures，issue #114）：未知 4xx 连败 N 次临时出池。
//
// body 仅在 ErrSoftRate/ErrAccountFault 分支用于解析重置时间/分野；model 为请求
// 携带的模型名。uerr 是 ChatStreamContext 返回的分类信封（可携带 RetryAfter）；
// 零值/防御路径下为 nil，冷却时长回落既有计算。
// noteRiskSignal 把「上游在收紧」的信号推给告警通道（同类 30 分钟静默一次）。
//
// 只挑这四类，不报普通限流与参数错误——429 和 400 天天有，报了只会让人对告警
// 脱敏。四类各自对应一种收紧方式：
//   - channel_refused（11128）：上游判定请求不是官方客户端发的（内容指纹命中）
//   - waf_block（403）：出口 IP 被 WAF 拦（多号连撞时最危险）
//   - session_dead（12153）：登录态被踢，通常意味着账号被风控
//   - account_fault（11140）：渠道校验失败（账号级授权异常）
//
// 生产上这四类长期是 0 条——正因为稀少，出现一次就值得看一眼。
func (h *Handler) noteRiskSignal(uid string, kind upstream.ErrKind, body, model string) {
	n := h.cfg.Alert
	if n == nil {
		return
	}
	short := uid
	if len(short) > 8 {
		short = short[:8]
	}
	var sb strings.Builder
	sb.WriteString("账号: ")
	sb.WriteString(short)
	if model != "" {
		sb.WriteString("\n模型: ")
		sb.WriteString(model)
	}
	if s := strings.TrimSpace(body); s != "" {
		if len(s) > 400 {
			s = s[:400] + "…"
		}
		sb.WriteString("\n上游原文: ")
		sb.WriteString(s)
	}
	detail := sb.String()
	switch kind {
	case upstream.ErrChannelRefused:
		n.Report("channel_refused", "风控信号：11128 通道校验失败（上游认为请求不是官方客户端）", detail)
	case upstream.ErrWafBlock:
		n.Report("waf_block", "风控信号：403 WAF 拦截（出口 IP 可能已被标记）", detail)
	case upstream.ErrSessionDead:
		n.Report("session_dead", "风控信号：登录态失效 12153（账号可能被踢）", detail)
	case upstream.ErrAccountFault:
		if upstream.IsAccountRestricted(body) {
			n.Report("account_fault", "风控信号：11140/11142 账号被上游限制（request illegal）", detail)
		} else {
			n.Report("account_fault", "账号级故障："+upstream.AccountFaultReason(body), detail)
		}
	}
}

func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string, uerr *upstream.Error) {
	h.noteRiskSignal(uid, kind, body, model)
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		resetAt, _ := upstream.ParseRateReset(body)
		if resetAt.IsZero() && uerr != nil && uerr.RetryAfter > 0 {
			resetAt = time.Now().Add(uerr.RetryAfter)
		}
		if upstream.IsModelRateLimit(body) && model != "" {
			// reason 写**实际**命中的码：它同时是面板展示文案与 pool.isQuotaReason
			// 的判定依据（6000/6004 视为日配额，其它视为瞬时速率）。此前恒写 6004，
			// 扩族后 6001–6008 会被一并误标成日配额，把「秒级速率窗口」当成
			// 「等到次日」处理。
			code := upstream.ModelRateLimitCode(body)
			if code == 0 {
				code = 6004 // 只命中正则、取不到数字码时的历史兜底
			}
			reason := fmt.Sprintf("%d model rate limit", code)
			h.cfg.Pool.CooldownSoftForModel(uid, h.softCooldown(), resetAt, model, reason)
			return
		}
		h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), resetAt, "429 rate limit")
	case upstream.ErrWafBlock:
		// WAF 403（无业务信封拦截形态）。软冷却复用 CooldownSoftRate 家族：基数
		// wafCooldownBase（60s，抖动后落 [45s,75s]）、softStreak 指数升级、封顶
		// soft_rate_max、冷却中兜底探测不翻倍——全部继承既有语义。
		// Retry-After 头优先（WAF 拦截页可能带该头）。不 Disable。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Now().Add(uerr.RetryAfter), "waf 403 block (retry-after)")
			return
		}
		h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Time{}, "waf 403 block")
	case upstream.ErrSessionDead:
		if uerr != nil && upstream.IsGatewayUnauthorized(uerr.Status, body) {
			acct := h.cfg.Pool.AuthByUID(uid)
			switch h.cfg.Upstream.VerifySessionByRefresh(acct) {
			case upstream.VerifyAlive:
				if err := acct.SaveAtomic(); err != nil {
					log.Printf("ERR: [server] gateway 401 refresh acct=%s: save auth failed: %v", logfmt.Label(acct.UID, acct.Nickname), err)
				}
				h.cfg.Pool.ClearSessionDead(uid)
				h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Time{}, "gateway 401 recovered")
			case upstream.VerifyDead:
				h.cfg.Pool.NoteSessionDeadWith(uid, "gateway 401 refresh rejected")
			default:
				h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Time{}, "gateway 401 inconclusive")
			}
			return
		}
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrAccountFault:
		// 账号级授权/配额故障按业务码分野（口径与 Classify 一致）：
		//   - 11140 / 11142 + "request illegal" → 账号被上游限制（异常行为/风控）：硬禁用。
		//     解除只能由用户在 WorkBuddy 官方渠道申诉，重登无效（issue 1849 结论）。
		//   - 14015 授权过期 / 14016 企业未激活 / 14017 试用未激活 → register 或重新登录
		//     后可能自愈，**保持软冷却**（禁用会让用户补完流程后仍无法用）。
		//     reason 写实际码，面板能直接看出是哪一个。
		// 注意 11128 的 msg 同为 "request illegal"，但那是通道校验失败，已在
		// Classify 分流到 ErrChannelRefused，走不到这里。
		if upstream.IsAccountRestricted(body) {
			h.cfg.Pool.Disable(uid, "account restricted by upstream (11140/11142 request illegal); appeal in WorkBuddy (submit feedback) to lift the restriction")
			return
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.softCooldown(), upstream.AccountFaultReason(body))
	case upstream.ErrChannelRefused:
		// 11128 通道校验失败（上游判定请求可能不是官方客户端发出，displayMsg 就是
		// 「请求被拦截，请重新发送」）。账号本身健康——官方客户端可正常使用同一账号，
		// 这不是账号级故障，**不罚账号**（无冷却/熔断/NoteFailures）。与
		// ErrContentBlocked 同待遇，由轮转继续尝试（换号不改变通道指纹，若全池同错
		// 会把上游原文透传给客户端，提示排查客户端指纹）。
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrContentBlocked:
		// 内容策略拦截（误报）：内容问题非账号问题，不罚账号（无冷却/熔断/NoteError）。
		// passthrough 模式由 chatCompletions 内降级重试处理；custom 模式本不会到此分支。
	case upstream.ErrPromptTooLong:
		// 11115「prompt is too long」：请求的问题不是账号的问题（同一 body 换任何
		// 号都超限）。零动作（不冷却/不熔断/不 NoteError，同 ErrContentBlocked 待遇），
		// chatCompletions 已直接透传原文返回不轮转——该分支只为文档完备。
	case upstream.ErrImageInvalid:
		// 图片格式/数据无效：请求的问题不是账号的问题（同一 body 换任何号都会
		// 得到相同解析错误）。零动作，chatCompletions 已 fail-fast 透传。
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关侧不再截断，均为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇）；chatCompletions
		// 已 fail-fast 400 透传原文、终止轮转——「换号可能有不同模型权限」属 11102
		// （ErrModelBlocked）的分类域，与本类无关。
	case upstream.ErrToolSequence:
		// 11148 工具调用记录损坏：请求的消息序列本身配对不上（assistant 的
		// tool_calls 与后续 tool 结果对不齐），换任何账号结果都一样。零动作
		// （不冷却/不熔断/不 NoteError/不喂连败），chatCompletions 已 fail-fast
		// 400 透传上游原文并终止轮转。
	case upstream.ErrUpstreamBusy:
		// 6020/6021 渠道级排队/满载：账号余额健康、未限流、session 未死，
		// 换号也撞同一堵墙。零动作（不冷却/不熔断/不 NoteError/不喂连败，同
		// ErrContentBlocked 待遇）；chatCompletions 会换号重试（排队可用性间歇），
		// 轮转耗尽后走末端 ErrUpstreamBusy 分支回 429 + Retry-After。
		// 一旦按 ErrSoftRate 罚号，会与全冷却兜底互相喂成整池停摆（2026-10-15 实案）。
	case upstream.ErrModelBusy:
		// 14003 只避让当前 (账号,模型) 的短容量窗口，不做账号级软冷却、
		// 不喂熔断或连败。已有粘性由调用方保留；新会话由普通选号严格避开。
		var retryAfter time.Duration
		if uerr != nil {
			retryAfter = uerr.RetryAfter
		}
		reason := "14003 model capacity busy"
		if upstream.IsModelUnavailable(body) {
			reason = "11134 model unavailable"
		}
		h.cfg.Pool.CooldownModelBusy(uid, model, retryAfter, reason)
	case upstream.ErrModelBlocked:
		// 11102「该后端无此模型」/11158「该账号类型无此模型权限」：
		// (账号, 模型) 负缓存避让。复用 modelCooldowns 机制（与 6004 同域），
		// 选号侧 healthyForModel 对该账号自动避开该模型。
		// 立即换号（本轮 continue），该账号该模型冷却，下次选号避开。
		h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReasonFor(body))
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
		// ErrClient（未知 4xx）喂连败计数（issue #114）：连续 N 次该形态失败 →
		// 账号临时出池（NoteFailures 达阈降权），单次/偶发不罚（不误伤）。ErrNone
		// 到这里属防御路径（status>=400 但分类成功），语义不明不喂。
		if kind == upstream.ErrClient {
			h.cfg.Pool.NoteFailures(uid)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeOpenAIErrorHint(w, status, code, msg, "")
}

// wafCooldownBase WAF 403 软冷却基数（建议 60s 起；抖动 ±25% 后落 [45s,75s]，
// 实际进入 CooldownSoftRate 后再按 softStreak 指数、封顶 soft_rate_max）。
// 与 SoftCooldown 分流的原因：WAF 403 是 IP/指纹维频控，信号比 429「账号级限流」轻
// （账号本身健康），但比 404 重（带粘性会连环）；60s 级的快速避让已足够让频控窗口
// 滑过。抖动复用 backoff.go jitterDur（单一来源）。
const wafCooldownBase = 60 * time.Second

// writeOpenAIErrorHint 同 writeOpenAIError，另在 error 对象上附加
// error.gateway_hint（hint 为空串时不带字段——未覆盖形态不编造）。
// message 仍是上游原文透传（hint 只做并列补充，绝不替换/包装 message）。
// writeOpenAIErrorHint 写出 OpenAI 兼容的错误响应，返回是否被降级成流内 error 帧。
//
// 降级的条件只有一个：响应头已被流式保活注释帧提交（状态码已定死成 200）。此时
// 再写状态码和 JSON 正文，客户端会在一条已开的 SSE 流里读到一段解析不了的内容；
// 改成流内 error 帧后，SDK 按「上游中途报错」处理——与真实形态一致。
func writeOpenAIErrorHint(w http.ResponseWriter, status int, code, msg, hint string) bool {
	inner := map[string]any{
		"message": msg,
		"type":    "api_error",
		"code":    code,
	}
	if hint != "" {
		inner["gateway_hint"] = hint
	}
	if k, ok := w.(interface{ Committed() bool }); ok && k.Committed() {
		writeStreamErrorFrame(w, inner)
		return true
	}
	writeJSON(w, status, map[string]any{"error": inner})
	return false
}

// writeStreamErrorFrame 在已提交的流里写出错误终态：一帧 error + 结束帧。
//
// 形状与上游的 error 帧一致（顶层 error 对象 + data: [DONE]），客户端不需要为
// 网关产生的错误单独写一套解析。
func writeStreamErrorFrame(w http.ResponseWriter, inner map[string]any) {
	raw, err := json.Marshal(map[string]any{"error": inner})
	if err != nil {
		return
	}
	if _, werr := fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw); werr != nil {
		return
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// recordErrorOutcome 收敛错误请求的请求日志口径。
//
// 降级时线上真实状态是 200（响应头已被保活注释帧提交），但 status 列仍记「这是
// 哪一类错误」，另用 outcome=stream_error 标明客户端是在流里收到的——与上游中途
// 报错帧的既有口径一致，避免把这类请求记成纯 HTTP 4xx 而掩盖「流已开」的事实。
func recordErrorOutcome(st *chatStat, status int, downgraded bool) {
	st.status = status
	if downgraded {
		st.outcome = reqlog.OutcomeStreamError
		return
	}
	st.outcome = reqlog.OutcomeHTTPError
}

// hasImagePart 报告聊天请求体是否携带多模态 image_url part（OpenAI 兼容形态
// messages[].content[] {type:"image_url"}）。畸形/其他形态一律 false（hint 侧
// 宁缺勿滥：判不出带图就不给「模型不支持图片」指向）。
func hasImagePart(body []byte) bool {
	var peek struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &peek) != nil {
		return false
	}
	for _, m := range peek.Messages {
		for _, p := range m.Content {
			if p.Type == "image_url" {
				return true
			}
		}
	}
	return false
}

// hintContext 组装 chatCompletions 的 gateway_hint 判定上下文：请求裸模型名 +
// 是否带图 + 模型目录 supports_images 声明（目录未收录 → ModelInCatalog=false，
// 不做「不支持」判定，防查不到误判）。仅错误路径调用（成功请求零开销）。
//
// 目录查询只读既有缓存快照（cachedModelsSnapshot），**不触发上游拉取**：错误路径
// 加一次 FetchModels 网络调用既拖慢错误响应、又污染上游调用语义（错误风暴时放大
// 请求量——与 WAF IP fail-fast 的「不放大请求量」哲学相悖）。缓存冷（最近 10min 未
// 拉过）→ ModelInCatalog=false，11133 退中性 hint（宁缺勿滥，不编造能力事实）。
func (h *Handler) hintContext(bareModel string, hasImage bool) upstream.HintContext {
	ctx := upstream.HintContext{Model: bareModel, HasImage: hasImage}
	if bareModel == "" {
		return ctx
	}
	for _, mi := range cachedModelsSnapshot() {
		if mi.ID == bareModel {
			ctx.ModelInCatalog = true
			ctx.ModelSupportsImages = mi.SupportsImages
			return ctx
		}
	}
	return ctx
}

// hintOf 末端错误透传的统一 hint 入口：kind + 上游原文 + 请求上下文 →
// gateway_hint 文案（upstream.GatewayHint 单一事实来源）。uerr 为 nil 时回落
// body 原文判定（防御路径）。transport 层错误（lastErr 非 *upstream.Error 且
// 上游没回 body）→ 无 hint（不编造）。
func (h *Handler) hintOf(kind upstream.ErrKind, body, bareModel string, hasImage bool, uerr *upstream.Error) string {
	msg := body
	if uerr != nil && uerr.Msg != "" {
		msg = uerr.Msg
	}
	return upstream.GatewayHint(kind, msg, h.hintContext(bareModel, hasImage))
}
