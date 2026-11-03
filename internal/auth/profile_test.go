package auth

import (
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
)

// SM-API-004 (ADR-0050): an administrator changes a user's email, display name and MFA requirement. The change is
// optimistic, audited, keeps emails unique and lower-cased, keeps a password user's email (their sign-in name), ends
// their sessions when it changes, and never lifts MFA that policy forces.
func TestUpdateProfile(t *testing.T) {
	f := newFx(t)
	admin := f.bootstrap()
	actor := audit.Actor{Type: audit.ActorUser, ID: admin.ID}
	u, err := f.svc.Create(f.ctx, actor, CreateInput{Email: "dev@example.com", DisplayName: "Dev", Password: strongPW, Grants: grants(RoleViewer)})
	must(t, err)
	sso, err := f.svc.Create(f.ctx, actor, CreateInput{DisplayName: "SSO only", Grants: grants(RoleViewer)})
	must(t, err)
	res, err := f.svc.Login(f.ctx, LoginInput{Email: "dev@example.com", Password: strongPW})
	must(t, err)

	if _, err := f.svc.UpdateProfile(f.ctx, actor, u.ID, u.Version+1, ProfileInput{Email: "dev@example.com", DisplayName: "Dev"}); apperr.CodeOf(err) != apperr.CodeConflict {
		t.Fatalf("stale version: %v", err)
	}
	if _, err := f.svc.UpdateProfile(f.ctx, actor, u.ID, u.Version, ProfileInput{Email: "admin@example.com", DisplayName: "Dev"}); apperr.CodeOf(err) != apperr.CodeAlreadyExists {
		t.Fatalf("another user's email: %v", err)
	}
	if _, err := f.svc.UpdateProfile(f.ctx, actor, u.ID, u.Version, ProfileInput{DisplayName: "Dev"}); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("a password user's email removed: %v", err)
	}
	for _, bad := range []ProfileInput{{Email: "not an email", DisplayName: "Dev"}, {Email: "dev@example.com", DisplayName: "  "}} {
		if _, err := f.svc.UpdateProfile(f.ctx, actor, u.ID, u.Version, bad); apperr.CodeOf(err) != apperr.CodeInvalid {
			t.Fatalf("invalid %+v: %v", bad, err)
		}
	}
	got, err := f.svc.UpdateProfile(f.ctx, actor, u.ID, u.Version, ProfileInput{Email: " New@Example.com ", DisplayName: " Developer ", MFARequired: true})
	must(t, err)
	if got.Email != "new@example.com" || got.DisplayName != "Developer" || !got.MFARequiredSet || !got.MFARequired || got.Version != u.Version+1 {
		t.Fatalf("updated %+v", got)
	}
	if _, err := f.svc.Authenticate(f.ctx, res.Token); err == nil {
		t.Fatal("a session survived the email change")
	}
	// the new address signs in; MFA is now required, so the session must enroll first
	res, err = f.svc.Login(f.ctx, LoginInput{Email: "new@example.com", Password: strongPW})
	if err != nil || !res.MustEnroll {
		t.Fatalf("login after the change: %+v %v", res, err)
	}
	// clearing the setting does not lift policy-forced MFA: an operator grant on a password account forces it
	_, err = f.svc.SetGrants(f.ctx, actor, u.ID, []GrantInput{{Role: authz.RoleOperator, Domain: authz.Global}})
	must(t, err)
	cur, err := f.svc.Get(f.ctx, u.ID)
	must(t, err)
	got, err = f.svc.UpdateProfile(f.ctx, actor, u.ID, cur.Version, ProfileInput{Email: "new@example.com", DisplayName: "Developer"})
	must(t, err)
	if got.MFARequiredSet || !got.MFARequired {
		t.Fatalf("policy-forced MFA lifted: %+v", got)
	}
	// a user without a password may have no email, and setting MFA on it is only a flag
	got, err = f.svc.UpdateProfile(f.ctx, actor, sso.ID, sso.Version, ProfileInput{DisplayName: "SSO user", MFARequired: true})
	must(t, err)
	if got.Email != "" || got.DisplayName != "SSO user" || !got.MFARequiredSet {
		t.Fatalf("sso user %+v", got)
	}
	var n int
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE action = 'user.profile_updated'`).Scan(&n))
	if n != 3 {
		t.Fatalf("%d profile audit records, want 3", n)
	}
	if _, err := f.svc.UpdateProfile(f.ctx, actor, "usr_missing", 1, ProfileInput{DisplayName: "x"}); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("missing user: %v", err)
	}
	// the last-administrator guard still holds for the administrator's own profile
	a, err := f.svc.Get(f.ctx, admin.ID)
	must(t, err)
	if _, err := f.svc.UpdateProfile(f.ctx, actor, a.ID, a.Version, ProfileInput{Email: "admin@example.com", DisplayName: "Admin", MFARequired: true}); err != nil {
		t.Fatalf("admin profile: %v", err)
	}
}
