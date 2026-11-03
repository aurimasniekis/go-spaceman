package human

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Where a notification is routed (SM-HUM-009).
const (
	// TargetRun is one run: the requester's own, a run of its agent instance, or a live run of the successor role.
	TargetRun = "run"
	// TargetInstance waits for the next run of the requester's agent instance until the hold ends.
	TargetInstance = "instance"
	// TargetControl is the space's control run, whichever it is when it reads its messages.
	TargetControl = "control"
	// TargetOperators means no agent can take it: people see it as undeliverable on the action and in Messages.
	TargetOperators = "operators"
)

// Why a notification was routed away from the requester's run.
const (
	ReasonRequesterEnded = "requester_ended" // the requester's run had ended
	ReasonRunEnded       = "run_ended"       // the run it was routed to ended before acknowledging it
	ReasonHoldExpired    = "hold_expired"    // no run of the requester's instance started in time
	ReasonOperator       = "operator"        // a person routed it
	ReasonTooManyRoutes  = "too_many_routes" // it was routed too often
)

type route struct {
	kind, run, role, instance string
	hold                      sql.NullInt64
}

func (rt route) String() string {
	switch rt.kind {
	case TargetRun:
		return "run:" + rt.run
	case TargetInstance:
		return "instance:" + rt.instance
	}
	return rt.kind
}

func runEnded(r db.Run) bool {
	return r.Observed == "exited" || r.Observed == "failed" || r.Observed == "lost" || (r.Observed == "unknown" && r.Desired == "stopped")
}

// firstRoute is where a new notification goes: the requester's run while it has not ended; otherwise what the
// policy says for an ended requester.
func (svc *Service) firstRoute(ctx context.Context, tx *store.Tx, a db.HumanAction, p PolicyFields) (route, string, error) {
	if a.RequesterRun != "" {
		r, err := tx.Q.GetRun(ctx, a.RequesterRun)
		if err == nil && r.SpaceID == a.SpaceID && !runEnded(r) {
			return route{kind: TargetRun, run: r.ID, role: r.Role}, "", nil
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return route{}, "", err
		}
	}
	rt, err := svc.requesterEnded(ctx, tx, a, p, a.RequesterRun)
	return rt, ReasonRequesterEnded, err
}

// requesterEnded applies the requester_ended policy: keep it for the next run of the requester's instance, or route it on.
func (svc *Service) requesterEnded(ctx context.Context, tx *store.Tx, a db.HumanAction, p PolicyFields, ended string) (route, error) {
	if p.RequesterEnded == EndedNextLaunch && ids.Valid(ids.Instance, a.RequesterID) {
		id, err := tx.Q.LiveRunOfInstance(ctx, db.LiveRunOfInstanceParams{SpaceID: a.SpaceID, InstanceID: a.RequesterID})
		switch {
		case err == nil && id != ended:
			return route{kind: TargetRun, run: id, role: a.RequesterRole}, nil
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return route{}, err
		}
		hold := tx.Now.Add(time.Duration(p.HoldMinutes) * time.Minute).UnixMicro()
		return route{kind: TargetInstance, instance: a.RequesterID, role: a.RequesterRole, hold: sql.NullInt64{Int64: hold, Valid: true}}, nil
	}
	return svc.fallback(ctx, tx, a, p, ended)
}

// fallback is where a response goes when its run is gone: a live run of the successor role, else the space's control
// when one is wanted (running, or being restarted by the supervisor), else the operators. Never to another run.
func (svc *Service) fallback(ctx context.Context, tx *store.Tx, a db.HumanAction, p PolicyFields, notRun string) (route, error) {
	if p.SuccessorRole != "" {
		id, err := tx.Q.LiveRunOfRole(ctx, db.LiveRunOfRoleParams{SpaceID: a.SpaceID, Role: p.SuccessorRole, NotRun: notRun})
		if err == nil {
			return route{kind: TargetRun, run: id, role: p.SuccessorRole}, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return route{}, err
		}
	}
	n, err := tx.Q.CountWantedControlRuns(ctx, a.SpaceID)
	if err != nil {
		return route{}, err
	}
	if n > 0 {
		return route{kind: TargetControl, role: "control"}, nil
	}
	return route{kind: TargetOperators}, nil
}

// notify stores the durable, correlated notification of an outcome for an agent requester (SM-HUM-009).
func (svc *Service) notify(ctx context.Context, tx *store.Tx, a db.HumanAction, kind string) error {
	p, err := policyFor(ctx, tx.Q, a.SpaceID, a.Type)
	if err != nil {
		return err
	}
	rt, reason, err := svc.firstRoute(ctx, tx, a, p.PolicyFields)
	if err != nil {
		return err
	}
	nid := ids.New("hnt")
	role := rt.role
	if role == "" {
		role = a.RequesterRole
	}
	in := db.InsertHumanNotificationParams{ID: nid, ActionID: a.ID, SpaceID: a.SpaceID, TargetRun: rt.run, TargetSession: a.RequesterSession, TargetRole: role,
		Kind: kind, CreatedAt: tx.NowMicros(), TargetKind: rt.kind, TargetInstance: rt.instance, HoldUntil: rt.hold, Wake: b2i(p.WakeIdle), OriginalRun: a.RequesterRun}
	if reason != "" {
		in.RoutedFrom, in.RouteReason, in.RoutedAt, in.RouteCount = "run:"+a.RequesterRun, reason, sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, 1
	}
	if err := tx.Q.InsertHumanNotification(ctx, in); err != nil {
		return err
	}
	if reason != "" {
		return svc.rerouted(ctx, tx, audit.System, a, nid, kind, "run:"+a.RequesterRun, rt, reason)
	}
	return nil
}

// rerouted records a routing decision with its provenance: audit, outbound event, and, when no agent can take the
// response, a message to the operators (the response itself stays on the action).
func (svc *Service) rerouted(ctx context.Context, tx *store.Tx, actor audit.Actor, a db.HumanAction, nid, kind, from string, rt route, reason string) error {
	if err := svc.changes.Record(ctx, tx, a.SpaceID, "human_action", a.ID, "updated", a.Version); err != nil {
		return err
	}
	if err := svc.publish(ctx, tx, outbox.HumanActionRerouted{HumanAction: svc.meta(a, a.Status, a.Outcome), NotificationID: nid,
		To: rt.kind, RouteReason: reason}, a, ""); err != nil {
		return err
	}
	if rt.kind == TargetOperators {
		mid := ids.New(ids.Message)
		body := fmt.Sprintf("The %s of human action %s (%q) could not be delivered: it was for run %s, and %s. No successor role run or control run "+
			"can take it. Open the action in Needs you or the space's human actions to send it to control once control runs.",
			kind, a.ID, a.Title, a.RequesterRun, reasonText(reason))
		if err := tx.Q.InsertMessage(ctx, db.InsertMessageParams{ID: mid, SpaceID: a.SpaceID, FromKind: "system", FromLabel: "SpaceMan", ToKind: "operators",
			Kind: "notification", Subject: "A human response could not be delivered", Body: body, CorrelationID: a.ID, DedupKey: "undeliverable:" + nid,
			CreatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, a.SpaceID, "message", mid, "created", 1); err != nil {
			return err
		}
	}
	// The response also becomes an inbox item when the type's policy asks for it (SM-INBOX-001, ADR-0049).
	if err := svc.responseRouted(ctx, tx, a, nid, kind, rt, reason); err != nil {
		return err
	}
	tx.AfterCommit(svc.notified)
	_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "human_action.rerouted", Target: audit.Target{Type: "human_action", ID: a.ID},
		Details: map[string]any{"space_id": a.SpaceID, "notification_id": nid, "original_run": a.RequesterRun, "from": from, "to": rt.String(), "reason": reason}})
	return err
}

func reasonText(reason string) string {
	switch reason {
	case ReasonRequesterEnded:
		return "that run had ended"
	case ReasonRunEnded:
		return "the run it was routed to ended"
	case ReasonHoldExpired:
		return "no run of the requester's agent instance started in time"
	case ReasonTooManyRoutes:
		return "it was routed too many times"
	}
	return "it was routed by a person"
}

// reroute moves stranded notifications on: those whose run ended before acknowledging them, and those waiting for an
// instance whose hold ended (or whose instance has a run now). It runs at start, after run ends and on deadlines.
func (svc *Service) reroute(ctx context.Context) (int, error) {
	n := 0
	var errs []error
	stranded, err := svc.s.R().ListStrandedHumanNotifications(ctx, 200)
	if err != nil {
		return 0, err
	}
	for _, x := range stranded {
		err := svc.s.W().TxNamed(ctx, "human.reroute", func(tx *store.Tx) error {
			cur, a, p, err := loadNotification(ctx, tx, x.ID)
			if err != nil || cur.TargetKind != TargetRun || cur.TargetRun != x.TargetRun || (cur.State != "pending" && cur.State != "delivered") {
				return ignoreMissing(err)
			}
			var rt route
			reason := ReasonRunEnded
			switch {
			case cur.RouteCount >= maxReroutes:
				rt, reason = route{kind: TargetOperators}, ReasonTooManyRoutes
			case cur.TargetRun == cur.OriginalRun:
				reason = ReasonRequesterEnded
				rt, err = svc.requesterEnded(ctx, tx, a, p.PolicyFields, cur.TargetRun)
			default:
				rt, err = svc.fallback(ctx, tx, a, p.PolicyFields, cur.TargetRun)
			}
			if err != nil {
				return err
			}
			if err := svc.move(ctx, tx, audit.System, a, cur, rt, reason); err != nil {
				return err
			}
			n++
			return nil
		})
		if err != nil {
			errs = append(errs, err) // one failing row never starves the others
		}
	}
	held, err := svc.s.R().ListHeldHumanNotifications(ctx, 500)
	if err != nil {
		return n, errors.Join(append(errs, err)...)
	}
	now := svc.clock.Now().UnixMicro()
	for _, x := range held {
		err := svc.s.W().TxNamed(ctx, "human.hold", func(tx *store.Tx) error {
			cur, a, p, err := loadNotification(ctx, tx, x.ID)
			if err != nil || cur.TargetKind != TargetInstance || cur.State != "pending" {
				return ignoreMissing(err)
			}
			id, err := tx.Q.LiveRunOfInstance(ctx, db.LiveRunOfInstanceParams{SpaceID: cur.SpaceID, InstanceID: cur.TargetInstance})
			switch {
			case err == nil:
				// The instance runs again (a resumed session): deliver it there.
				if _, err := tx.Q.PinHumanNotification(ctx, db.PinHumanNotificationParams{RunID: id, ID: cur.ID}); err != nil {
					return err
				}
				tx.AfterCommit(svc.notified)
				n++
				return svc.changes.Record(ctx, tx, a.SpaceID, "human_action", a.ID, "updated", a.Version)
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
			if !cur.HoldUntil.Valid || cur.HoldUntil.Int64 > now {
				return nil
			}
			rt, err := svc.fallback(ctx, tx, a, p.PolicyFields, "")
			if err != nil {
				return err
			}
			n++
			return svc.move(ctx, tx, audit.System, a, cur, rt, ReasonHoldExpired)
		})
		if err != nil {
			errs = append(errs, err) // one failing row never starves the others
		}
	}
	return n, errors.Join(errs...)
}

func ignoreMissing(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func loadNotification(ctx context.Context, tx *store.Tx, id string) (db.HumanNotification, db.HumanAction, Policy, error) {
	cur, err := tx.Q.GetHumanNotification(ctx, id)
	if err != nil {
		return cur, db.HumanAction{}, Policy{}, err
	}
	a, err := tx.Q.GetHumanAction(ctx, cur.ActionID)
	if err != nil {
		return cur, a, Policy{}, err
	}
	p, err := policyFor(ctx, tx.Q, a.SpaceID, a.Type)
	return cur, a, p, err
}

func (svc *Service) move(ctx context.Context, tx *store.Tx, actor audit.Actor, a db.HumanAction, cur db.HumanNotification, rt route, reason string) error {
	from := route{kind: cur.TargetKind, run: cur.TargetRun, instance: cur.TargetInstance}.String()
	role := rt.role
	if role == "" {
		role = cur.TargetRole
	}
	n, err := tx.Q.RouteHumanNotification(ctx, db.RouteHumanNotificationParams{TargetKind: rt.kind, TargetRun: rt.run, TargetRole: role, TargetInstance: rt.instance,
		HoldUntil: rt.hold, RoutedFrom: from, RouteReason: reason, Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: cur.ID,
		FromKind: cur.TargetKind, FromRun: cur.TargetRun})
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.New(apperr.CodeConflict, "the notification changed concurrently")
	}
	return svc.rerouted(ctx, tx, actor, a, cur.ID, cur.Kind, from, rt, reason)
}

// RouteNotification lets a person route a response that is not acknowledged yet (typically an undeliverable one) to
// the space's control or to a live run of a role. to is "control" or "role:<name>".
func (svc *Service) RouteNotification(ctx context.Context, actor audit.Actor, spaceID, actionID, notificationID, to string) (*Action, error) {
	err := svc.s.W().TxNamed(ctx, "human.route", func(tx *store.Tx) error {
		cur, a, _, err := loadNotification(ctx, tx, notificationID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (cur.ActionID != actionID || a.SpaceID != spaceID)) {
			return apperr.New(apperr.CodeNotFound, "notification not found")
		} else if err != nil {
			return err
		}
		if cur.State != "pending" && cur.State != "delivered" {
			return apperr.New(apperr.CodeConflict, "the notification is already %s", cur.State)
		}
		var rt route
		switch {
		case to == TargetControl:
			rt = route{kind: TargetControl, role: "control"}
		case len(to) > 5 && to[:5] == "role:" && reRoleName.MatchString(to[5:]):
			id, err := tx.Q.LiveRunOfRole(ctx, db.LiveRunOfRoleParams{SpaceID: spaceID, Role: to[5:], NotRun: ""})
			if errors.Is(err, sql.ErrNoRows) {
				return apperr.New(apperr.CodePrecondition, "no run of role %s is running", to[5:])
			} else if err != nil {
				return err
			}
			rt = route{kind: TargetRun, run: id, role: to[5:]}
		default:
			return invalid("to", `must be "control" or "role:<name>"`)
		}
		return svc.move(ctx, tx, actor, a, cur, rt, ReasonOperator)
	})
	if err != nil {
		return nil, err
	}
	return svc.Get(ctx, spaceID, actionID)
}

// Viewer is a run reading human actions through its tools.
type Viewer struct {
	RunID, InstanceID string
	Control           bool
}

// related reports whether a run may read an action's response: the requester's run or agent instance, a run the
// response was routed to, or control. Other runs of the space see the request, never the response (SM-HUM-009).
func (svc *Service) related(ctx context.Context, a *Action, v Viewer, notified map[string]bool) bool {
	if v.Control || a.RequesterRun == v.RunID || (a.RequesterID != "" && a.RequesterID == v.InstanceID) {
		return true
	}
	return notified[a.ID]
}

// ForViewer hides the response, comments and reason of actions a run is not related to.
func (svc *Service) ForViewer(ctx context.Context, spaceID string, v Viewer, l ...*Action) ([]*Action, error) {
	ns, err := svc.s.R().ListHumanActionIDsNotifiedTo(ctx, db.ListHumanActionIDsNotifiedToParams{SpaceID: spaceID, TargetRun: v.RunID})
	if err != nil {
		return nil, err
	}
	notified := map[string]bool{}
	for _, id := range ns {
		notified[id] = true
	}
	for _, a := range l {
		if !svc.related(ctx, a, v, notified) {
			a.Response, a.Comments, a.Notifications, a.StatusReason, a.ResponseHidden = json0, nil, nil, "", true
		}
	}
	return l, nil
}
