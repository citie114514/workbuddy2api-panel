// encoding.go 出站请求的 Accept-Encoding 与响应解压。
//
// 为什么必须自己解压：Go 的 http.Transport 只在自己补 `Accept-Encoding: gzip`
// 时才自动解包；一旦请求显式设了这个头，标准库就把响应原样交出来。所以设了
// 官方那串编码之后，解压责任全在我们这边。
//
// 依据（官方桌面端 5.9.3 / 5.8.3 安装包，2026-10-31 核对，同类项目
// misakano7545/workbuddy2api-panel 真机抓包互证）：
//   - 内置 CLI 与后台服务的请求框架都是 axios，http 适配器在请求没设
//     Accept-Encoding 时补 `gzip, compress, deflate, br`。对话、刷新令牌、登录、
//     模型目录、积分、签到、上报、外观、专家列表**全是这个值**。
//   - CLI 的 axios 另有 `advertiseZstdAcceptEncoding` 开关，**默认关**，对话不带
//     zstd。zstd 只出现在浏览器（官网网页 `gzip, deflate, br, zstd`）和 Node
//     内置 fetch（`br, gzip, deflate`，网页登录码）上，都不在我们的路径里。
//
// 此前网关不设这个头，Go 只发 `Accept-Encoding: gzip`。那是每个请求都带、且与
// 官方不同的稳定特征——比 TCP TTL 之类更容易被一条规则捞出来。
package upstream

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/lzw"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
)

// acceptEncodingAxios 官方 axios http 适配器的默认值。
// 顺序照官方原样，别重排——它是逐字符可比的。
const acceptEncodingAxios = "gzip, compress, deflate, br"

// decodingBody 把「解压 reader」与「原 body 的 Close」绑在一起。
// 只包装 Read/Close，不缓冲：SSE 逐帧刷新与超长流都不受影响。
type decodingBody struct {
	io.Reader
	raw io.ReadCloser
}

func (b *decodingBody) Close() error { return b.raw.Close() }

// decodeResponseBody 按 Content-Encoding 包一层解压 reader。
// 无编码 / identity / 不支持的编码（含 compress）一律原样返回。
//
// 解压成功时清掉 Content-Encoding 并把 ContentLength 置 -1：响应体已经不再是
// 编码后的字节，留着这两个字段会让下游按错长度读或二次解压。
func decodeResponseBody(resp *http.Response) (io.ReadCloser, error) {
	if resp == nil || resp.Body == nil {
		return nil, errors.New("decode response: nil body")
	}
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return resp.Body, nil
	}

	var (
		r   io.Reader
		err error
	)
	switch enc {
	case "gzip", "x-gzip":
		r, err = gzip.NewReader(resp.Body)
	case "br":
		r = brotli.NewReader(resp.Body)
	case "deflate":
		// HTTP 的 deflate 名义上是 zlib 包装，但历史上有服务器直接发裸 deflate
		// 流。按 zlib 头两字节判断：CMF 低 4 位是 8（deflate）且 CMF*256+FLG 能
		// 被 31 整除才是 zlib。
		var br *bufio.Reader
		br, err = newDeflateSource(resp.Body)
		if err == nil {
			r, err = newDeflateReader(br)
		}
	case "compress":
		// HTTP 的 compress 编码是 LZW（UNIX compress）。既然请求头里声明了它，
		// 就必须能解——"声明支持但不实现"是比不支持更容易出事的状态：真返回时
		// 上层会拿到压缩字节、JSON 直接解析失败。参数取 UNIX compress 的标准
		// 形态（LSB-first，8 位字宽）。实际服务器几乎不会用这个编码，但成本是
		// 一行，缺口的代价是一次请求直接挂掉。
		r = lzw.NewReader(resp.Body, lzw.LSB, 8)
	default:
		// zstd / 未知编码：原样返回。不硬解未知格式，避免把可用的响应弄坏。
		return resp.Body, nil
	}
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", enc, err)
	}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	return &decodingBody{Reader: r, raw: resp.Body}, nil
}

// newDeflateSource 返回一个可 Peek 的读取源（失败时返回错误前先不消费字节）。
func newDeflateSource(body io.ReadCloser) (*bufio.Reader, error) {
	br := bufio.NewReader(body)
	if _, err := br.Peek(2); err != nil {
		return nil, err
	}
	return br, nil
}

// newDeflateReader 按首两字节分 zlib 与裸 deflate。
func newDeflateReader(br *bufio.Reader) (io.Reader, error) {
	head, err := br.Peek(2)
	if err != nil {
		return nil, err
	}
	if head[0]&0x0f == 8 && (uint16(head[0])<<8|uint16(head[1]))%31 == 0 {
		return zlib.NewReader(br)
	}
	return flate.NewReader(br), nil
}
