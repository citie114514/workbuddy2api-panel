// outbound_log.go 4xx 排障：把出站请求体写进日志（默认关闭，见
// features.debug_log_outbound_body）。
//
// 为什么只在 4xx 记：400/422 的成因（stop 类型不对、工具配对断裂、参数越界）几乎
// 只体现在我们发出去的那份 body 里，上游回包往往只有一句「参数不合法」。5xx 与限流
// 没有这个价值，反而会把日志刷满、还可能泄露大段用户上下文。
package server

import (
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// outboundBodyLogLimit 单条日志的 body 上限（字节）。4xx 的成因基本都在前几 KB
// （参数区 / messages 头部），没必要把整个长上下文写进日志。
const outboundBodyLogLimit = 4096

// dataURLRe 匹配 data URL 里的 base64 内容（至少 64 字符，避免误伤正文里偶然出现的
// 短串）。图片与音频的体积和隐私都在这里，必须剥掉。
var dataURLRe = regexp.MustCompile(`data:([A-Za-z0-9.+/-]+);base64,[A-Za-z0-9+/=]{64,}`)

// maskDataURLs 把 base64 内容替换成 "<masked N bytes>"，保留 MIME 与原始长度：
// 排障时需要看到「确实带了图、有多大」，但不能把图本身落盘。
func maskDataURLs(s string) string {
	return dataURLRe.ReplaceAllStringFunc(s, func(m string) string {
		i := strings.Index(m, ";base64,")
		if i < 0 {
			return m
		}
		head := m[:i+len(";base64,")]
		return head + "<masked " + strconv.Itoa(len(m)-len(head)) + " bytes>"
	})
}

// outboundBodyForLog 返回可直接落日志的 body 摘要：图片 base64 脱敏 + 长度截断。
// 截断点回退到 rune 边界，避免把多字节字符劈成乱码（日志按行解析会出错）。
func outboundBodyForLog(body []byte) string {
	s := maskDataURLs(string(body))
	if len(s) <= outboundBodyLogLimit {
		return s
	}
	cut := outboundBodyLogLimit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "...<truncated total=" + strconv.Itoa(len(s)) + " bytes>"
}

// logOutboundOn4xx 4xx 时把出站 body 写进日志。
//
// 只认 4xx 且**排除 429**：5xx 是上游/网络的问题，body 看不出更多；429 是限流，
// 一天几十条的排队/限流会各带一整份长上下文，把日志真正有用的线索淹掉。
func (h *Handler) logOutboundOn4xx(st *chatStat, body []byte) {
	if st == nil || st.status < 400 || st.status >= 500 || st.status == http.StatusTooManyRequests || len(body) == 0 {
		return
	}
	log.Printf("WARN: [server] outbound_4xx acct=%s model=%s status=%d upstream_code=%d bytes=%d body=%s",
		logfmt.Label(st.uid, st.nick), st.model, st.status, st.upstreamCode, len(body), outboundBodyForLog(body))
}
