package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

var miniFiles = fstest.MapFS{
	"index.html":           {Data: []byte("<!doctype html><title>MainUI</title>")},
	"tg.html":              {Data: []byte("<!doctype html><title>MiniApp</title>")},
	"favicon.svg":          {Data: []byte("<svg/>")},
	"assets/app-abc123.js": {Data: []byte("console.log(1)")},
}

func miniGet(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

// The Mini App page's CSP exactly (plan §4.6): the Telegram loader by its full URL, no 'unsafe-inline' for scripts, only
// Telegram Web may frame it.
func TestMiniAppCSPExact(t *testing.T) {
	const want = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval' https://telegram.org/js/telegram-web-app.js; " +
		"style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; " +
		"worker-src 'self' blob:; frame-ancestors https://web.telegram.org; base-uri 'none'; form-action 'self'; object-src 'none'"
	if MiniAppCSP != want {
		t.Fatalf("CSP =\n%s\nwant\n%s", MiniAppCSP, want)
	}
	directives := map[string]string{}
	for _, d := range strings.Split(MiniAppCSP, ";") {
		name, val, _ := strings.Cut(strings.TrimSpace(d), " ")
		directives[name] = val
	}
	if directives["frame-ancestors"] != "https://web.telegram.org" {
		t.Errorf("frame-ancestors = %q", directives["frame-ancestors"])
	}
	script := directives["script-src"]
	if strings.Contains(script, "unsafe-inline") || strings.Contains(script, "unsafe-eval ") || strings.Contains(script, "*") {
		t.Errorf("script-src = %q", script)
	}
	var hosts []string
	for _, src := range strings.Fields(script) {
		if !strings.HasPrefix(src, "'") {
			hosts = append(hosts, src)
		}
	}
	if len(hosts) != 1 || hosts[0] != "https://telegram.org/js/telegram-web-app.js" {
		t.Errorf("script hosts = %v, want the exact loader URL only", hosts)
	}
	if directives["connect-src"] != "'self'" || directives["default-src"] != "'self'" {
		t.Errorf("connect/default = %q/%q", directives["connect-src"], directives["default-src"])
	}
}

func TestMiniAppHeaders(t *testing.T) {
	h := MiniAppHandler(miniFiles)
	for _, target := range []string{"/", "/tg.html", "/needs-you/hum_1", "/assets/app-abc123.js", "/assets/missing.js"} {
		r := miniGet(h, http.MethodGet, target)
		hd := r.Header()
		if hd.Get("Content-Security-Policy") != MiniAppCSP {
			t.Errorf("%s: CSP = %q", target, hd.Get("Content-Security-Policy"))
		}
		if _, set := hd["X-Frame-Options"]; set {
			t.Errorf("%s: X-Frame-Options must not be set (it would forbid Telegram Web's iframe)", target)
		}
		for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer",
			"Cross-Origin-Opener-Policy": "same-origin", "Cross-Origin-Resource-Policy": "same-origin"} {
			if hd.Get(k) != v {
				t.Errorf("%s: %s = %q, want %q", target, k, hd.Get(k), v)
			}
		}
		if hd.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: CORS header on the shell", target)
		}
	}
}

func TestMiniAppServing(t *testing.T) {
	h := MiniAppHandler(miniFiles)
	for _, target := range []string{"/", "/tg.html", "/index.html", "/needs-you/hum_1", "/favicon.svg"} {
		r := miniGet(h, http.MethodGet, target)
		if r.Code != 200 || !strings.Contains(r.Body.String(), "MiniApp") || strings.Contains(r.Body.String(), "MainUI") ||
			r.Header().Get("Cache-Control") != "no-cache" || !strings.HasPrefix(r.Header().Get("Content-Type"), "text/html") {
			t.Errorf("%s: %d %v %q", target, r.Code, r.Header(), r.Body.String())
		}
	}
	if r := miniGet(h, http.MethodGet, "/assets/app-abc123.js"); r.Code != 200 || !strings.Contains(r.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("asset: %d %v", r.Code, r.Header())
	}
	if r := miniGet(h, http.MethodGet, "/assets/missing.js"); r.Code != 404 || r.Header().Get("Cache-Control") != "no-store" || strings.Contains(r.Body.String(), "MiniApp") {
		t.Errorf("missing asset: %d %v", r.Code, r.Header())
	}
	if r := miniGet(h, http.MethodPost, "/"); r.Code != 405 {
		t.Errorf("POST = %d", r.Code)
	}
	// A build without tg.html (or without a UI) serves 404, never the main page.
	for _, f := range []fstest.MapFS{{"index.html": {Data: []byte("MainUI")}}, nil} {
		if r := miniGet(MiniAppHandler(f), http.MethodGet, "/"); r.Code != 404 || strings.Contains(r.Body.String(), "MainUI") {
			t.Errorf("no tg.html: %d %q", r.Code, r.Body.String())
		}
	}
}

// The main host never serves tg.html (it must only run with the Mini App CSP on the Mini App origin), and its own
// headers are unchanged.
func TestMainHandlerNeverServesTgHTML(t *testing.T) {
	h := Handler(miniFiles)
	for _, target := range []string{"/tg.html", "/", "/tg-dl/abc"} {
		r := miniGet(h, http.MethodGet, target)
		if r.Code != 200 || strings.Contains(r.Body.String(), "MiniApp") || !strings.Contains(r.Body.String(), "MainUI") {
			t.Errorf("%s: %d %q", target, r.Code, r.Body.String())
		}
		if r.Header().Get("Content-Security-Policy") != CSP || r.Header().Get("X-Frame-Options") != "DENY" ||
			!strings.Contains(r.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
			strings.Contains(r.Header().Get("Content-Security-Policy"), "telegram") {
			t.Errorf("%s: main headers changed: %v", target, r.Header())
		}
	}
}

// miniAppHeaders sets every header of the Mini App host's pages, whichever branch of the handler answers.
func TestMiniAppHeadersFunction(t *testing.T) {
	h := http.Header{}
	miniAppHeaders(h)
	for k, v := range map[string]string{"Content-Security-Policy": MiniAppCSP, "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer",
		"Cross-Origin-Opener-Policy": "same-origin", "Cross-Origin-Resource-Policy": "same-origin"} {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if r := miniGet(MiniAppHandler(miniFiles), http.MethodPost, "/"); r.Code != http.StatusMethodNotAllowed ||
		r.Header().Get("Cross-Origin-Opener-Policy") != "same-origin" || r.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" {
		t.Errorf("405: %d %v", r.Code, r.Header())
	}
}
