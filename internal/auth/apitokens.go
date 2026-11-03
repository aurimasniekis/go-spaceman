package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Automation (API) tokens: `spaceman_at_<43 base64url chars>` bearer tokens for Terraform, the CLI and scripts
// (SM-AUTH-006). A token acts for the user who created it with its own scoped grant set, which must be covered by the
// creator's grants when issued. Every request is authorized for both the token and its creator, so a token stops
// working for anything its creator lost, and entirely when the creator is disabled. Only a keyed MAC is stored.
const (
	APITokenPrefix = "spaceman_at_"
	MinAPITokenTTL = 24 * time.Hour
	MaxAPITokenTTL = 366 * 24 * time.Hour
	// apiTokenTouchEvery bounds the writes that record when a token was last used.
	apiTokenTouchEvery = time.Minute
)

// neverExpiresAt is the expires_at stored for tokens without expiry (ADR-0053), so every expiry comparison holds
// unchanged.
var neverExpiresAt = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

// APIToken describes an automation token (never the token itself).
type APIToken struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	UserID    string        `json:"user_id"`
	Grants    []authz.Grant `json:"grants"`
	Hint      string        `json:"hint"`
	CreatedAt time.Time     `json:"created_at"`
	// ExpiresAt is nil when the token never expires.
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	NeverExpires bool       `json:"never_expires"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	LastUsedIP   string     `json:"last_used_ip,omitempty"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	RevokedBy    string     `json:"revoked_by,omitempty"`
	// ParentTokenID is the API token that issued this one, if any (ADR-0058).
	ParentTokenID string `json:"parent_token_id,omitempty"`
}

func apiTokenFromRow(r db.ApiToken) *APIToken {
	t := &APIToken{ID: r.ID, Name: r.Name, UserID: r.UserID, Grants: []authz.Grant{}, Hint: r.Hint,
		CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), NeverExpires: r.NeverExpires == 1, LastUsedIP: r.LastUsedIp.String,
		RevokedBy: r.RevokedBy.String, ParentTokenID: r.ParentTokenID.String}
	if !t.NeverExpires {
		v := time.UnixMicro(r.ExpiresAt).UTC()
		t.ExpiresAt = &v
	}
	if r.LastUsedAt.Valid {
		v := time.UnixMicro(r.LastUsedAt.Int64).UTC()
		t.LastUsedAt = &v
	}
	if r.RevokedAt.Valid {
		v := time.UnixMicro(r.RevokedAt.Int64).UTC()
		t.RevokedAt = &v
	}
	return t
}

// CreateAPIToken issues a token for the calling user scoped to grants, each of which must be covered by the caller's
// own grants. The token is returned once. A token either expires after ttl or, with neverExpires (and ttl zero), never expires
// (ADR-0053). An administrator token may issue tokens, which it then parents; a token issued by a token may not
// (ADR-0058).
func (svc *Service) CreateAPIToken(ctx context.Context, p *Principal, name string, scope []GrantInput, ttl time.Duration, neverExpires bool) (*APIToken, string, error) {
	name = strings.TrimSpace(name)
	v := &apperr.Validation{}
	if name == "" || utf8.RuneCountInString(name) > 100 {
		v.Add("name", "must be 1-100 characters")
	}
	if p.User == nil {
		return nil, "", apperr.New(apperr.CodeForbidden, "only users can issue API tokens; create a user first")
	}
	grants, gerr := validGrants(scope)
	if gerr != nil {
		return nil, "", gerr
	}
	if len(grants) == 0 {
		v.Add("grants", "at least one grant is required")
	}
	for i, g := range grants {
		if !authz.Covers(p.User.Grants, authz.Grant{Role: g.Role, Domain: g.Domain}) {
			v.Add("grants", "entry %d (%s in %s) exceeds your own grants", i, g.Role, g.Domain)
		}
	}
	switch {
	case neverExpires && ttl != 0:
		v.Add("expires_in_days", "must be omitted for a token that never expires")
	case !neverExpires && (ttl < MinAPITokenTTL || ttl > MaxAPITokenTTL):
		v.Add("expires_in_days", "must be between 1 and 366 days")
	}
	if p.TokenParentID != "" {
		return nil, "", apperr.New(apperr.CodeForbidden, "a token issued by another token cannot issue tokens")
	}
	if err := v.Err(); err != nil {
		return nil, "", err
	}
	secret, err := randomToken()
	if err != nil {
		return nil, "", err
	}
	token := APITokenPrefix + secret
	mac, err := svc.tokenMAC(token)
	if err != nil {
		return nil, "", err
	}
	id := ids.New(ids.APIToken)
	err = svc.s.W().TxNamed(ctx, "auth.create_api_token", func(tx *store.Tx) error {
		expires, details := tx.Now.Add(ttl), map[string]any{"name": name, "grants": grants, "expires_in": ttl.String()}
		if neverExpires {
			expires, details = neverExpiresAt, map[string]any{"name": name, "grants": grants, "never_expires": true}
		}
		actor := audit.Actor{Type: audit.ActorUser, ID: p.User.ID}
		if p.TokenID != "" {
			actor.Via, details["parent_token_id"] = "api_token:"+p.TokenID, p.TokenID
		}
		if err := tx.Q.InsertAPIToken(ctx, db.InsertAPITokenParams{ID: id, Name: name, UserID: p.User.ID, Mac: mac,
			Hint: token[len(token)-4:], CreatedAt: tx.NowMicros(), ExpiresAt: expires.UnixMicro(), NeverExpires: b2i(neverExpires),
			ParentTokenID: sql.NullString{String: p.TokenID, Valid: p.TokenID != ""}}); err != nil {
			return err
		}
		if err := svc.insertGrants(ctx, tx, TokenSubject(id), grants, "token", actor); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "api_token.created",
			Target: audit.Target{Type: "api_token", ID: id}, Details: details})
		return err
	})
	if err != nil {
		return nil, "", err
	}
	t, err := svc.GetAPIToken(ctx, id)
	return t, token, err
}

// GetAPIToken returns one token's metadata.
func (svc *Service) GetAPIToken(ctx context.Context, id string) (*APIToken, error) {
	r, err := svc.s.R().GetAPIToken(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.CodeNotFound, "API token not found")
	}
	if err != nil {
		return nil, err
	}
	return svc.withGrants(ctx, apiTokenFromRow(r))
}

func (svc *Service) withGrants(ctx context.Context, t *APIToken) (*APIToken, error) {
	rows, err := svc.s.R().ListRoleGrantsBySubject(ctx, TokenSubject(t.ID))
	if err != nil {
		return nil, err
	}
	t.Grants = grantsFromRows(rows)
	return t, nil
}

// ListAPITokens returns every token, active first.
func (svc *Service) ListAPITokens(ctx context.Context) ([]*APIToken, error) {
	rows, err := svc.s.R().ListAPITokens(ctx)
	if err != nil {
		return nil, err
	}
	return svc.tokensWithGrants(ctx, rows)
}

// ListUserAPITokens returns one user's tokens, active first.
func (svc *Service) ListUserAPITokens(ctx context.Context, userID string) ([]*APIToken, error) {
	rows, err := svc.s.R().ListAPITokensByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return svc.tokensWithGrants(ctx, rows)
}

func (svc *Service) tokensWithGrants(ctx context.Context, rows []db.ApiToken) ([]*APIToken, error) {
	out := make([]*APIToken, len(rows))
	for i, r := range rows {
		t, err := svc.withGrants(ctx, apiTokenFromRow(r))
		if err != nil {
			return nil, err
		}
		out[i] = t
	}
	return out, nil
}

// RevokeAPIToken stops a token at once, together with the tokens it issued (ADR-0058). Revoking a revoked token is a
// no-op.
func (svc *Service) RevokeAPIToken(ctx context.Context, actor audit.Actor, id string) (*APIToken, error) {
	err := svc.s.W().TxNamed(ctx, "auth.revoke_api_token", func(tx *store.Tx) error {
		if _, err := tx.Q.GetAPIToken(ctx, id); errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "API token not found")
		} else if err != nil {
			return err
		}
		children, err := tx.Q.ListActiveChildAPITokens(ctx, sql.NullString{String: id, Valid: true})
		if err != nil {
			return err
		}
		for _, tid := range append([]string{id}, children...) {
			n, err := tx.Q.RevokeAPIToken(ctx, db.RevokeAPITokenParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
				Actor: sql.NullString{String: actorRef(actor), Valid: true}, ID: tid})
			if err != nil {
				return err
			}
			if n == 0 {
				continue
			}
			var details map[string]any
			if tid != id {
				details = map[string]any{"parent_token_id": id}
			}
			if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "api_token.revoked", Target: audit.Target{Type: "api_token", ID: tid},
				Details: details}); err != nil {
				return err
			}
			if err := tx.Q.DeleteRoleGrantsBySubject(ctx, TokenSubject(tid)); err != nil {
				return err
			}
			revoked := tid
			svc.revokedAfterCommit(tx, revocation{tokenID: revoked})
			tx.AfterCommit(func() {
				if svc.bus != nil {
					svc.bus.Publish(eventbus.Event{Topic: eventbus.TopicTokenRevoked, Key: revoked})
				}
			})
		}
		svc.reloadAfterCommit(ctx, tx)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return svc.GetAPIToken(ctx, id)
}

// AuthenticateAPIToken resolves a bearer token to a principal acting for the creating user, authorized as both the token
// and the user. Unknown, revoked and expired tokens, and tokens whose user is disabled, all fail with the same error.
func (svc *Service) AuthenticateAPIToken(ctx context.Context, token, ip string) (*Principal, error) {
	if !strings.HasPrefix(token, APITokenPrefix) || len(token) != len(APITokenPrefix)+43 {
		return nil, ErrUnauthorized
	}
	gen := svc.rev.gen.Load()
	macs, err := svc.tokenMACs(token)
	if err != nil {
		return nil, err
	}
	var row db.ApiToken
	found := false
	for _, mac := range macs {
		r, err := svc.s.R().GetAPITokenByMAC(ctx, mac)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		row, found = r, true
		break
	}
	now := svc.opts.Clock.Now()
	if !found || row.RevokedAt.Valid || !now.Before(time.UnixMicro(row.ExpiresAt)) {
		return nil, ErrUnauthorized
	}
	op, err := svc.Get(ctx, row.UserID)
	if err != nil || !op.Enabled {
		return nil, ErrUnauthorized
	}
	if !row.LastUsedAt.Valid || now.Sub(time.UnixMicro(row.LastUsedAt.Int64)) >= apiTokenTouchEvery {
		_ = svc.s.W().TxNamed(ctx, "auth.touch_api_token", func(tx *store.Tx) error {
			return tx.Q.TouchAPIToken(ctx, db.TouchAPITokenParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
				Ip: sql.NullString{String: ip, Valid: ip != ""}, ID: row.ID, Since: sql.NullInt64{Int64: tx.Now.Add(-apiTokenTouchEvery).UnixMicro(), Valid: true}})
		})
	}
	return &Principal{User: op, SessionID: row.ID, TokenID: row.ID, TokenParentID: row.ParentTokenID.String, Method: MethodToken,
		AuthAt: now, ExpiresAt: time.UnixMicro(row.ExpiresAt), revGen: gen}, nil
}
