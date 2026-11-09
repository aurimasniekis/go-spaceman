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
// whose module is inlined as a data: URL. The API keeps its own `default-src 'none'` policy.
const CSP = "default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; " +
	"font-src 'self' data:; connect-src 'self' data:; worker-src 'self' blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; " +
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

// isStaticFile reports top-level files such as /favicon.svg (but not index.html, which is served for every route).
func isStaticFile(files fs.FS, p string) bool {
	name := strings.TrimPrefix(p, "/")
	if name == "" || name == "index.html" || strings.Contains(name, "/") || !strings.Contains(name, ".") {
		return false
	}
	_, err := fs.Stat(files, name)
	return err == nil
}
