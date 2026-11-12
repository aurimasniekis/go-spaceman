package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestHandler(t *testing.T) {
	files := fstest.MapFS{
		"index.html":                    {Data: []byte("<!doctype html><title>SpaceMan</title>")},
		"favicon.svg":                   {Data: []byte("<svg/>")},
		"assets/app-abc123.js":          {Data: []byte("console.log(1)")},
		"assets/nested/y.js":            {Data: []byte("console.log(2)")},
		"assets/ghostty-vt-abc123.wasm": {Data: []byte("\x00asm\x01\x00\x00\x00")},
	}
	h := Handler(files)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	if r := get("/requests/req_1"); r.Code != 200 || !strings.Contains(r.Body.String(), "SpaceMan") || r.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("SPA route: %d %v", r.Code, r.Header())
	}
	if r := get("/"); !strings.Contains(r.Header().Get("Content-Security-Policy"), "'wasm-unsafe-eval'") ||
		strings.Contains(r.Header().Get("Content-Security-Policy"), "'unsafe-eval'") || r.Header().Get("X-Frame-Options") != "DENY" ||
		!strings.Contains(r.Header().Get("Content-Security-Policy"), "connect-src 'self';") {
		t.Fatalf("UI security headers: %v", r.Header())
	}
	if r := get("/assets/app-abc123.js"); r.Code != 200 || !strings.Contains(r.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d %v", r.Code, r.Header())
	}
	// The terminal's WebAssembly module (ADR-0076): compileStreaming needs application/wasm, and it is cached for good.
	if r := get("/assets/ghostty-vt-abc123.wasm"); r.Code != 200 || r.Header().Get("Content-Type") != "application/wasm" ||
		r.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("wasm asset: %d %v", r.Code, r.Header())
	}
	// A chunk of another build (a tab opened before an upgrade) is a plain 404 that nothing caches, never the SPA HTML.
	for _, p := range []string{"/assets/missing.js", "/assets/spaces.index-OLD.js", "/assets/composer-OLD.css", "/assets/nested/x.js", "/assets/nested"} {
		r := get(p)
		if r.Code != 404 || strings.Contains(r.Body.String(), "<title>") || r.Header().Get("Cache-Control") != "no-store" ||
			!strings.HasPrefix(r.Header().Get("Content-Type"), "text/plain") || r.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("missing asset %s: %d %v %q", p, r.Code, r.Header(), r.Body.String())
		}
	}
	// index.html is never long-cached, on any route, so a tab revalidates it and learns the current chunk names.
	for _, p := range []string{"/", "/spaces", "/terminals", "/index.html"} {
		if r := get(p); r.Code != 200 || r.Header().Get("Cache-Control") != "no-cache" || !strings.HasPrefix(r.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("index %s: %d %v", p, r.Code, r.Header())
		}
	}
	if r := get("/favicon.svg"); r.Code != 200 {
		t.Fatalf("favicon: %d", r.Code)
	}
	if r := get("/../../etc/passwd"); r.Code != 200 || strings.Contains(r.Body.String(), "root:") {
		t.Fatalf("traversal: %d", r.Code)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != 405 {
		t.Fatalf("POST: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	Handler(nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 404 {
		t.Fatalf("no UI: %d", rec.Code)
	}
	if _, ok := Dist(); ok {
		t.Fatal("Dist without embedui tag")
	}
}
