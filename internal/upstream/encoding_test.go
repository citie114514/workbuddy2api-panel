package upstream

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/lzw"
	"compress/zlib"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
)

// compressWith 用指定编码压一段明文（"identity" 原样返回）。
func compressWith(t *testing.T, enc, plain string) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.WriteCloser
	var err error
	switch enc {
	case "gzip":
		w = gzip.NewWriter(&buf)
	case "br":
		w = brotli.NewWriter(&buf)
	case "zlib": // 带 zlib 头的 deflate
		w = zlib.NewWriter(&buf)
	case "deflate": // 裸 deflate 流（无 zlib 头）
		w, err = flate.NewWriter(&buf, flate.DefaultCompression)
	case "compress": // HTTP 的 compress = LZW（UNIX compress 形态）
		w = lzw.NewWriter(&buf, lzw.LSB, 8)
	case "identity":
		return []byte(plain)
	default:
		t.Fatalf("unknown enc %s", enc)
	}
	if err != nil {
		t.Fatalf("new writer %s: %v", enc, err)
	}
	if _, err := w.Write([]byte(plain)); err != nil {
		t.Fatalf("write %s: %v", enc, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close %s: %v", enc, err)
	}
	return buf.Bytes()
}

// TestRoundTripAcceptEncodingAndDecode 锁住两件事：
//  1. 出站自动带官方 axios 的 Accept-Encoding（此前只有 Go 默认的 gzip）；
//  2. 设了这个头之后，gzip / br / zlib-deflate / 裸 deflate 四种响应都能被正确
//     解开——这是「设了头就由自己解压」的必然配套，漏一种就是线上乱码。
func TestRoundTripAcceptEncodingAndDecode(t *testing.T) {
	const plain = `{"code":0,"msg":"OK","data":{"hello":"你好"}}`
	cases := []struct {
		name       string
		headerEnc  string // 响应里的 Content-Encoding
		compressAs string // 实际压缩算法
	}{
		{"gzip", "gzip", "gzip"},
		{"br", "br", "br"},
		{"deflate-zlib", "deflate", "zlib"},
		{"deflate-raw", "deflate", "deflate"},
		{"identity", "", "identity"},
		{"compress-LZW", "compress", "compress"},
		{"unknown-encoding 原样返回", "zstd", "identity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotAE string
			body := compressWith(t, tc.compressAs, plain)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAE = r.Header.Get("Accept-Encoding")
				if tc.headerEnc != "" {
					w.Header().Set("Content-Encoding", tc.headerEnc)
				}
				_, _ = w.Write(body)
			}))
			defer srv.Close()

			cl := &http.Client{Transport: newUIDTransport()}
			resp, err := cl.Get(srv.URL)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if gotAE != acceptEncodingAxios {
				t.Errorf("Accept-Encoding=%q want %q", gotAE, acceptEncodingAxios)
			}
			if string(got) != plain {
				t.Errorf("body=%q want %q", got, plain)
			}
			// 解压后不能留着 Content-Encoding / Content-Length：否则下游会按压缩
			// 长度读、或对已解开的字节再解一次。
			if tc.compressAs != "identity" {
				if ce := resp.Header.Get("Content-Encoding"); ce != "" {
					t.Errorf("解压后 Content-Encoding 应清空，实际 %q", ce)
				}
				if resp.ContentLength != -1 {
					t.Errorf("解压后 ContentLength 应为 -1，实际 %d", resp.ContentLength)
				}
			}
		})
	}
}

// TestRoundTripExplicitAcceptEncodingNotOverridden 调用方显式设了值时不覆盖。
func TestRoundTripExplicitAcceptEncodingNotOverridden(t *testing.T) {
	var gotAE string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAE = r.Header.Get("Accept-Encoding")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	cl := &http.Client{Transport: newUIDTransport()}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if gotAE != "identity" {
		t.Errorf("Accept-Encoding=%q want identity（显式值不应被覆盖）", gotAE)
	}
}

// TestRoundTripStreamNotBuffered 解压层不得把流缓冲起来：SSE 首帧必须能立刻出来
// （网关的 TTFT 全靠这一点）。这里用 gzip 包一个长流，写一段就读一段。
func TestRoundTripStreamNotBuffered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/event-stream")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write([]byte("data: first\n\n"))
		_ = gz.Flush()
		w.(http.Flusher).Flush()
		_, _ = gz.Write([]byte("data: second\n\n"))
		_ = gz.Close()
	}))
	defer srv.Close()

	cl := &http.Client{Transport: newUIDTransport()}
	resp, err := cl.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 12)
	n, err := io.ReadFull(resp.Body, buf)
	if err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "first") {
		t.Errorf("首帧=%q 未包含 first（说明解压层缓冲了整条流）", buf[:n])
	}
}
