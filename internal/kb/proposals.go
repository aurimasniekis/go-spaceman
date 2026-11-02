package kb

import (
	"context"
	"log/slog"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// ProposeInput offers a commit on the session's branch for acceptance.
type ProposeInput struct {
	Title     string `json:"title" maxLength:"200"`
	Summary   string `json:"summary,omitempty" maxLength:"20000" doc:"What changes and why (Markdown)"`
	Commit    string `json:"commit" maxLength:"64" doc:"The full commit ID on your branch"`
	RequestID string `json:"request_id,omitempty" maxLength:"64" doc:"The request this answers, if any"`
}

// Propose records a proposal from a KB session (SM-KB-005). Under the serialized policy SpaceMan accepts proposals
// one at a time; under review a maintainer decides; read-only KBs take no proposals.
func (svc *Service) Propose(ctx context.Context, p *fleet.RunPrincipal, in ProposeInput) (*Proposal, error) {
	_, sess, err := svc.ownRequest(ctx, p, "")
	if err != nil {
		return nil, err
	}
	k, err := svc.s.R().GetKB(ctx, sess.KbID)
	if err != nil {
		return nil, errNotFound
	}
	if k.WritePolicy == ReadOnly {
		return nil, apperr.New(apperr.CodeForbidden, "the %s knowledge base is read-only for support sessions", k.Slug)
	}
	v := &apperr.Validation{}
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" || len(in.Title) > 200 {
		v.Add("title", "is required (at most 200 characters)")
	}
	if !reCommit.MatchString(in.Commit) {
		v.Add("commit", "must be a full commit ID on your branch")
	}
	if err := v.Err(); err != nil {
		return nil, err
	}
	id := ids.New("kbp")
	err = svc.s.W().TxNamed(ctx, "kb.propose", func(tx *store.Tx) error {
		if err := tx.Q.InsertKBProposal(ctx, db.InsertKBProposalParams{ID: id, KbID: k.ID, SpaceID: k.SpaceID, SessionID: sess.ID, RequestID: in.RequestID,
			Title: in.Title, Summary: in.Summary, Branch: sess.Branch, CommitID: in.Commit, CreatedAt: tx.NowMicros(), UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, k.SpaceID, "kb", k.ID, "updated", 0); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorAgent, ID: p.RunID}, Action: "kb.proposed",
			Target: audit.Target{Type: "kb", ID: k.ID}, Details: map[string]any{"space_id": k.SpaceID, "proposal": id, "commit": in.Commit, "branch": sess.Branch}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.Kick()
	r, err := svc.s.R().GetKBProposal(ctx, id)
	if err != nil {
		return nil, err
	}
	out := proposalOut(r)
	return &out, nil
}

// Accept asks the node to merge a proposal (review policy: a maintainer's decision).
func (svc *Service) Accept(ctx context.Context, actor audit.Actor, spaceID, id string) (*Proposal, error) {
	p, err := svc.s.R().GetKBProposal(ctx, id)
	if err != nil || p.SpaceID != spaceID {
		return nil, apperr.New(apperr.CodeNotFound, "proposal not found")
	}
	k, err := svc.s.R().GetKB(ctx, p.KbID)
	if err != nil {
		return nil, errNotFound
	}
	if err := svc.startAccept(ctx, actor, k, p); err != nil {
		return nil, err
	}
	return svc.proposal(ctx, id)
}

func (svc *Service) proposal(ctx context.Context, id string) (*Proposal, error) {
	r, err := svc.s.R().GetKBProposal(ctx, id)
	if err != nil {
		return nil, err
	}
	out := proposalOut(r)
	return &out, nil
}

// startAccept queues kb.accept on the space's node. One acceptance runs per KB at a time, so acceptance is serialized.
func (svc *Service) startAccept(ctx context.Context, actor audit.Actor, k db.KbRepo, p db.KbProposal) error {
	if p.State != "proposed" && p.State != "conflict" && p.State != "failed" {
		return apperr.New(apperr.CodePrecondition, "the proposal is %s", p.State)
	}
	err := svc.s.W().TxNamed(ctx, "kb.accept", func(tx *store.Tx) error {
		if n, err := tx.Q.CountAcceptingProposals(ctx, k.ID); err != nil {
			return err
		} else if n > 0 {
			return apperr.New(apperr.CodeConflict, "another proposal of this knowledge base is being accepted; try again when it finishes")
		}
		cmd, err := svc.fleet.QueueSpaceCommand(ctx, tx, k.SpaceID, proto.CmdKBAccept, "kb-accept:"+p.ID+":"+ids.New("try"), proto.KBAcceptPayload{
			ProposalID: p.ID, Repo: k.Repo, Branch: p.Branch, Commit: p.CommitID, Target: k.BaseBranch, Validator: parseList(k.ValidatorJson), Timeout: 120,
			Message: "Accept KB proposal: " + p.Title})
		if err != nil {
			return err
		}
		n, err := tx.Q.SetKBProposalState(ctx, db.SetKBProposalStateParams{State: "accepting", MergedCommit: "", Detail: "", CommandID: cmd, DecidedBy: actor.ID,
			Now: tx.NowMicros(), ID: p.ID, Expected: p.State})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodeConflict, "the proposal changed concurrently")
		}
		if err := svc.changes.Record(ctx, tx, k.SpaceID, "kb", k.ID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "kb.accept_requested", Target: audit.Target{Type: "kb", ID: k.ID},
			Details: map[string]any{"space_id": k.SpaceID, "proposal": p.ID, "commit": p.CommitID, "target": k.BaseBranch}})
		return err
	})
	if err != nil {
		return err
	}
	svc.fleet.KickSpace(ctx, k.SpaceID)
	return nil
}

// acceptNext accepts the oldest proposal of a serialized KB when none is in flight.
func (svc *Service) acceptNext(ctx context.Context, k db.KbRepo) {
	if n, err := svc.s.R().CountAcceptingProposals(ctx, k.ID); err != nil || n > 0 {
		return
	}
	p, err := svc.s.R().NextSerializedProposal(ctx, k.ID)
	if err != nil {
		return
	}
	if err := svc.startAccept(ctx, system, k, p); err != nil && apperr.CodeOf(err) != apperr.CodeConflict {
		svc.log.Warn("serialized KB acceptance", slog.String("kb", k.Slug), slog.String("proposal", p.ID), slog.Any("error", err))
	}
}

// Reject declines a proposal; the session's branch is left as it is.
func (svc *Service) Reject(ctx context.Context, actor audit.Actor, spaceID, id, reason string) (*Proposal, error) {
	p, err := svc.s.R().GetKBProposal(ctx, id)
	if err != nil || p.SpaceID != spaceID {
		return nil, apperr.New(apperr.CodeNotFound, "proposal not found")
	}
	err = svc.s.W().TxNamed(ctx, "kb.reject", func(tx *store.Tx) error {
		n, err := tx.Q.SetKBProposalState(ctx, db.SetKBProposalStateParams{State: "rejected", Detail: reason, CommandID: p.CommandID, DecidedBy: actor.ID,
			Now: tx.NowMicros(), ID: id, Expected: p.State})
		if err != nil {
			return err
		}
		if n == 0 || p.State == "accepting" || p.State == "accepted" {
			return apperr.New(apperr.CodePrecondition, "the proposal is %s", p.State)
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "kb", p.KbID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "kb.rejected", Target: audit.Target{Type: "kb", ID: p.KbID},
			Details: map[string]any{"space_id": spaceID, "proposal": id}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.notifySession(ctx, p, "rejected", reason)
	return svc.proposal(ctx, id)
}

// OnResult records the node's kb.accept outcome. A result for a proposal that is no longer being accepted (a repeat
// after reconnecting) changes nothing.
func (svc *Service) OnResult(ctx context.Context, nodeID string, res proto.KBResult) {
	p, err := svc.s.R().GetKBProposal(ctx, res.ProposalID)
	if err != nil || p.State != "accepting" || p.CommandID != res.CommandID {
		return
	}
	if sp, err := svc.s.R().GetSpace(ctx, p.SpaceID); err != nil || sp.NodeID != nodeID {
		return
	}
	state := map[string]string{proto.KBMerged: "accepted", proto.KBConflict: "conflict"}[res.Result]
	if state == "" {
		state = "failed"
	}
	merged := ""
	if state == "accepted" && reCommit.MatchString(res.Merged) {
		merged = res.Merged
	}
	detail := res.Detail
	if len(detail) > 2000 {
		detail = detail[:2000]
	}
	err = svc.s.W().TxNamed(ctx, "kb.result", func(tx *store.Tx) error {
		n, err := tx.Q.SetKBProposalState(ctx, db.SetKBProposalStateParams{State: state, MergedCommit: merged, Detail: detail, CommandID: p.CommandID,
			DecidedBy: p.DecidedBy, Now: tx.NowMicros(), ID: p.ID, Expected: "accepting"})
		if err != nil || n == 0 {
			return err
		}
		if err := svc.changes.Record(ctx, tx, p.SpaceID, "kb", p.KbID, "updated", 0); err != nil {
			return err
		}
		if state == "accepted" {
			if _, err := svc.Outbox.Enqueue(ctx, tx, outbox.Event{Payload: outbox.KBProposalAccepted{SpaceID: p.SpaceID, KBID: p.KbID, ProposalID: p.ID,
				SessionID: p.SessionID, Commit: p.CommitID, MergedCommit: merged}, Correlation: p.ID, Causation: p.CommandID}); err != nil {
				return err
			}
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorNode, ID: nodeID}, Action: "kb.proposal_" + state,
			Target: audit.Target{Type: "kb", ID: p.KbID}, Details: map[string]any{"space_id": p.SpaceID, "proposal": p.ID, "merged": merged}})
		return err
	})
	if err != nil {
		svc.log.Warn("record KB acceptance", slog.String("proposal", p.ID), slog.Any("error", err))
		return
	}
	svc.notifySession(ctx, p, state, detail)
	svc.Kick()
}

// notifySession tells the proposing session what happened to its proposal.
func (svc *Service) notifySession(ctx context.Context, p db.KbProposal, what, detail string) {
	sess, err := svc.s.R().GetKBSession(ctx, p.SessionID)
	if err != nil || sess.RunID == "" {
		return
	}
	k, err := svc.s.R().GetKB(ctx, p.KbID)
	if err != nil {
		return
	}
	body := "Your proposal " + p.ID + " (" + p.Title + ") was " + what + "."
	if detail != "" {
		body += "\n\n" + detail
	}
	if what == "conflict" {
		body += "\n\nNothing was overwritten: merge the KB's " + k.BaseBranch + " branch into yours, resolve, commit and propose again."
	}
	if !svc.running(ctx, sess) {
		return // the session's next run reads the proposal state from the KB page if it needs it
	}
	_, _, _ = svc.msgs.Send(ctx, system, p.SpaceID, messages.From{Kind: "system", ID: "kb:" + k.Slug, Label: "KB " + k.Name},
		messages.SendInput{To: "run:" + sess.RunID, Kind: "notification", Subject: "KB proposal " + what, Body: body, CorrelationID: p.ID,
			DedupKey: "kb-proposal:" + p.ID + ":" + what + ":" + p.CommandID})
}
