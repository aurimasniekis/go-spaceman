// Package webui serves the operator web application (SM-UI-001). Release and container builds embed web/dist with
// the embedui build tag; other builds serve a short notice instead.
package webui

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// CSP is the policy for UI pages (ADR-0020): Pulse's policy plus WebAssembly compilation for the ghostty-web terminal,
// whose module is a hashed asset of the build (ADR-0076). The API keeps its own `default-src 'none'` policy.
const CSP = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; " +
	"font-src 'self' data:; connect-src 'self'; worker-src 'self' blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; " +
	"object-src 'none'"

// securityHeaders sets the browser hardening headers on every UI response.
func securityHeaders(h http.Header) {
	h.Set("Content-Security-Policy", CSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

// Handler serves the UI: hashed assets are immutable and cached, every other path gets index.html (client-side
// routing) with no-cache, so a tab always revalidates it and learns the current chunk names. Missing asset files are
// 404 with no-store, never index.html, so a stale reference fails visibly and the 404 is not cached.
func Handler(files fs.FS) http.Handler {
	if files == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, "The SpaceMan web UI is not included in this build (build with -tags embedui after `pnpm build`).\n")
		})
	}
	assets := http.FileServerFS(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w.Header())
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := path.Clean("/" + r.URL.Path)
		if strings.HasPrefix(p, "/assets/") || isStaticFile(files, p) {
			if fi, err := fs.Stat(files, strings.TrimPrefix(p, "/")); err != nil || fi.IsDir() {
				// An asset of another build (a tab opened before an upgrade): a real 404 that no browser or proxy
				// keeps, so the client's import fails cleanly and a reload picks up the current build.
				w.Header().Set("Cache-Control", "no-store")
				http.NotFound(w, r)
				return
			}
			if strings.HasPrefix(p, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			assets.ServeHTTP(w, r)
			return
		}
		index, err := fs.ReadFile(files, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}

// isStaticFile reports top-level files such as /favicon.svg (but not index.html, which is served for every route, and
// never tg.html, which only the Mini App host serves).
func isStaticFile(files fs.FS, p string) bool {
	name := strings.TrimPrefix(p, "/")
	if name == "" || name == "index.html" || name == MiniAppEntry || strings.Contains(name, "/") || !strings.Contains(name, ".") {
		return false
	}
	_, err := fs.Stat(files, name)
	return err == nil
}

// MiniAppEntry is the Mini App's HTML shell, a second Vite entry of the same build (ADR-0083 §4.6). The main host
// never serves it (isStaticFile), and the Mini App host serves nothing else as a page.
const MiniAppEntry = "tg.html"

// MiniAppScriptURL is the one script allowed from outside the host: Telegram's official loader, pinned by its exact URL
// (a CSP source with a path matches that path only), not by host.
const MiniAppScriptURL = "https://telegram.org/js/telegram-web-app.js"

// MiniAppCSP is the Mini App page's policy (plan §4.6). It differs from CSP in the Telegram loader's exact URL and in
// frame-ancestors, which admits Telegram Web's iframe only. 'self' is the Mini App host, so the shell cannot call the
// main host. No 'unsafe-inline' for scripts.
const MiniAppCSP = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval' " + MiniAppScriptURL + "; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; worker-src 'self' blob:; " +
	"frame-ancestors https://web.telegram.org; base-uri 'none'; form-action 'self'; object-src 'none'"

// miniAppHeaders sets the Mini App host's page headers. X-Frame-Options is deliberately absent: DENY would forbid the
// Telegram Web iframe, and frame-ancestors already names the only embedder.
func miniAppHeaders(h http.Header) {
	h.Set("Content-Security-Policy", MiniAppCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

// MiniAppHandler serves the Mini App host's static side: the hashed build assets under /assets/ (the same files the
// main UI serves: static code, nothing user-specific) and tg.html for every other GET (the shell's client routes). The
// API, the signed downloads and everything else are routed before it. With no tg.html in the build it answers 404.
func MiniAppHandler(files fs.FS) http.Handler {
	var assets http.Handler
	if files != nil {
		assets = http.FileServerFS(files)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		miniAppHeaders(w.Header())
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if files == nil {
			w.Header().Set("Cache-Control", "no-store")
			http.NotFound(w, r)
			return
		}
		if p := path.Clean("/" + r.URL.Path); strings.HasPrefix(p, "/assets/") {
			if fi, err := fs.Stat(files, strings.TrimPrefix(p, "/")); err != nil || fi.IsDir() {
				w.Header().Set("Cache-Control", "no-store")
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			assets.ServeHTTP(w, r)
			return
		}
		page, err := fs.ReadFile(files, MiniAppEntry)
		if err != nil {
			w.Header().Set("Cache-Control", "no-store")
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(page)
	})
}
