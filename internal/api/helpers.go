package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

func jsonEncode(w io.Writer, v any) error { return json.NewEncoder(w).Encode(v) }

// Actor returns the audit actor of the request's principal.
func Actor(ctx context.Context) audit.Actor {
	p := PrincipalFrom(ctx)
	switch {
	case p == nil:
		return audit.Actor{Type: audit.ActorSystem, ID: "anonymous"}
	case p.RootTokenID != "":
		return audit.Actor{Type: audit.ActorRoot, ID: p.RootTokenID}
	case p.Internal():
		// A SpaceMan MCP call (ADR-0063): the owner's authority, through the server's internal token, for a run.
		return audit.Actor{Type: audit.ActorUser, ID: p.User.ID, Via: auth.ViaSpacemanMCP + p.TokenID, Run: p.RunID}
	case p.TokenID != "":
		return audit.Actor{Type: audit.ActorUser, ID: p.User.ID, Via: "api_token:" + p.TokenID}
	case p.Method == auth.MethodOIDC:
		// SSO sessions act with the user's SSO-mapped grants too; later rechecks (approvers) need to know (ADR-0029).
		return audit.Actor{Type: audit.ActorUser, ID: p.User.ID, Via: "sso:" + p.Provider}
	default:
		return audit.Actor{Type: audit.ActorUser, ID: p.User.ID}
	}
}

// IfMatch carries the expected resource version of an update (optimistic concurrency, SM-API-002).
type IfMatch struct {
	IfMatch string `header:"If-Match" doc:"The resource version this update was based on, as \"<version>\" (required)"`
}

// Version parses If-Match: "<n>" or W/"<n>". A missing header is 428 precondition_required.
func (m IfMatch) Version() (int64, error) {
	v := strings.TrimPrefix(strings.TrimSpace(m.IfMatch), "W/")
	if v == "" {
		return 0, apperr.New(apperr.CodePreconditionRequired, "If-Match with the resource version is required")
	}
	n, err := strconv.ParseInt(strings.Trim(v, `"`), 10, 64)
	if err != nil || n <= 0 {
		return 0, apperr.New(apperr.CodeInvalid, "If-Match must be a quoted positive version, e.g. \"3\"")
	}
	return n, nil
}

// ETag formats a version for responses.
func ETag(version int64) string { return `"` + strconv.FormatInt(version, 10) + `"` }

// IdempotencyKey is the optional header of idempotent creates.
type IdempotencyKey struct {
	Key string `header:"Idempotency-Key" doc:"Retry-safe creation: 8-128 printable ASCII characters; replays within 24 h return the original result"`
}

const idempotencyTTL = 24 * time.Hour

// Idempotent runs fn at most once per (principal, operation, key) within 24 h (SM-API-002). A replay with the same
// request returns the stored result (replayed=true); a different request with the same key is a conflict. When
// secret is set the result is not stored (it contains a one-time secret) and a replay is a conflict instead.
func Idempotent[O any](ctx context.Context, d *Deps, op string, key IdempotencyKey, request any, secret bool, fn func() (*O, error)) (*O, bool, error) {
	k := key.Key
	if k == "" {
		o, err := fn()
		return o, false, err
	}
	if len(k) < 8 || len(k) > 128 || strings.IndexFunc(k, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
		return nil, false, apperr.New(apperr.CodeInvalid, "Idempotency-Key must be 8-128 printable ASCII characters")
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, false, err
	}
	sum := sha256.Sum256(append([]byte(op+"\x00"), body...))
	principal := principalKeyOf(PrincipalFrom(ctx))
	// Concurrent requests with the same key (a client retrying while its first attempt is still running) run one at
	// a time, so the second sees the first one's stored result instead of creating a duplicate (security review
	// SR-L01). One central owns the database (SM-INV-001), so an in-process lock suffices.
	defer idempotencyLocks.lock(principal + "\x00" + op + "\x00" + k)()
	now := d.now()
	row, err := d.Store.R().GetIdempotency(ctx, db.GetIdempotencyParams{Principal: principal, Operation: op, Key: k, ExpiresAt: now.UnixMicro()})
	switch {
	case err == nil:
		if !bytes.Equal(row.RequestHash, sum[:]) {
			return nil, false, apperr.New(apperr.CodeConflict, "Idempotency-Key was already used for a different request")
		}
		if secret || row.Body == nil {
			return nil, false, apperr.New(apperr.CodeConflict, "this request already succeeded; its one-time secret cannot be shown again")
		}
		var o O
		if err := json.Unmarshal(row.Body, &o); err != nil {
			return nil, false, err
		}
		return &o, true, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, false, err
	}
	o, err := fn()
	if err != nil {
		return nil, false, err
	}
	var stored []byte
	if !secret {
		if stored, err = json.Marshal(o); err != nil {
			return nil, false, err
		}
	}
	_ = d.Store.W().TxNamed(ctx, "api.idempotency_record", func(tx *store.Tx) error {
		return tx.Q.InsertIdempotency(ctx, db.InsertIdempotencyParams{Principal: principal, Operation: op, Key: k, RequestHash: sum[:],
			Status: 200, Body: stored, CreatedAt: tx.NowMicros(), ExpiresAt: tx.Now.Add(idempotencyTTL).UnixMicro()})
	})
	return o, false, nil
}

// keyLocks are mutexes by key, created on demand and dropped when unused.
type keyLocks struct {
	mu   sync.Mutex
	keys map[string]*keyLock
}

type keyLock struct {
	mu   sync.Mutex
	refs int
}

func (l *keyLocks) lock(key string) func() {
	l.mu.Lock()
	if l.keys == nil {
		l.keys = map[string]*keyLock{}
	}
	e := l.keys[key]
	if e == nil {
		e = &keyLock{}
		l.keys[key] = e
	}
	e.refs++
	l.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(l.keys, key)
		}
		l.mu.Unlock()
	}
}

var idempotencyLocks keyLocks

func principalKeyOf(p *auth.Principal) string {
	switch {
	case p == nil:
		return "anonymous"
	case p.RootTokenID != "":
		return "root:" + p.RootTokenID
	case p.User != nil:
		return "user:" + p.User.ID
	}
	return "anonymous"
}

// Page are the cursor pagination query parameters (opaque server-issued cursors, SM-API-002).
type Page struct {
	Cursor string `query:"cursor" doc:"Opaque cursor from a previous response's next_cursor"`
	Limit  int    `query:"limit" minimum:"1" maximum:"200" default:"50"`
}

// cursor is the decoded position: the sort key and the ID of the last returned item.
type cursor struct {
	K string `json:"k"`
	I string `json:"i"`
}

// EncodeCursor returns an opaque cursor positioned after (key, id).
func EncodeCursor(key, id string) string {
	b, _ := json.Marshal(cursor{K: key, I: id})
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor decodes a cursor; "" is the first page. Malformed cursors are validation errors.
func DecodeCursor(s string) (key, id string, err error) {
	if s == "" {
		return "", "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	var c cursor
	if err != nil || json.Unmarshal(b, &c) != nil || c.I == "" || EncodeCursor(c.K, c.I) != s {
		return "", "", apperr.New(apperr.CodeInvalid, "invalid cursor")
	}
	return c.K, c.I, nil
}

func (p Page) limit() int {
	if p.Limit <= 0 {
		return 50
	}
	return min(p.Limit, 200)
}
