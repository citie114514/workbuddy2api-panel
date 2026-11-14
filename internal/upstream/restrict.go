// restrict.go 上游「账号存在异常行为」风控文书的识别（连坐保护的判据）。
//
// 移植自 misakano7545/workbuddy2api-panel（internal/upstream/restrict.go）。
//
// 11140 有两种形态，处置方向完全相反，必须分开：
//   - 英文 "request illegal"（渠道校验/授权）→ 见 IsAccountRestricted，判在账号上；
//   - 账号风控限制文书（中文「账号存在异常行为，已被限制使用」/ 英文 "unusual
//     activity"）→ 本文件。这一类判据在上游是**设备/出口级**，换账号只会连坐
//     （社区封禁复盘：一次任务里连撞 7 个号，全被封）。
//
// 为什么必须 code + 文案同时要求：11140 也承载模型级限流（"The model provider is
// rate-limiting requests."），只按 code 判会把限流转成账号终态（少服务一天）；只按
// 文案判会误伤正文里恰好讨论这些词的数据。
package upstream

import "strings"

// restrictionMarkers 风控限制文书的实测措辞。
var restrictionMarkers = []string{
	"账号存在异常行为",
	"已被限制",
	"unusual activity",
}

// IsRiskRestricted 报告响应信封是否为「账号被上游风控限制」（结构化 code 11140 +
// 限制文案）。与 IsAccountRestricted 的区别：那个认的是渠道校验的 "request illegal"
// （账号级授权问题），这个认的是风控限制文书（设备/出口级，换号会连坐）。
func IsRiskRestricted(body string) bool {
	if len(body) == 0 {
		return false
	}
	if !hasBusinessCode(body, "11140") {
		return false
	}
	lower := strings.ToLower(body)
	for _, m := range restrictionMarkers {
		if strings.Contains(body, m) || strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}
