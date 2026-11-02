package auth

import (
	"errors"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics/metricstest"
)

// HP-OBS-004: user login attempts are counted by method and outcome.
func TestLoginMetrics(t *testing.T) {
	f := newFx(t)
	m := metrics.New()
	f.svc = New(f.s, f.kr, audit.NewWriter(f.kr), eventbus.New(), mustAuthz(t, f.s), Options{Clock: f.clk, Metrics: m})
	f.bootstrap()
	for i := range 4 {
		_, _ = f.svc.Login(f.ctx, LoginInput{Email: "admin@example.com", Password: "wrong password!!", Meta: SessionMeta{IP: "203.0.113." + itoa(i+1)}})
	}
	if _, err := f.svc.Login(f.ctx, LoginInput{Email: "admin@example.com", Password: strongPW, Meta: SessionMeta{IP: "192.0.2.1"}}); !errors.Is(err, ErrThrottled) {
		t.Fatalf("want throttled, got %v", err)
	}
	must(t, f.svc.ClearThrottle(f.ctx, audit.System, f.bootstrapID()))
	if _, err := f.svc.Login(f.ctx, LoginInput{Email: "admin@example.com", Password: strongPW, Meta: SessionMeta{IP: "192.0.2.9"}}); err != nil {
		t.Fatal(err)
	}
	tok, _, err := f.svc.IssueRecoveryLogin(f.ctx, "", "rescue@example.com")
	must(t, err)
	_, err = f.svc.RecoveryLogin(f.ctx, tok, SessionMeta{})
	must(t, err)
	_, _ = f.svc.RecoveryLogin(f.ctx, tok, SessionMeta{}) // reused token
	f.svc.observeLogin(metrics.LoginPassword, errors.New("db down"))
	f.svc.observeLogin(metrics.LoginPassword, ErrLocalLoginDisabled) // not an attempt

	for _, c := range []struct {
		method, outcome string
		want            float64
	}{
		{"password", "failure", 4}, {"password", "throttled", 1}, {"password", "success", 1}, {"password", "error", 1},
		{"recovery", "success", 1}, {"recovery", "failure", 1},
	} {
		if v := metricstest.Value(t, m, "spaceman_user_logins_total", "method", c.method, "outcome", c.outcome); v != c.want {
			t.Errorf("%s/%s = %v, want %v", c.method, c.outcome, v, c.want)
		}
	}
}

func (f *fx) bootstrapID() string {
	op, err := f.s.R().GetUserByEmail(f.ctx, nullStr("admin@example.com"))
	must(f.t, err)
	return op.ID
}
