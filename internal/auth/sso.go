package auth

import (
	"context"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// SSOSubject is the authorization subject of a user's SSO-sourced grants ("user:<id>#sso"). Only SSO sessions act
// as it (Principal.Subjects); it also holds every grant of the user's own subject (authz.SSOSuffix).
func SSOSubject(id string) string { return UserSubject(id) + authz.SSOSuffix }

// SSOGrant is one grant derived from an identity provider group mapping. GroupValue is the claim value that produced
// it (kept as the grant's source reference).
type SSOGrant struct {
	Role       authz.Role
	Domain     string
	GroupValue string
}

// SyncSSOGrantsTx replaces the user's SSO-sourced grants with grants inside the caller's transaction and reports
// whether they changed. Manual grants are untouched (Hallpass HP-OIDC-011). The authorization engine reloads after
// commit. SSO grants never count toward the last-administrator guard, so a manual global administrator always
// remains and an identity provider change cannot lock SpaceMan out.
func (svc *Service) SyncSSOGrantsTx(ctx context.Context, tx *store.Tx, userID string, grants []SSOGrant, actor audit.Actor) (bool, error) {
	subject := SSOSubject(userID)
	before, err := tx.Q.ListRoleGrantsBySubject(ctx, subject)
	if err != nil {
		return false, err
	}
	if err := tx.Q.DeleteRoleGrantsBySubject(ctx, subject); err != nil {
		return false, err
	}
	for _, g := range grants {
		if !g.Role.Valid() || !authz.ValidDomain(g.Domain) {
			continue // mappings are validated when they are added; never widen on bad data
		}
		if err := tx.Q.InsertRoleGrant(ctx, db.InsertRoleGrantParams{Subject: subject, Role: string(g.Role), Domain: g.Domain, Source: "sso",
			SourceRef: g.GroupValue, CreatedAt: tx.NowMicros(), CreatedBy: actorRef(actor)}); err != nil {
			return false, err
		}
	}
	after, err := tx.Q.ListRoleGrantsBySubject(ctx, subject)
	if err != nil {
		return false, err
	}
	changed := !sameGrants(grantsFromRows(before), grantsFromRows(after))
	if changed {
		svc.reloadAfterCommit(ctx, tx)
		svc.publish(tx, userID)
	}
	return changed, nil
}

// DropSSOGrantsTx removes the user's SSO-sourced grants (provider deleted, identity unlinked). They come back from the
// mappings at the user's next SSO sign-in.
func (svc *Service) DropSSOGrantsTx(ctx context.Context, tx *store.Tx, userID string) error {
	_, err := svc.SyncSSOGrantsTx(ctx, tx, userID, nil, audit.System)
	return err
}

// PublishUserChangedTx announces a user change after tx commits (JIT provisioning, identity links).
func (svc *Service) PublishUserChangedTx(tx *store.Tx, userID string) { svc.publish(tx, userID) }
