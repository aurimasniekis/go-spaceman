package fleet

import (
	"context"
	"errors"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// GatewayKindHarborPilot names the Harbor Pilot gateway in a run's launch spec and manifest (ADR-0060).
const GatewayKindHarborPilot = "harbor-pilot"

// GatewaySpec records that a run's model traffic goes through an LLM gateway instead of a direct account (ADR-0060
// decision 4). It is non-secret and stored with the run: the dispatch mints the run's child token from it.
type GatewaySpec struct {
	Kind      string `json:"kind" enum:"harbor-pilot"`
	ProjectID string `json:"project_id" doc:"The Harbor Pilot project the run is charged to"`
}

// Gateway routes the runs of the harnesses a space opted in through an LLM gateway (Harbor Pilot, ADR-0060). It is
// implemented outside fleet (harborpilot.Lifecycle); nil keeps every run on direct accounts.
type Gateway interface {
	// PreLaunch refuses a launch before its transaction (the network checks stay outside it): no connection, a budget
	// that is used up. It returns nil for a harness the space does not route through the gateway.
	PreLaunch(ctx context.Context, spaceID, harness string) error
	// Route decides inside the launch transaction whether the run goes through the gateway. nil: it does not, and the
	// space's direct accounts apply. A launch that names an account for a routed harness, or a model the space does not
	// allow, is refused: there is never a fallback to a direct account (Q1).
	Route(ctx context.Context, tx *store.Tx, spaceID, harness, accountRef, model string) (*GatewaySpec, error)
	// Token returns the secrets that carry a gateway run's child token, minting it on first use and reusing it on a
	// re-dispatch. session is the harness session the launch starts or resumes (the bound session ref). A failure
	// wraps ErrGatewayUnavailable (the run fails as gateway_unavailable) or ErrAgentTooOld.
	Token(ctx context.Context, run *Run, session string) (map[string]string, error)
}

// ErrGatewayUnavailable is returned (wrapped) when a gateway run's token can't be had: no connection, or Harbor Pilot
// refused or failed the mint. The run fails as gateway_unavailable; it never falls back to a direct account.
var ErrGatewayUnavailable = errors.New("the run's LLM gateway is unavailable")

// ErrAgentTooOld is returned (wrapped) when a gateway launch goes to a node whose agent can't deliver gateway
// credentials (HP-T4 refuses those); the run fails as agent_too_old.
var ErrAgentTooOld = errors.New("the node's agent is too old for gateway runs")

// Revocation reasons recorded with a queued gateway revocation (bounded, not free text).
const (
	RevokeRunEnded    = "run_ended"
	RevokeRunStopped  = "run_stopped"
	RevokeLaunchFail  = "launch_failed"
	RevokeRelocated   = "space_relocated"
	RevokePurged      = "space_purged"
	RevokeLeaseLost   = "lease_lost"
	RevokeReplaced    = "token_replaced"
	RevokeDuplicate   = "duplicate_mint"
	RevokeParentReset = "parent_rotated"
)

// queueGatewayRevocation queues the revocation of a run's live gateway token inside the end path's transaction (a crash
// can't lose it, ADR-0060 decision 5) and drops the sealed token. The revoker is woken after the commit. It reports
// whether anything was queued.
func (svc *Service) queueGatewayRevocation(ctx context.Context, tx *store.Tx, runID, reason string) (bool, error) {
	n, err := tx.Q.QueueRunGatewayRevocation(ctx, db.QueueRunGatewayRevocationParams{Reason: reason, Now: tx.NowMicros(), RunID: runID})
	if err != nil {
		return false, err
	}
	if _, err := tx.Q.ClearRunGatewayToken(ctx, runID); err != nil {
		return false, err
	}
	if n > 0 {
		svc.wakeRevoker(tx)
	}
	return n > 0, nil
}

// queueSpaceGatewayRevocations queues the revocation of every live gateway token of a space's runs (relocation, purge).
func (svc *Service) queueSpaceGatewayRevocations(ctx context.Context, tx *store.Tx, spaceID, reason string) error {
	n, err := tx.Q.QueueSpaceGatewayRevocations(ctx, db.QueueSpaceGatewayRevocationsParams{Reason: reason, Now: tx.NowMicros(), SpaceID: spaceID})
	if err != nil {
		return err
	}
	if _, err := tx.Q.ClearSpaceGatewayTokens(ctx, spaceID); err != nil {
		return err
	}
	if n > 0 {
		svc.wakeRevoker(tx)
	}
	return nil
}

func (svc *Service) wakeRevoker(tx *store.Tx) {
	if svc.opts.OnGatewayRevocation != nil {
		tx.AfterCommit(svc.opts.OnGatewayRevocation)
	}
}

// leaseSweepMargin is added to a lease's expiry and the disconnect grace before its unknown runs' gateway tokens are
// revoked: it covers the node's interrupt grace before the lease guard kills a run.
const leaseSweepMargin = time.Minute

// NextGatewayLeaseSweep is when the lease-loss sweep is next due (zero: nothing to sweep). Runs a node left unknown
// keep their gateway tokens until it reconnects; once its lease and the disconnect grace have passed, the node's lease
// guard has stopped them, so their tokens are revoked (SM-GW-005). The deadline timer calls it.
func (svc *Service) NextGatewayLeaseSweep(ctx context.Context) (time.Time, error) {
	at, err := svc.s.R().NextGatewayLeaseSweep(ctx)
	if err != nil || at == 0 {
		return time.Time{}, err
	}
	return time.UnixMicro(at).Add(svc.opts.DisconnectGrace + leaseSweepMargin), nil
}

// SweepGatewayLeases queues the revocation of the gateway tokens of runs that stayed unknown past their space's lease
// expiry and the disconnect grace. The runs' state is unchanged: they stay unknown until their node reports them
// (SM-NODE-005). It returns how many runs' tokens were queued.
func (svc *Service) SweepGatewayLeases(ctx context.Context) (int, error) {
	// The deadline timer calls this on every round: look before opening a write transaction.
	if ids, err := svc.s.R().ListGatewayLeaseLostRuns(ctx, svc.now().Add(-(svc.opts.DisconnectGrace + leaseSweepMargin)).UnixMicro()); err != nil || len(ids) == 0 {
		return 0, err
	}
	n := 0
	err := svc.s.W().TxNamed(ctx, "fleet.gateway_lease_sweep", func(tx *store.Tx) error {
		n = 0
		before := tx.Now.Add(-(svc.opts.DisconnectGrace + leaseSweepMargin)).UnixMicro()
		ids, err := tx.Q.ListGatewayLeaseLostRuns(ctx, before)
		if err != nil {
			return err
		}
		for _, id := range ids {
			queued, err := svc.queueGatewayRevocation(ctx, tx, id, RevokeLeaseLost)
			if err != nil {
				return err
			}
			if queued {
				n++
			}
		}
		return nil
	})
	return n, err
}
