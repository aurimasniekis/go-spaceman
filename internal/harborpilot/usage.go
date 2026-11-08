package harborpilot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Run-end usage read-back (ADR-0060 decision 7, HP-T5). A gateway run's end path queues it next to the token's
// revocation (fleet.GatewayUsageFirstRead after the end). Harbor Pilot writes its ledger as requests finish, so the
// figures are read at each of UsageReads after the end; the last read is final. Each read replaces the previous one.
var UsageReads = []time.Duration{fleet.GatewayUsageFirstRead, 2 * time.Minute, 10 * time.Minute}

const (
	usageBatch   = 10
	usageTimeout = 10 * time.Second
	// A read that keeps failing in a way worth retrying is retried (30 s, ×2, at most 5 min) until usageGiveUp after the
	// run's end, then the row is failed and the run keeps what it has.
	usageBackoff  = 30 * time.Second
	usageMaxWait  = 5 * time.Minute
	usageGiveUp   = time.Hour
	usageSlack    = time.Minute
	usageModelMax = 50
)

// NextUsageRead is when the earliest queued usage read is due (zero: none). The deadline timer calls it.
func (l *Lifecycle) NextUsageRead(ctx context.Context) (time.Time, error) {
	at, err := l.svc.s.R().NextGatewayUsageRead(ctx)
	if err != nil || at == 0 {
		return time.Time{}, err
	}
	return time.UnixMicro(at).UTC(), nil
}

// ReadDueUsage reads back the usage of ended gateway runs that is due, outside any transaction, with the connection's
// service-principal token: GET /api/v1/usage/summary?ref=spaceman.run:<run>&binding_id=<binding>&group_by=model. The
// binding is always passed: ref values are chosen by whoever mints, and only the binding makes them SpaceMan's.
//
// The figures are stored with the run (shown as gateway-observed), and fill the run's usage with source harbor-pilot
// only where the harness reported none (Codex); a harness's own usage is never overwritten. Without a connection or a
// service-principal token the row is settled as unavailable; a refusal (401, 403, 404, 400) fails it; a network
// failure, 5xx or 429 is retried until an hour after the run's end. A failed read never writes zero usage.
func (l *Lifecycle) ReadDueUsage(ctx context.Context) (int, error) {
	now := l.svc.s.Now()
	due, err := l.svc.s.R().ListDueGatewayUsage(ctx, db.ListDueGatewayUsageParams{Now: now.UnixMicro(), Lim: usageBatch})
	if err != nil || len(due) == 0 {
		return 0, err
	}
	// Every due row is settled or rescheduled, whatever goes wrong: a row left due would make the timer fire again at once.
	var c *Client
	token, binding, unusable := "", "", ""
	conn, err := l.svc.s.R().GetHarborPilotConnection(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		unusable = "no Harbor Pilot connection is configured, so the run's usage can't be read back"
	case err != nil:
		return 0, err
	case len(conn.ServiceTokenEnc) == 0:
		unusable = "the Harbor Pilot connection has no service-principal token (usage.read), so the run's usage can't be read back"
	case conn.BindingID == "":
		unusable = "the Harbor Pilot connection's client binding is unknown, so the run's usage can't be read back"
	default:
		if token, err = l.svc.open(serviceAAD(conn.ID), conn.ServiceTokenEnc, "service-principal"); err != nil {
			l.svc.log().Error("harbor pilot usage: the service-principal token can't be opened", "err", err)
			unusable = "the service-principal token can't be opened"
		} else if c, err = l.svc.client(conn.BaseUrl, conn.AllowInsecureHttp == 1); err != nil {
			unusable = "the connection's address is not usable"
		}
		binding = conn.BindingID
	}
	done := 0
	var errs []error
	// A round shares the deadline timer with human-action expiry and the other deadlines, so it is bounded: once a read
	// fails in transport (Harbor Pilot unreachable, or not answering within usageTimeout), the round's other rows are
	// rescheduled without asking Harbor Pilot. A gateway that swallows requests costs one timeout per round, not one
	// per row.
	unreachable := false
	for _, r := range due {
		if c == nil {
			errs = append(errs, l.usageFailed(ctx, r, "unavailable", unusable, now))
			done++
			continue
		}
		if unreachable {
			errs = append(errs, l.usageSkip(ctx, r, "not read: Harbor Pilot did not answer an earlier read in the same round", now.Add(usageBackoff)))
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, usageTimeout)
		sum, err := c.UsageSummary(cctx, token, UsageQuery{GroupBy: "model", Refs: map[string]string{RefRun: r.RunID}, BindingID: binding,
			From: time.UnixMicro(r.RunCreatedAt).Add(-usageSlack), To: now.Add(usageSlack)})
		cancel()
		if err != nil {
			var he *Error
			ended := time.UnixMicro(r.EndedAt)
			msg := "the read failed"
			if errors.As(err, &he) {
				msg = "Harbor Pilot usage could not be read: " + he.Reason()
			}
			if !errors.As(err, &he) || he.Class == ClassTransport {
				unreachable = true
			}
			if (errors.As(err, &he) && !he.Retryable()) || now.Sub(ended) >= usageGiveUp {
				errs = append(errs, l.usageFailed(ctx, r, "failed", msg, now))
				done++
				continue
			}
			wait := min(usageBackoff<<min(r.Failures, 10), usageMaxWait)
			errs = append(errs, l.usageRetry(ctx, r, msg, now.Add(wait)))
			continue
		}
		if err := l.storeUsage(ctx, r, sum, now); err != nil {
			errs = append(errs, err)
			continue
		}
		done++
	}
	return done, errors.Join(errs...)
}

func (l *Lifecycle) usageFailed(ctx context.Context, r db.ListDueGatewayUsageRow, state, msg string, now time.Time) error {
	return l.svc.s.W().TxNamed(ctx, "harborpilot.usage_failed", func(tx *store.Tx) error {
		n, err := tx.Q.SetGatewayUsageFailure(ctx, db.SetGatewayUsageFailureParams{State: state, NextAttemptAt: now.UnixMicro(), LastError: bounded(msg, 500),
			Now: tx.NowMicros(), RunID: r.RunID})
		if err != nil || n == 0 {
			return err
		}
		return l.svc.changes.Record(ctx, tx, r.SpaceID, "run", r.RunID, "updated", 0)
	})
}

func (l *Lifecycle) usageRetry(ctx context.Context, r db.ListDueGatewayUsageRow, msg string, next time.Time) error {
	return l.svc.s.W().TxNamed(ctx, "harborpilot.usage_retry", func(tx *store.Tx) error {
		_, err := tx.Q.SetGatewayUsageFailure(ctx, db.SetGatewayUsageFailureParams{State: "pending", NextAttemptAt: next.UnixMicro(), LastError: bounded(msg, 500),
			Now: tx.NowMicros(), RunID: r.RunID})
		return err
	})
}

// usageSkip reschedules a read that was not attempted. No call was made, so it doesn't count as a failure (and doesn't
// lengthen the row's backoff).
func (l *Lifecycle) usageSkip(ctx context.Context, r db.ListDueGatewayUsageRow, msg string, next time.Time) error {
	return l.svc.s.W().TxNamed(ctx, "harborpilot.usage_skip", func(tx *store.Tx) error {
		_, err := tx.Q.RescheduleGatewayUsage(ctx, db.RescheduleGatewayUsageParams{NextAttemptAt: next.UnixMicro(), LastError: bounded(msg, 500),
			Now: tx.NowMicros(), RunID: r.RunID})
		return err
	})
}

// GatewayUsage is a run's usage as one read of Harbor Pilot's summary gives it.
type GatewayUsage struct {
	Models       []proto.ModelUsage
	Costs        []CostAmount
	Requests     int64
	UnknownUsage int64
}

// RunUsage turns a usage summary grouped by model into per-model usage. Token counts of a model whose every request
// went without reported usage are unknown (nil), never zero.
func RunUsage(sum *UsageSummary) GatewayUsage {
	out := GatewayUsage{Models: []proto.ModelUsage{}, Costs: cleanCosts(sum.Totals)}
	for _, g := range sum.Groups {
		out.Requests += g.Attempts
		out.UnknownUsage += g.Tokens.UnknownUsage
		if g.Attempts == 0 || len(out.Models) >= usageModelMax {
			continue
		}
		m := proto.ModelUsage{Model: bounded(Sanitize(g.Key), 128), Requests: g.Attempts}
		if m.Model == "" {
			m.Model = "unknown"
		}
		if g.Tokens.UnknownUsage < g.Attempts {
			in, o, cw, cr := g.Tokens.Input, g.Tokens.Output, g.Tokens.CacheWrite, g.Tokens.CacheRead
			m.Input, m.Output, m.CacheWrite, m.CacheRead = &in, &o, &cw, &cr
		}
		out.Models = append(out.Models, m)
	}
	return out
}

// estimate is the run's cost as Harbor Pilot priced it, in USD: its actual charge when it has one, else its estimate.
func estimate(costs []CostAmount) *proto.CostEstimate {
	for _, kind := range []string{"actual_api_charge", "estimated_api_charge"} {
		for _, c := range costs {
			if c.Kind != kind || c.Currency != "USD" {
				continue
			}
			v, err := strconv.ParseFloat(c.Amount, 64)
			if err != nil {
				continue
			}
			return &proto.CostEstimate{USD: v, Source: proto.UsageHarborPilot,
				Assumptions: fmt.Sprintf("Harbor Pilot's %s for the run's requests through the gateway (gateway-observed, read back after the run ended)", kind)}
		}
	}
	return nil
}

func (l *Lifecycle) storeUsage(ctx context.Context, r db.ListDueGatewayUsageRow, sum *UsageSummary, now time.Time) error {
	u := RunUsage(sum)
	final := r.Reads+1 >= int64(len(UsageReads))
	state, next := "final", now
	if !final {
		state = "pending"
		next = time.UnixMicro(r.EndedAt).Add(UsageReads[r.Reads+1])
	}
	models, _ := json.Marshal(u.Models)
	costs, _ := json.Marshal(u.Costs)
	est := ""
	if e := estimate(u.Costs); e != nil {
		b, _ := json.Marshal(e)
		est = string(b)
	}
	note := "Observed by the Harbor Pilot gateway and read back after the run ended (the harness reported no usage)"
	if !final {
		note += ", not final yet"
	}
	if u.UnknownUsage > 0 {
		note += fmt.Sprintf("; %d of %d requests had no usage reported by the provider (not counted)", u.UnknownUsage, u.Requests)
	}
	return l.svc.s.W().TxNamed(ctx, "harborpilot.usage", func(tx *store.Tx) error {
		n, err := tx.Q.SetGatewayUsageRead(ctx, db.SetGatewayUsageReadParams{State: state, NextAttemptAt: next.UnixMicro(), Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
			ModelsJson: string(models), CostsJson: string(costs), Attempts: u.Requests, UnknownUsage: u.UnknownUsage, RunID: r.RunID})
		if err != nil || n == 0 {
			return err
		}
		if u.Requests == 0 {
			// An empty answer (no request seen yet, ledger lag, or a binding change) is not known zero usage: it is kept
			// in run_gateway_usage only, and the run's own usage stays as the harness reported it (unknown for Codex).
			return nil
		}
		if err := tx.Q.FillRunUsageFromGateway(ctx, db.FillRunUsageFromGatewayParams{RunID: r.RunID, SpaceID: r.SpaceID, ModelsJson: string(models),
			EstimateJson: est, Final: b2i(final), Note: note, UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		return l.svc.changes.Record(ctx, tx, r.SpaceID, "run", r.RunID, "updated", 0)
	})
}
