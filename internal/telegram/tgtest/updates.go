package tgtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// SecretHeader carries setWebhook's secret_token on every webhook request (API#setwebhook).
const SecretHeader = "X-Telegram-Bot-Api-Secret-Token" //nolint:gosec // G101: a header name, not a credential

// maxPollWait caps a getUpdates long poll so a test never hangs on a large timeout.
const maxPollWait = 30 * time.Second

// prepareLocked gives u the next update_id when it has none and records its callback query as answerable.
func (s *Server) prepareLocked(u *botapi.Update) {
	if u.UpdateID == 0 {
		u.UpdateID = s.nextUpd
	}
	if u.UpdateID >= s.nextUpd {
		s.nextUpd = u.UpdateID + 1
	}
	if q := u.CallbackQuery; q != nil && q.ID != "" {
		if _, ok := s.callbacks[q.ID]; !ok {
			s.callbacks[q.ID] = &callbackState{}
		}
	}
}

// PushUpdate queues u for getUpdates and returns its update_id (assigned when u has none).
func (s *Server) PushUpdate(u botapi.Update) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prepareLocked(&u)
	s.updates = append(s.updates, u)
	close(s.updateCh)
	s.updateCh = make(chan struct{})
	return u.UpdateID
}

// Pending returns the updates getUpdates hasn't had confirmed yet.
func (s *Server) Pending() []botapi.Update {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]botapi.Update(nil), s.updates...)
}

func (s *Server) getUpdates(r *http.Request, p params) (any, *apiError) {
	limit := MaxUpdatesPerPoll
	if n, ok := p.int64("limit"); ok {
		if n < 1 || n > MaxUpdatesPerPoll {
			return nil, badRequest("limit must be within 1..100")
		}
		limit = int(n)
	}
	timeout, _ := p.int64("timeout")
	offset, hasOffset := p.int64("offset")
	deadline := time.NewTimer(min(time.Duration(max(timeout, 0))*time.Second, maxPollWait))
	defer deadline.Stop()
	for {
		s.mu.Lock()
		if s.webhook.URL != "" {
			s.mu.Unlock()
			return nil, &apiError{code: http.StatusConflict,
				desc: "Conflict: can't use getUpdates method while webhook is active; use deleteWebhook to delete the webhook first"}
		}
		if hasOffset {
			s.confirmLocked(offset)
		}
		if len(s.updates) > 0 || timeout <= 0 {
			n := min(limit, len(s.updates))
			out := append([]botapi.Update{}, s.updates[:n]...)
			s.mu.Unlock()
			return out, nil
		}
		ch := s.updateCh
		s.mu.Unlock()
		select {
		case <-ch:
		case <-deadline.C:
			timeout = 0
		case <-r.Context().Done():
			return []botapi.Update{}, nil
		}
	}
}

// confirmLocked drops updates confirmed by offset: those below it, or for a negative offset all but the last -offset
// (API#getupdates).
func (s *Server) confirmLocked(offset int64) {
	if offset < 0 {
		if keep := int(-offset); keep < len(s.updates) {
			s.updates = s.updates[len(s.updates)-keep:]
		}
		return
	}
	i := 0
	for i < len(s.updates) && s.updates[i].UpdateID < offset {
		i++
	}
	s.updates = s.updates[i:]
}

// Deliver posts u to the webhook as Telegram would: to Options.WebhookTarget or the URL registered with setWebhook,
// with the registered secret in SecretHeader. It returns the HTTP status; a non-2xx status is recorded as the
// webhook's last error (getWebhookInfo).
func (s *Server) Deliver(ctx context.Context, u botapi.Update) (int, error) {
	s.mu.Lock()
	s.prepareLocked(&u)
	secret := s.secret
	s.mu.Unlock()
	body, err := json.Marshal(u)
	if err != nil {
		return 0, err
	}
	return s.DeliverRaw(ctx, secret, body)
}

// DeliverRaw posts body to the webhook target with secret in SecretHeader (no header when secret is empty), for
// tests of a wrong or missing secret, a malformed body or an oversized one.
func (s *Server) DeliverRaw(ctx context.Context, secret string, body []byte) (int, error) {
	s.mu.Lock()
	target := s.opts.WebhookTarget
	if target == "" {
		target = s.webhook.URL
	}
	s.mu.Unlock()
	if target == "" {
		return 0, errors.New("tgtest: no webhook registered and no WebhookTarget")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set(SecretHeader, secret)
	}
	hc := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := hc.Do(req)
	if err != nil {
		s.webhookError(err.Error())
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		s.webhookError(fmt.Sprintf("Wrong response from the webhook: %d %s", resp.StatusCode, http.StatusText(resp.StatusCode)))
	}
	return resp.StatusCode, nil
}

func (s *Server) webhookError(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.webhook.LastErrorDate = s.clk.Now().Unix()
	s.webhook.LastErrorMessage = msg
}

// WebhookSecret returns the secret_token registered with setWebhook ("" when none).
func (s *Server) WebhookSecret() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.secret
}
