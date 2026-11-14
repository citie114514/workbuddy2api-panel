package server

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestMaskDataURLs base64 内容必须被剥掉（体积 + 隐私），但 MIME 与原始字节数要保留
// ——排障时要能看出「确实带了图、多大」。
func TestMaskDataURLs(t *testing.T) {
	blob := strings.Repeat("A", 200)
	in := `{"url":"data:image/png;base64,` + blob + `"}`
	got := maskDataURLs(in)
	if strings.Contains(got, blob) {
		t.Fatalf("base64 未被剥离: %s", got)
	}
	if !strings.Contains(got, "data:image/png;base64,<masked 200 bytes>") {
		t.Fatalf("应保留 MIME 与长度标记: %s", got)
	}
	// 短串不误伤（正文里偶尔出现的示例文本）。
	short := "data:text/plain;base64,QUJD"
	if got := maskDataURLs(short); got != short {
		t.Fatalf("短 base64 不应改写: %q -> %q", short, got)
	}
}

// TestOutboundBodyForLogTruncates 超长 body 截断到上限，截断点必须落在 rune 边界
// （日志按行解析，半个汉字会让下游解析出错）。
func TestOutboundBodyForLogTruncates(t *testing.T) {
	body := bytes.Repeat([]byte("a"), outboundBodyLogLimit-1)
	body = append(body, []byte("中文中文中文")...)
	got := outboundBodyForLog(body)
	if !utf8.ValidString(got) {
		t.Fatal("截断后不是合法 UTF-8")
	}
	if !strings.Contains(got, "<truncated total=") {
		t.Fatalf("缺少截断标记: %q", got)
	}
	small := []byte(`{"model":"glm-5.3"}`)
	if got := outboundBodyForLog(small); got != string(small) {
		t.Fatalf("未超限不应改写: %q", got)
	}
}

// TestOutboundBodyLoggedOn4xx 开关打开时 4xx 必须把出站 body 落日志；关闭时一行都不许有。
//
// 这是排障能力本身：上游 400 只说「参数不合法」，成因（stop 类型、工具配对、参数越界）
// 只在出站 body 里。默认关闭是因为 body 含用户内容。
func TestOutboundBodyLoggedOn4xx(t *testing.T) {
	const marker = "OUTBOUND_MARK"
	for _, enabled := range []bool{true, false} {
		var buf bytes.Buffer
		prevOut, prevFlags := log.Writer(), log.Flags()
		log.SetOutput(&buf)
		t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

		up := newFakeUpstream(t, func(string) (int, string, bool) {
			return 400, `{"code":11101,"msg":"bad params"}`, false
		})
		p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
		h := NewHandler(Config{Pool: p, Upstream: up, DebugLogOutboundBody: enabled})

		body := `{"model":"glm-5.2","messages":[{"role":"user","content":"` + marker + `"}]}`
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code=%d want 400", rec.Code)
		}
		out := buf.String()
		hasLine := strings.Contains(out, "outbound_4xx")
		if enabled && !hasLine {
			t.Fatalf("开关打开却没有 outbound_4xx 行:\n%s", out)
		}
		if enabled && !strings.Contains(out, marker) {
			t.Fatalf("日志里没有出站 body 内容:\n%s", out)
		}
		if !enabled && hasLine {
			t.Fatalf("开关关闭却写入了 outbound_4xx 行:\n%s", out)
		}
	}
}

// TestOutboundBodyNotLoggedOn429 429 不记：排队/限流一天几十条，每条带一整份长上下文
// 会把日志里有用的线索淹掉。
func TestOutboundBodyNotLoggedOn429(t *testing.T) {
	var buf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prevOut) })

	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 429, `{"code":6004,"msg":"rate limited"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, DebugLogOutboundBody: true})
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"MARK429"}]}`
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if strings.Contains(buf.String(), "outbound_4xx") {
		t.Fatalf("429 不应写 outbound_4xx://n%s", buf.String())
	}
}

// blockingBody 一直读到 ctx 结束才返回错误，用来模拟「上游已建连、客户端中途断开」。
type blockingBody struct{ ctx context.Context }

func (b blockingBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b blockingBody) Close() error             { return nil }

// TestClientCanceledRecords499 调用方断开不得记成 502/503：
//   - 502 upstream_parse 会让人以为是上游解析问题；
//   - 503 no_healthy_account 会让人以为号池挂了。
//
// 两条都是假故障。取消的请求应记 499（nginx 口径）+ outcome=interrupted，且不罚账号
// （fail 会解绑粘性，把人自己取消的请求算成账号问题）。
func TestClientCanceledRecords499(t *testing.T) {
	cases := []struct {
		name string
		// preCancel=true：请求发出前就已取消（轮转立即终止 → 末端分支）
		// preCancel=false：上游已建连、读的过程中取消（非流式聚合分支）
		preCancel bool
	}{
		{"canceled-before-send", true},
		{"canceled-mid-read", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{}, 1)

			// 两种取消时机都要走真实路径：假上游必须尊重 ctx，否则取消不可见
			//（不尊重 ctx 的桩会让整条链路"成功"，测出来是假绿）。
			up := &upstream.Client{
				HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					select {
					case started <- struct{}{}:
					default:
					}
					if tc.preCancel {
						if err := r.Context().Err(); err != nil {
							return nil, err // 建连前就已取消
						}
						return &http.Response{
							StatusCode: 200,
							Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
							Body:       io.NopCloser(strings.NewReader(sseOK)),
						}, nil
					}
					return &http.Response{
						StatusCode: 200,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       io.NopCloser(blockingBody{ctx: r.Context()}),
					}, nil
				})},
				ChatBaseCN: "https://fake.example",
			}

			reqLog := reqlog.New(reqlog.Config{})
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
			h := NewHandler(Config{Pool: p, Upstream: up, RequestLog: reqLog})

			req := httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
			rec := httptest.NewRecorder()

			done := make(chan struct{})
			go func() { h.ServeHTTP(rec, req); close(done) }()

			if tc.preCancel {
				cancel()
			} else {
				select {
				case <-started:
					cancel()
				case <-time.After(2 * time.Second):
					t.Fatal("上游未被调用")
				}
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("取消后请求未返回")
			}

			s := reqLog.Snapshot()
			if len(s.Recent) != 1 {
				t.Fatalf("recent=%d want 1", len(s.Recent))
			}
			if got := s.Recent[0].Status; got != statusClientClosedRequest {
				t.Errorf("status=%d want 499", got)
			}
			if got := s.Recent[0].Outcome; got != reqlog.OutcomeInterrupted {
				t.Errorf("outcome=%q want %q", got, reqlog.OutcomeInterrupted)
			}
			st, _ := p.Status("u1")
			if st.Cooling || st.ErrTotal != 0 || st.InFlight != 0 {
				t.Errorf("取消的请求不得罚账号: %+v", st)
			}
		})
	}
}
