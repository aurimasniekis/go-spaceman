package harborpilot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Bound ref keys of a run's child token (ADR-0060 decision 5). Bindings shared by many spaces must not pin
// spaceman.space: Harbor Pilot refuses a bound value that differs from a pinned one.
const (
	RefSpace    = "spaceman.space"
	RefRun      = "spaceman.run"
	RefInstance = "spaceman.instance"
	RefTask     = "spaceman.task"
	RefSession  = "session"
	RefProject  = "project_id"
)

// BudgetChecker is the budget part of PreLaunch (HP-T5): it refuses a launch whose space's covering blocking budget is
// used up, and applies the space's on_budget_status_unavailable when budget status can't be read.
type BudgetChecker interface {
	CheckBudget(ctx context.Context, spaceID string, s *SpaceSettings) error
}

// LifecycleOptions tune the run-token lifecycle.
type LifecycleOptions struct {
	// Budget is the budget part of PreLaunch (nil: no budget check before the launch; Harbor Pilot still enforces).
	Budget BudgetChecker
	// MintRetry are the waits before each retry of a mint that failed in a way worth retrying (network, 5xx, 429);
	// nil: 1 s, 4 s and 10 s.
	MintRetry []time.Duration
	// Metrics receives the failed-revocation gauge (nil: none).
	Metrics *metrics.Metrics
}

// Lifecycle is the central side of a gateway run's child token (ADR-0060 decisions 4 and 5, SM-GW-003/005): it routes
// launches, mints or reuses the token at dispatch, and revokes queued tokens on the deadline timer. It implements
// fleet.Gateway.
type Lifecycle struct {
	svc *Service
	o   LifecycleOptions

	mu       sync.Mutex
	inflight map[string]*runLock // per-run single flight of Token
}

type runLock struct {
	mu   sync.Mutex
	refs int
}

var _ fleet.Gateway = (*Lifecycle)(nil)

// NewLifecycle returns the run-token lifecycle over the connection and space settings of svc.
func NewLifecycle(svc *Service, o LifecycleOptions) *Lifecycle {
	if o.MintRetry == nil {
		o.MintRetry = []time.Duration{time.Second, 4 * time.Second, 10 * time.Second}
	}
	return &Lifecycle{svc: svc, o: o, inflight: map[string]*runLock{}}
}

// routed returns a space's settings when it routes the harness through Harbor Pilot (nil otherwise).
func routed(row db.SpaceHarborPilot, harness string) *SpaceSettings {
	if row.Enabled != 1 {
		return nil
	}
	var hs, aliases []string
	_ = json.Unmarshal([]byte(row.HarnessesJson), &hs)
	if !slices.Contains(hs, harness) {
		return nil
	}
	_ = json.Unmarshal([]byte(row.AllowedModelAliasesJson), &aliases)
	return &SpaceSettings{SpaceID: row.SpaceID, Stored: true, Enabled: true, ProjectID: row.ProjectID, Harnesses: hs, TTLSeconds: row.TtlSeconds,
		OnBudgetStatusUnavailable: row.OnBudgetStatusUnavailable, AllowedModelAliases: aliases, Version: row.Version}
}

func errNoConnectionFor(harness string) error {
	return apperr.New(apperr.CodePrecondition, "the space routes %s runs through Harbor Pilot, but no Harbor Pilot connection is configured: "+
		"an administrator connects Harbor Pilot first (runs never fall back to a direct account)", harness)
}

// PreLaunch refuses a launch of a routed harness before the launch transaction: when no connection is configured, or
// (HP-T5, the Budget checker) when a blocking budget covering the space is used up. The dispatch mint then asks Harbor
// Pilot to check its budgets again (check_budget), with its own figures, before the harness starts.
func (l *Lifecycle) PreLaunch(ctx context.Context, spaceID, harness string) error {
	row, err := l.svc.s.R().GetSpaceHarborPilot(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	set := routed(row, harness)
	if set == nil {
		return nil
	}
	if _, err := l.svc.s.R().GetHarborPilotConnection(ctx); errors.Is(err, sql.ErrNoRows) {
		return errNoConnectionFor(harness)
	} else if err != nil {
		return err
	}
	if l.o.Budget != nil {
		return l.o.Budget.CheckBudget(ctx, spaceID, set)
	}
	return nil
}

// Route decides inside the launch transaction whether the run goes through Harbor Pilot. A launch of a routed harness
// that names an account is refused (never a fallback, Q1), as is a model outside the space's allowed aliases.
func (l *Lifecycle) Route(ctx context.Context, tx *store.Tx, spaceID, harness, accountRef, model string) (*fleet.GatewaySpec, error) {
	row, err := tx.Q.GetSpaceHarborPilot(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	set := routed(row, harness)
	if set == nil {
		return nil, nil
	}
	if accountRef != "" {
		return nil, apperr.New(apperr.CodePrecondition, "the space routes %s runs through Harbor Pilot: a launch can't name an account (%s); "+
			"leave the account out, or disable Harbor Pilot for %s in the space", harness, accountRef, harness)
	}
	if len(set.AllowedModelAliases) > 0 && !slices.Contains(set.AllowedModelAliases, model) {
		if model == "" {
			return nil, apperr.New(apperr.CodePrecondition, "the space allows only these Harbor Pilot models: %s; the launch names none (set the role's model)",
				strings.Join(set.AllowedModelAliases, ", "))
		}
		return nil, apperr.New(apperr.CodePrecondition, "model %q is not allowed in this space through Harbor Pilot; allowed: %s", model,
			strings.Join(set.AllowedModelAliases, ", "))
	}
	if _, err := tx.Q.GetHarborPilotConnection(ctx); errors.Is(err, sql.ErrNoRows) {
		return nil, errNoConnectionFor(harness)
	} else if err != nil {
		return nil, err
	}
	return &fleet.GatewaySpec{Kind: fleet.GatewayKindHarborPilot, ProjectID: set.ProjectID}, nil
}

func (l *Lifecycle) lock(runID string) func() {
	l.mu.Lock()
	rl := l.inflight[runID]
	if rl == nil {
		rl = &runLock{}
		l.inflight[runID] = rl
	}
	rl.refs++
	l.mu.Unlock()
	rl.mu.Lock()
	return func() {
		rl.mu.Unlock()
		l.mu.Lock()
		if rl.refs--; rl.refs == 0 {
			delete(l.inflight, runID)
		}
		l.mu.Unlock()
	}
}

// reuseMargin: a stored token that expires sooner than this is replaced by a new one at the next dispatch.
const reuseMargin = 5 * time.Minute

func unavailable(format string, args ...any) error {
	return fmt.Errorf("%w: %s", fleet.ErrGatewayUnavailable, fmt.Sprintf(format, args...))
}

// Token returns the secrets carrying a gateway run's child token (proto.SecretGatewayToken). A re-dispatch reuses the
// stored token while it has more than reuseMargin left, so one run mints once; otherwise it mints (outside any
// transaction), seals the token with RunAAD and stores it. A failure the run can't recover from wraps
// fleet.ErrGatewayUnavailable with a readable reason: the run fails, and never falls back to a direct account.
func (l *Lifecycle) Token(ctx context.Context, run *fleet.Run, session string) (map[string]string, error) {
	if run.Launch.Gateway == nil || run.Launch.Gateway.Kind != fleet.GatewayKindHarborPilot {
		return nil, unavailable("the run names no Harbor Pilot gateway")
	}
	unlock := l.lock(run.ID)
	defer unlock()

	q := l.svc.s.R()
	old := ""
	cur, err := q.GetRunGatewayCredential(ctx, run.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	case cur.TokenEnc == nil:
		return nil, unavailable("the run's Harbor Pilot token was already revoked")
	case time.UnixMicro(cur.ExpiresAt).After(l.svc.s.Now().Add(reuseMargin)):
		tok, err := l.svc.open(RunAAD(run.ID), cur.TokenEnc, "run")
		if err != nil {
			return nil, err
		}
		return map[string]string{proto.SecretGatewayToken: tok}, nil
	default:
		old = cur.CredentialID // about to expire: replaced below
	}

	// A parent rotation that commits while the mint is in flight can't see the new credential: it is revoked here with
	// the parent that minted it, and the mint is done again with the new parent.
	for attempt := 1; ; attempt++ {
		sec, err := l.mintAndStore(ctx, run, session, old)
		if !errors.Is(err, errParentReplaced) {
			return sec, err
		}
		if attempt >= 2 {
			return nil, unavailable("the Harbor Pilot parent token was replaced while the run's token was minted; start the run again")
		}
	}
}

// Address implements fleet.Gateway: the connection's run-facing URL (run_base_url, or base_url), checked against the
// run-facing rule again (https, no loopback exemption, unless insecure HTTP is allowed; ADR-0060 decision 3).
func (l *Lifecycle) Address(ctx context.Context) (*proto.GatewayLaunch, error) {
	conn, err := l.svc.s.R().GetHarborPilotConnection(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, unavailable("no Harbor Pilot connection is configured")
	} else if err != nil {
		return nil, err
	}
	raw := conn.RunBaseUrl
	if raw == "" {
		raw = conn.BaseUrl
	}
	u, err := ParseBaseURL(raw, conn.AllowInsecureHttp == 1, false)
	if err != nil {
		return nil, unavailable("the address runs reach Harbor Pilot on is not usable: %s", err.Error())
	}
	return &proto.GatewayLaunch{Kind: proto.GatewayHarborPilot, BaseURL: u.String(), AllowInsecureHTTP: conn.AllowInsecureHttp == 1}, nil
}

// errParentReplaced: the connection's parent token was replaced (or the connection removed) while a run token was
// minted with the old one; the credential was revoked or queued.
var errParentReplaced = errors.New("harborpilot: the parent token was replaced during the mint")

// mintAndStore mints a run token with the connection's current parent and stores it (replacing old, when set).
func (l *Lifecycle) mintAndStore(ctx context.Context, run *fleet.Run, session, old string) (map[string]string, error) {
	q := l.svc.s.R()
	conn, err := q.GetHarborPilotConnection(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, unavailable("no Harbor Pilot connection is configured")
	} else if err != nil {
		return nil, err
	}
	parent, err := l.svc.open(parentAAD(conn.ID), conn.ParentTokenEnc, "parent")
	if err != nil {
		return nil, err
	}
	c, err := l.svc.client(conn.BaseUrl, conn.AllowInsecureHttp == 1)
	if err != nil {
		return nil, unavailable("the Harbor Pilot connection's address is not usable: %s", err.Error())
	}
	ttl := int64(DefaultTTLSeconds)
	if row, err := q.GetSpaceHarborPilot(ctx, run.SpaceID); err == nil {
		ttl = row.TtlSeconds
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	refs := map[string]string{RefSpace: run.SpaceID, RefRun: run.ID, RefInstance: run.InstanceID, RefSession: session, RefProject: run.Launch.Gateway.ProjectID}
	if task, err := q.GetTaskIDForRun(ctx, run.ID); err == nil {
		refs[RefTask] = task
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	cred, tok, err := l.mint(ctx, c, parent, MintRequest{Name: "spaceman-run-" + run.ID, Scopes: RunScopes, TTLSeconds: ttl, BoundRefs: refs,
		CheckBudget: true})
	if err != nil {
		var he *Error
		if errors.As(err, &he) {
			return nil, unavailable("Harbor Pilot refused to mint the run's token: %s", he.Reason())
		}
		return nil, err
	}
	fp := ParentFingerprint(parent)
	sealed, err := l.svc.kr.Seal(keyring.PurposeCredential, RunAAD(run.ID), []byte(tok))
	if err != nil {
		l.queueOrphan(ctx, cred, run, fleet.RevokeLaunchFail, fp)
		return nil, err
	}
	exp := cred.ExpiresAt
	if exp.IsZero() {
		exp = l.svc.s.Now().Add(time.Duration(ttl) * time.Second)
	}
	var stopped, replaced bool
	err = l.svc.s.W().TxNamed(ctx, "harborpilot.store_run_token", func(tx *store.Tx) error {
		stopped, replaced = false, false
		queue := func(id, reason string, expires int64, fp string) error {
			tx.AfterCommit(l.wake)
			return tx.Q.InsertGatewayRevocation(ctx, db.InsertGatewayRevocationParams{CredentialID: id, RunID: run.ID, SpaceID: run.SpaceID, Reason: reason,
				ExpiresAt: expires, NextAttemptAt: tx.NowMicros(), RequestedAt: tx.NowMicros(), ParentFp: fpArg(fp)})
		}
		// The parent was replaced after the mint: the rotation committed without seeing this credential, so storing it
		// would leave a token only the old parent can revoke. It is revoked after this transaction. (Re-entering the same
		// parent also bumps the version: that only costs a second mint.)
		if now, err := tx.Q.GetHarborPilotConnection(ctx); errors.Is(err, sql.ErrNoRows) || (err == nil && (now.ID != conn.ID || now.ParentTokenVersion != conn.ParentTokenVersion)) {
			replaced = true
			return nil
		} else if err != nil {
			return err
		}
		r, err := tx.Q.GetRun(ctx, run.ID)
		if err != nil {
			return err
		}
		// The run was stopped or ended while the mint was in flight: its end path found nothing to revoke, so this token
		// is revoked here.
		if r.Desired != "running" || r.Observed == proto.RunExited || r.Observed == proto.RunFailed || r.Observed == proto.RunLost {
			stopped = true
			return queue(cred.ID, fleet.RevokeRunStopped, exp.UnixMicro(), fp)
		}
		row, err := tx.Q.GetRunGatewayCredential(ctx, run.ID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return tx.Q.InsertRunGatewayCredential(ctx, db.InsertRunGatewayCredentialParams{RunID: run.ID, SpaceID: run.SpaceID, CredentialID: cred.ID,
				TokenEnc: sealed, ExpiresAt: exp.UnixMicro(), CreatedAt: tx.NowMicros(), ParentFp: fpArg(fp)})
		case err != nil:
			return err
		case row.TokenEnc == nil:
			stopped = true
			return queue(cred.ID, fleet.RevokeRunStopped, exp.UnixMicro(), fp)
		case row.CredentialID != old:
			// Another dispatch stored a token first: keep that one, revoke this one.
			if err := queue(cred.ID, fleet.RevokeDuplicate, exp.UnixMicro(), fp); err != nil {
				return err
			}
			b, err := l.svc.kr.Open(keyring.PurposeCredential, RunAAD(run.ID), row.TokenEnc)
			tok = string(b)
			return err
		}
		oldFP := ""
		if row.ParentFp.Valid {
			oldFP = row.ParentFp.String
		}
		if err := queue(old, fleet.RevokeReplaced, row.ExpiresAt, oldFP); err != nil {
			return err
		}
		_, err = tx.Q.ReplaceRunGatewayCredential(ctx, db.ReplaceRunGatewayCredentialParams{CredentialID: cred.ID, TokenEnc: sealed, ExpiresAt: exp.UnixMicro(),
			CreatedAt: tx.NowMicros(), ParentFp: fpArg(fp), RunID: run.ID, OldCredentialID: old})
		return err
	})
	if err != nil {
		l.queueOrphan(ctx, cred, run, fleet.RevokeLaunchFail, fp)
		return nil, err
	}
	if replaced {
		cctx, cancel := context.WithTimeout(ctx, revokeTimeout)
		rerr := c.Revoke(cctx, parent, cred.ID)
		cancel()
		if st, _ := revokeOutcome(rerr, true); st != "revoked" {
			// Queued with the old parent's fingerprint: the revoker, holding the new parent, records it as failed and audits it.
			l.queueOrphan(ctx, cred, run, fleet.RevokeParentReset, fp)
		}
		return nil, errParentReplaced
	}
	if stopped {
		// Not a gateway failure: the launch is cancelled with the stop.
		return nil, errors.New("harborpilot: the run was stopped while its token was minted")
	}
	return map[string]string{proto.SecretGatewayToken: tok}, nil
}

// mint mints a child, retrying failures worth retrying (network, 5xx, 429 other than a budget refusal).
func (l *Lifecycle) mint(ctx context.Context, c *Client, parent string, in MintRequest) (*Credential, string, error) {
	for attempt := 0; ; attempt++ {
		cred, tok, err := c.Mint(ctx, parent, in)
		var he *Error
		if err == nil || !errors.As(err, &he) || !he.Retryable() || attempt >= len(l.o.MintRetry) {
			return cred, tok, err
		}
		select {
		case <-ctx.Done():
			return nil, "", err
		case <-time.After(l.o.MintRetry[attempt]):
		}
	}
}

// queueOrphan queues the revocation of a minted credential that could not be stored with its run.
func (l *Lifecycle) queueOrphan(ctx context.Context, cred *Credential, run *fleet.Run, reason, parentFP string) {
	exp := cred.ExpiresAt
	if exp.IsZero() {
		exp = l.svc.s.Now().Add(MaxTTLSeconds * time.Second)
	}
	ctx = context.WithoutCancel(ctx) // the dispatch's bound may have run out: the credential must still be queued
	err := l.svc.s.W().TxNamed(ctx, "harborpilot.queue_orphan", func(tx *store.Tx) error {
		return tx.Q.InsertGatewayRevocation(ctx, db.InsertGatewayRevocationParams{CredentialID: cred.ID, RunID: run.ID, SpaceID: run.SpaceID, Reason: reason,
			ExpiresAt: exp.UnixMicro(), NextAttemptAt: tx.NowMicros(), RequestedAt: tx.NowMicros(), ParentFp: fpArg(parentFP)})
	})
	if err != nil {
		l.svc.log().Error("harbor pilot run credential neither stored nor queued for revocation", "credential_id", cred.ID, "run_id", run.ID, "err", err)
		return
	}
	l.wake()
}

func (l *Lifecycle) wake() { l.svc.wake() }

// Revoker bounds: a round revokes at most revokeBatch credentials, each call bounded by revokeTimeout, so the shared
// deadline timer is never held long; the rest are due at once and go in the next round.
const (
	revokeBatch   = 10
	revokeTimeout = 5 * time.Second
	revokeBackoff = time.Second
	revokeMaxWait = 5 * time.Minute
)

// NextRevocation is when the earliest queued revocation is due, or the token of a failed one expires, which takes it
// out of the failed count and the gauge (zero: neither). The deadline timer calls it.
func (l *Lifecycle) NextRevocation(ctx context.Context) (time.Time, error) {
	l.refreshFailed(ctx)
	q := l.svc.s.R()
	at, err := q.NextGatewayRevocation(ctx)
	if err != nil {
		return time.Time{}, err
	}
	exp, err := q.NextFailedGatewayRevocationExpiry(ctx, l.svc.s.Now().UnixMicro())
	if err != nil {
		return time.Time{}, err
	}
	if at == 0 || (exp != 0 && exp < at) {
		at = exp
	}
	if at == 0 {
		return time.Time{}, nil
	}
	return time.UnixMicro(at).UTC(), nil
}

// RevokeDue revokes queued credentials that are due, outside any transaction, with the connection's parent token:
//   - 204: revoked; 404: revoked as far as Harbor Pilot knows (recorded);
//   - 401: the parent itself is revoked or expired, which revoked its children (recorded, audited);
//   - a 404 or 401 for a credential another (or an unknown) parent minted proves nothing about it: failed (recorded,
//     audited, counted in spaceman_gateway_revocations_failed);
//   - 403 or another refusal: failed (recorded, audited, logged): it needs a person;
//   - a network failure, 5xx or 429: retried with backoff (1 s, ×4, at most 5 min) until the credential expires, then
//     recorded as expired.
//
// The queue is durable, so revocations queued before a restart are done after it. It returns how many rows settled.
func (l *Lifecycle) RevokeDue(ctx context.Context) (int, error) {
	now := l.svc.s.Now()
	due, err := l.svc.s.R().ListDueGatewayRevocations(ctx, db.ListDueGatewayRevocationsParams{Now: now.UnixMicro(), Lim: revokeBatch})
	if err != nil || len(due) == 0 {
		return 0, err
	}
	// Every due row is settled or rescheduled, whatever goes wrong here: a row left due would make the shared deadline
	// timer fire again at once.
	var c *Client
	parent, parentFP, unusable := "", "", "no Harbor Pilot connection is configured"
	conn, err := l.svc.s.R().GetHarborPilotConnection(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return 0, err
	default:
		if parent, err = l.svc.open(parentAAD(conn.ID), conn.ParentTokenEnc, "parent"); err != nil {
			l.svc.log().Error("harbor pilot revoker: the parent token can't be opened", "err", err)
			unusable = "the parent token can't be opened"
		} else if c, err = l.svc.client(conn.BaseUrl, conn.AllowInsecureHttp == 1); err != nil {
			c, unusable = nil, "the connection's address is not usable"
		} else {
			parentFP = ParentFingerprint(parent)
		}
	}
	settled := 0
	var errs []error
	for _, r := range due {
		state, msg := "", ""
		switch {
		case !time.UnixMicro(r.ExpiresAt).After(now):
			state, msg = "expired", "expired before it was revoked"
		case c == nil:
			msg = unusable
		default:
			cctx, cancel := context.WithTimeout(ctx, revokeTimeout)
			err := c.Revoke(cctx, parent, r.CredentialID)
			cancel()
			state, msg = revokeOutcome(err, r.ParentFp.Valid && r.ParentFp.String == parentFP)
		}
		if err := l.settle(ctx, r, state, msg, now); err != nil {
			errs = append(errs, err)
			continue
		}
		if state != "" {
			settled++
		}
	}
	if settled > 0 {
		l.refreshFailed(ctx)
	}
	return settled, errors.Join(errs...)
}

// msgOtherParent prefixes a 404 or 401 answered to a parent that did not mint the credential.
const msgOtherParent = "another (or an unknown) parent token minted this credential, so Harbor Pilot's answer to the current one proves nothing " +
	"(revoke the old parent in Harbor Pilot, which ends its children): "

// revokeOutcome classifies the answer to revoking a credential: state "" means retry later. sameParent says whether
// the parent that asked is the one that minted the credential: a 404 or 401 settles as revoked only then.
func revokeOutcome(err error, sameParent bool) (state, msg string) {
	var he *Error
	switch {
	case err == nil:
		return "revoked", ""
	case !errors.As(err, &he):
		return "", "revoke failed"
	case (he.Class == ClassNotFound || he.Class == ClassUnauthorized) && !sameParent:
		return "failed", msgOtherParent + he.Reason()
	case he.Class == ClassNotFound:
		return "revoked", "not found: Harbor Pilot has no such credential of this parent"
	case he.Class == ClassUnauthorized:
		return "revoked", "the parent token is invalid, expired or revoked, which ended its children"
	case he.Retryable() || he.Class == ClassProtocol:
		return "", he.Reason()
	}
	return "failed", he.Reason()
}

// refreshFailed sets the failed-revocation gauge from the queue.
func (l *Lifecycle) refreshFailed(ctx context.Context) {
	if l.o.Metrics == nil {
		return
	}
	if n, err := l.svc.s.R().CountFailedGatewayRevocations(ctx, l.svc.s.Now().UnixMicro()); err == nil {
		l.o.Metrics.SetGatewayRevocationsFailed(n)
	}
}

func (l *Lifecycle) settle(ctx context.Context, r db.GatewayRevocation, state, msg string, now time.Time) error {
	msg = bounded(msg, 500)
	return l.svc.s.W().TxNamed(ctx, "harborpilot.revocation", func(tx *store.Tx) error {
		if state == "" {
			wait := revokeBackoff << min(2*r.Attempts, 20)
			wait = min(wait, revokeMaxWait)
			next := min(now.Add(wait).UnixMicro(), r.ExpiresAt)
			_, err := tx.Q.SetGatewayRevocationRetry(ctx, db.SetGatewayRevocationRetryParams{LastError: msg, NextAttemptAt: next, CredentialID: r.CredentialID})
			return err
		}
		n, err := tx.Q.SetGatewayRevocationDone(ctx, db.SetGatewayRevocationDoneParams{State: state, LastError: msg,
			Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, CredentialID: r.CredentialID})
		if err != nil || n == 0 || (state != "failed" && !strings.HasPrefix(msg, "the parent token")) {
			return err
		}
		// A refused revocation, or a parent that no longer authenticates, needs a person: say so in the audit log.
		l.svc.log().Error("harbor pilot revocation needs attention", "credential_id", r.CredentialID, "run_id", r.RunID, "state", state, "reason", msg)
		_, err = l.svc.audit.Append(ctx, tx, audit.Event{Actor: audit.System, Action: "harbor_pilot.revocation." + state, Target: audit.Target{Type: "harbor_pilot_credential", ID: r.CredentialID},
			Details: map[string]any{"run_id": r.RunID, "space_id": r.SpaceID, "reason": r.Reason, "error": msg}})
		return err
	})
}
