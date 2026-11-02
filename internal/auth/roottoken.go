package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// The bootstrap root token (SM-AUTH-005): `spaceman_rt_<43 base64url chars>`. It has full scope ("admin" in "*"), never
// expires, is displayed exactly once, and only a keyed MAC (keyring token key) is stored. It exists to create the first
// administrator and for emergency administration; it is never given to agents or nodes (agents and nodes have their own
// credential types, which the API rejects as bearer tokens and vice versa). Root authority does not bypass execution
// safety: services enforce invariants (pinning, leases, single control run) regardless of the caller (SM-INV-016).
const RootTokenPrefix = "spaceman_rt_"

// RootToken describes the root token (never the token itself).
type RootToken struct {
	ID         string     `json:"id"`
	Hint       string     `json:"hint"`
	CreatedAt  time.Time  `json:"created_at"`
	CreatedBy  string     `json:"created_by"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	LastUsedIP string     `json:"last_used_ip,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// ErrRootTokenExists is returned by IssueRootToken once a root token was ever issued: bootstrap happens once; use
// rotation (online, or `spaceman admin root-token rotate` offline) afterwards.
var ErrRootTokenExists = apperr.New(apperr.CodePrecondition,
	"a root token was already issued; rotate it with `spaceman admin root-token rotate` or recover access with `spaceman admin recover`")

// IssueRootToken creates the first root token (offline CLI bootstrap: requires the data-directory lock and the KEK).
func (svc *Service) IssueRootToken(ctx context.Context) (string, *RootToken, error) {
	actor := audit.Actor{Type: audit.ActorCLI, ID: "bootstrap"}
	var token, id string
	err := svc.s.W().TxNamed(ctx, "auth.root_token.issue", func(tx *store.Tx) error {
		n, err := tx.Q.CountRootTokens(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return ErrRootTokenExists
		}
		token, id, err = svc.insertRootToken(ctx, tx, actor)
		if err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "root_token.issued", Target: audit.Target{Type: "root_token", ID: id}})
		return err
	})
	if err != nil {
		return "", nil, err
	}
	rt, err := svc.getRootToken(ctx, id)
	return token, rt, err
}

// RotateRootToken revokes the active root token (if any) and issues a new one, returned once. Online it requires the
// caller to be a global administrator or the root token itself (enforced by the API layer); offline it is the CLI.
func (svc *Service) RotateRootToken(ctx context.Context, actor audit.Actor) (string, *RootToken, error) {
	var token, id string
	err := svc.s.W().TxNamed(ctx, "auth.root_token.rotate", func(tx *store.Tx) error {
		old, err := tx.Q.GetActiveRootToken(ctx)
		hadOld := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := svc.revokeRootTokens(ctx, tx, actor); err != nil {
			return err
		}
		if token, id, err = svc.insertRootToken(ctx, tx, actor); err != nil {
			return err
		}
		details := map[string]any{}
		if hadOld {
			details["previous_id"] = old.ID
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "root_token.rotated", Target: audit.Target{Type: "root_token", ID: id},
			Details: details})
		return err
	})
	if err != nil {
		return "", nil, err
	}
	rt, err := svc.getRootToken(ctx, id)
	return token, rt, err
}

// RevokeRootToken revokes the active root token. It is refused while no enabled global administrator user exists,
// so revocation can never lock every human out (offline `spaceman admin recover` remains the last resort).
func (svc *Service) RevokeRootToken(ctx context.Context, actor audit.Actor) error {
	return svc.s.W().TxNamed(ctx, "auth.root_token.revoke", func(tx *store.Tx) error {
		n, err := tx.Q.CountEnabledGlobalAdmins(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodePrecondition, "create an enabled global administrator before revoking the root token")
		}
		old, err := tx.Q.GetActiveRootToken(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "no active root token")
		} else if err != nil {
			return err
		}
		if err := svc.revokeRootTokens(ctx, tx, actor); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "root_token.revoked", Target: audit.Target{Type: "root_token", ID: old.ID}})
		return err
	})
}

// GetRootToken returns the active root token's metadata.
func (svc *Service) GetRootToken(ctx context.Context) (*RootToken, error) {
	r, err := svc.s.R().GetActiveRootToken(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.CodeNotFound, "no active root token")
	}
	if err != nil {
		return nil, err
	}
	return rootTokenFromRow(r), nil
}

// AuthenticateRootToken resolves the root bearer token. Unknown and revoked tokens fail with the generic error.
func (svc *Service) AuthenticateRootToken(ctx context.Context, token, ip string) (*Principal, error) {
	if !strings.HasPrefix(token, RootTokenPrefix) || len(token) != len(RootTokenPrefix)+43 {
		return nil, ErrUnauthorized
	}
	macs, err := svc.tokenMACs(token)
	if err != nil {
		return nil, err
	}
	for _, mac := range macs {
		r, err := svc.s.R().GetRootTokenByMAC(ctx, mac)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if r.RevokedAt.Valid {
			return nil, ErrUnauthorized
		}
		now := svc.opts.Clock.Now()
		if !r.LastUsedAt.Valid || now.Sub(time.UnixMicro(r.LastUsedAt.Int64)) >= apiTokenTouchEvery {
			_ = svc.s.W().TxNamed(ctx, "auth.touch_root_token", func(tx *store.Tx) error {
				return tx.Q.TouchRootToken(ctx, db.TouchRootTokenParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
					Ip: sql.NullString{String: ip, Valid: ip != ""}, ID: r.ID, Since: sql.NullInt64{Int64: tx.Now.Add(-apiTokenTouchEvery).UnixMicro(), Valid: true}})
			})
		}
		return &Principal{RootTokenID: r.ID, SessionID: r.ID, Method: MethodRoot, AuthAt: now, ExpiresAt: neverExpiresAt}, nil
	}
	return nil, ErrUnauthorized
}

func (svc *Service) insertRootToken(ctx context.Context, tx *store.Tx, actor audit.Actor) (token, id string, err error) {
	secret, err := randomToken()
	if err != nil {
		return "", "", err
	}
	token = RootTokenPrefix + secret
	mac, err := svc.tokenMAC(token)
	if err != nil {
		return "", "", err
	}
	id = ids.New(ids.RootToken)
	if err := tx.Q.InsertRootToken(ctx, db.InsertRootTokenParams{ID: id, Mac: mac, Hint: token[len(token)-4:], CreatedAt: tx.NowMicros(),
		CreatedBy: actorRef(actor)}); err != nil {
		return "", "", err
	}
	if err := svc.insertGrants(ctx, tx, RootSubject(id), []GrantInput{{Role: authz.RoleAdmin, Domain: authz.Global}}, "bootstrap", actor); err != nil {
		return "", "", err
	}
	return token, id, nil
}

func (svc *Service) revokeRootTokens(ctx context.Context, tx *store.Tx, actor audit.Actor) error {
	old, err := tx.Q.GetActiveRootToken(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := tx.Q.RevokeRootTokens(ctx, db.RevokeRootTokensParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
		Actor: sql.NullString{String: actorRef(actor), Valid: true}}); err != nil {
		return err
	}
	if err := tx.Q.DeleteRoleGrantsBySubject(ctx, RootSubject(old.ID)); err != nil {
		return err
	}
	svc.reloadAfterCommit(ctx, tx)
	tx.AfterCommit(func() {
		if svc.bus != nil {
			svc.bus.Publish(eventbus.Event{Topic: eventbus.TopicTokenRevoked, Key: old.ID})
		}
	})
	return nil
}

func (svc *Service) getRootToken(ctx context.Context, id string) (*RootToken, error) {
	r, err := svc.s.R().GetActiveRootToken(ctx)
	if err != nil {
		return nil, err
	}
	if r.ID != id {
		return nil, apperr.New(apperr.CodeConflict, "root token changed concurrently")
	}
	return rootTokenFromRow(r), nil
}

func rootTokenFromRow(r db.RootToken) *RootToken {
	t := &RootToken{ID: r.ID, Hint: r.Hint, CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), CreatedBy: r.CreatedBy, LastUsedIP: r.LastUsedIp.String}
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
