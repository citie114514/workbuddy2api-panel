package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// reportClientIPPath 客户端公网 IP 的回显端点（chat 域）。
const reportClientIPPath = "/v2/geoblock"

// reportClientIPTimeout 探测超时。取不到就放弃——这个字段只是事件里的一个附带值，
// 不值得为它拖慢上报。
const reportClientIPTimeout = 3 * time.Second

// 进程级缓存：官方客户端也是启动后探一次、之后复用（按进程而非按账号）。
var (
	reportClientIPOnce sync.Once
	reportClientIPVal  string
)

// resolveReportClientIP 取本机出口公网 IP，用于填事件里的 clientIp。
//
// 该请求是裸调用：不带 Authorization、不带任何自定义头（与官方一致）。
// 失败返回空串，调用方据此不注入该字段（宁缺不错）。
func (c *Client) resolveReportClientIP(a *auth.Auth) string {
	reportClientIPOnce.Do(func() {
		base := c.chatBase(a)
		if base == "" {
			return
		}
		req, err := http.NewRequest(http.MethodGet, strings.TrimRight(base, "/")+reportClientIPPath, nil)
		if err != nil {
			return
		}
		// 超时挂在 context 上（不改调用方 client 的配置）；client 优先用 Client 自带的那份，
		// 这样测试注入的 transport 能拦到这条请求。
		ctx, cancel := context.WithTimeout(context.Background(), reportClientIPTimeout)
		defer cancel()
		req = req.WithContext(ctx)
		cli := c.HTTP
		if cli == nil {
			cli = http.DefaultClient
		}
		resp, err := cli.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return
		}
		var obj struct {
			IP string `json:"ip"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&obj); err != nil {
			return
		}
		if ip := strings.TrimSpace(obj.IP); isProbablyIP(ip) {
			reportClientIPVal = ip
		}
	})
	return reportClientIPVal
}

// isProbablyIP 只做形态校验（IPv4 点分 / 含冒号的 IPv6），避免把上游的错误页塞进事件。
func isProbablyIP(s string) bool {
	if s == "" || len(s) > 45 {
		return false
	}
	return net.ParseIP(s) != nil
}

// injectReportClientIP 给批量事件的每一条补 clientIp（已有则不覆盖）。
//
// 放在发送处统一注入，而不是给每个事件结构体加字段：/v2/report 的事件码有多个，
// 逐结构体加字段迟早会漏掉其中一个，漏了就是「同一批事件里有的带、有的不带」。
// body 不是 JSON 数组时原样返回（不动非事件负载）。
func (c *Client) injectReportClientIP(a *auth.Auth, body []byte) []byte {
	ip := c.resolveReportClientIP(a)
	if ip == "" {
		return body
	}
	var evs []map[string]any
	if err := json.Unmarshal(body, &evs); err != nil || len(evs) == 0 {
		return body
	}
	changed := false
	for _, ev := range evs {
		if _, ok := ev["clientIp"]; ok {
			continue
		}
		ev["clientIp"] = ip
		changed = true
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(evs)
	if err != nil {
		return body
	}
	return out
}
