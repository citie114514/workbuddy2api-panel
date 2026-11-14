package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
)

// readUpstreamBody 读取假上游收到的请求体。
//
// 网关按官方 productFeature.RequestBodyGzip 的行为压缩 chat 请求体（见
// upstream.gzipRequestBody），假上游必须像真上游一样解压后再断言——否则测试
// 拿到的是压缩流，JSON 解析必然失败。
func readUpstreamBody(r *http.Request) []byte {
	if r == nil || r.Body == nil {
		return nil
	}
	var rd io.Reader = r.Body
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			raw, _ := io.ReadAll(r.Body)
			return raw
		}
		defer zr.Close()
		rd = zr
	}
	raw, _ := io.ReadAll(rd)
	return raw
}
