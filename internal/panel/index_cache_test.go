package panel

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// 面板静态资源必须同时做到：响应 no-store，且 HTML 引用带版本号。
// 前者约束浏览器即时刷新，后者绕开已按扩展名缓存的 CDN/代理。
func TestPanelStaticAssetsAreVersionedAndNoStore(t *testing.T) {
	p := New(Config{Version: "1.14.3-test"})

	indexRec := httptest.NewRecorder()
	p.index(indexRec, httptest.NewRequest("GET", "/panel/", nil))
	if got := indexRec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("index Cache-Control=%q want no-store", got)
	}
	body := indexRec.Body.String()
	for _, ref := range []string{
		`href="mobile.css?v=1.14.3-test"`,
		`src="app.js?v=1.14.3-test"`,
		`src="mobile.js?v=1.14.3-test"`,
	} {
		if !strings.Contains(body, ref) {
			t.Errorf("versioned index missing %s", ref)
		}
	}

	checks := []struct {
		name string
		fn   func(*httptest.ResponseRecorder)
	}{
		{"app.js", func(w *httptest.ResponseRecorder) { p.appScript(w, httptest.NewRequest("GET", "/panel/app.js", nil)) }},
		{"mobile.js", func(w *httptest.ResponseRecorder) {
			p.mobileScript(w, httptest.NewRequest("GET", "/panel/mobile.js", nil))
		}},
		{"mobile.css", func(w *httptest.ResponseRecorder) {
			p.mobileStyle(w, httptest.NewRequest("GET", "/panel/mobile.css", nil))
		}},
	}
	for _, c := range checks {
		rec := httptest.NewRecorder()
		c.fn(rec)
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s Cache-Control=%q want no-store", c.name, got)
		}
	}
}
