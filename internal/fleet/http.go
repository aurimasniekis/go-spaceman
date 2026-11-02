package fleet

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// EnrollPath is the node enrollment endpoint.
const EnrollPath = "/agent/v1/enroll"

// AgentHandler serves the node-facing endpoints: enrollment and the WebSocket connection. They use node credentials,
// never user sessions or API tokens (and the administrative API never accepts node credentials).
func (svc *Service) AgentHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET "+proto.ConnectPath, svc.hub)
	lim := &limiter{m: map[string]*rate.Limiter{}}
	mux.HandleFunc("POST "+EnrollPath, func(w http.ResponseWriter, r *http.Request) {
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		if !lim.allow(ip) {
			http.Error(w, "too many enrollment attempts", http.StatusTooManyRequests)
			return
		}
		var in struct {
			Token string `json:"token"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		res, err := svc.Enroll(r.Context(), in.Token, ip)
		if err != nil {
			code := http.StatusInternalServerError
			if apperr.CodeOf(err) == apperr.CodeUnauthorized {
				code = http.StatusUnauthorized
			}
			http.Error(w, http.StatusText(code), code)
			if code == http.StatusInternalServerError && !errors.Is(err, r.Context().Err()) {
				svc.opts.Log.Error("enroll", "error.message", err.Error())
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(res)
	})
	return mux
}

type limiter struct {
	mu sync.Mutex
	m  map[string]*rate.Limiter
}

func (l *limiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) > 10000 {
		l.m = map[string]*rate.Limiter{}
	}
	li := l.m[ip]
	if li == nil {
		li = rate.NewLimiter(rate.Every(6*time.Second), 10)
		l.m[ip] = li
	}
	return li.Allow()
}
