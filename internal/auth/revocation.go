package auth

import (
	"context"
	"database/sql"
	"encoding/hex"
	"sync"
	"sync/atomic"

	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Stream revocation (SM-AUTH-007, ADR-0033). Every request is authenticated and authorized when it arrives, so
// revocation applies to the next request at once. Long-lived streams (the change feed, the live terminal) hold a
// StreamWatch: after a transaction that revokes a session, a token or a user's sessions, or changes grants, commits,
// the matching watches are signalled and their streams re-authorize immediately. A periodic re-check is the backstop
// for expiry, idle timeout and anything revoked outside this process.
//
// Signals carry no data and never fail: each watch has a one-slot channel, so a burst of revocations coalesces into a
// single re-check and none is lost.

// revocation names what a committed transaction revoked: all, or one of sessionKey, sessionID (with userID), userID
// or tokenID.
type revocation struct {
	all        bool   // grants changed: every stream re-checks its authorization
	userID     string // the user's sessions were revoked or the user changed (disabled, password, MFA)
	sessionKey string // one session (hex of its token hash)
	sessionID  string // one session by display ID (with userID)
	tokenID    string // an API token or the root token
}

// Revocations fans revocation signals out to the streams they may affect.
type Revocations struct {
	mu      sync.Mutex
	watches map[*StreamWatch]struct{}
	// gen counts signals; a principal remembers the value it was authenticated under, so a revocation that commits
	// between authentication and Watch is not missed.
	gen atomic.Uint64
}

// NewRevocations returns an empty notifier.
func NewRevocations() *Revocations { return &Revocations{watches: map[*StreamWatch]struct{}{}} }

// StreamWatch receives a signal on C whenever a committed revocation may affect its principal. The stream then
// re-authorizes; a signal never means "revoked" by itself.
type StreamWatch struct {
	C          <-chan struct{}
	c          chan struct{}
	userID     string
	sessionKey string
	sessionID  string
	tokenID    string
	r          *Revocations
	once       sync.Once
}

// Watch registers a stream authenticated as p. Close it when the stream ends.
func (r *Revocations) Watch(p *Principal) *StreamWatch {
	c := make(chan struct{}, 1)
	w := &StreamWatch{C: c, c: c, r: r}
	if p != nil {
		if p.User != nil {
			w.userID = p.User.ID
		}
		if len(p.tokenHash) > 0 {
			w.sessionKey, w.sessionID = hex.EncodeToString(p.tokenHash), p.SessionID
		}
		w.tokenID = p.TokenID
		if p.RootTokenID != "" {
			w.tokenID = p.RootTokenID
		}
	}
	r.mu.Lock()
	r.watches[w] = struct{}{}
	r.mu.Unlock()
	if p == nil || r.gen.Load() != p.revGen {
		w.signal() // something was revoked after p was authenticated: re-check at once
	}
	return w
}

// Close unregisters the watch.
func (w *StreamWatch) Close() {
	w.once.Do(func() {
		w.r.mu.Lock()
		delete(w.r.watches, w)
		w.r.mu.Unlock()
	})
}

func (w *StreamWatch) signal() {
	select {
	case w.c <- struct{}{}:
	default: // a re-check is already pending
	}
}

func (w *StreamWatch) matches(rev revocation) bool {
	switch {
	case rev.all:
		return true
	case rev.sessionKey != "":
		return w.sessionKey == rev.sessionKey
	case rev.sessionID != "":
		return w.sessionID == rev.sessionID && w.userID == rev.userID
	case rev.userID != "":
		return w.userID == rev.userID
	case rev.tokenID != "":
		return w.tokenID == rev.tokenID
	}
	return false
}

// Active returns the number of registered watches (metrics and tests).
func (r *Revocations) Active() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.watches)
}

func (r *Revocations) signal(rev revocation) {
	r.gen.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	for w := range r.watches {
		if w.matches(rev) {
			w.signal()
		}
	}
}

// Revocations returns the service's stream revocation notifier.
func (svc *Service) Revocations() *Revocations { return svc.rev }

// revokedAfterCommit signals rev once tx commits (never inside the transaction, and never for a rollback).
func (svc *Service) revokedAfterCommit(tx *store.Tx, rev revocation) {
	tx.AfterCommit(func() { svc.rev.signal(rev) })
}

// revokeUserSessionsTx revokes every session of a user except the one whose token hashes to except (nil or empty:
// all), and signals the user's streams after commit. The user's API-token streams re-check too: the change that
// revoked the sessions (disable, password, MFA, grants) may affect them.
func (svc *Service) revokeUserSessionsTx(ctx context.Context, tx *store.Tx, userID, reason string, except []byte) error {
	if except == nil {
		except = []byte{}
	}
	if _, err := tx.Q.RevokeUserSessions(ctx, db.RevokeUserSessionsParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
		Reason: sql.NullString{String: reason, Valid: true}, UserID: userID, ExceptHash: except}); err != nil {
		return err
	}
	svc.revokedAfterCommit(tx, revocation{userID: userID})
	return nil
}

// revokeWebSessionTx revokes one session by token hash and signals its streams after commit.
func (svc *Service) revokeWebSessionTx(ctx context.Context, tx *store.Tx, tokenHash []byte, reason string) (int64, error) {
	n, err := tx.Q.RevokeWebSession(ctx, db.RevokeWebSessionParams{RevokedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
		RevokeReason: sql.NullString{String: reason, Valid: true}, IDHash: tokenHash})
	if err == nil && n > 0 {
		svc.revokedAfterCommit(tx, revocation{sessionKey: hex.EncodeToString(tokenHash)})
	}
	return n, err
}
