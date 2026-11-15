package approvals

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// An action already stored under approval:<id> that this service did not write (one from before the namespace was
// reserved) is never linked: the approval ends and the next call asks again under a new ID (ADR-0087).
func TestApprovalRefusesForeignActionUnderItsKey(t *testing.T) {
	const op, args = "spaceman.task_create", `{"title":"Deploy"}`
	for _, c := range []struct {
		name string
		edit func(in *human.CreateInput)
	}{
		{"same", func(*human.CreateInput) {}},
		{"body", func(in *human.CreateInput) { in.Body = "Harmless: approve." }},
		{"title", func(in *human.CreateInput) { in.Title = "Approve something else" }},
		{"type", func(in *human.CreateInput) { in.Type, in.Request = human.TypeQuestion, json.RawMessage(`{"allow_text":true}`) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFx(t)
			f.policy(op, 1)
			a := f.gateRequired(run1, op, args)
			genuine, err := f.human.Get(f.ctx, "spc_a", a.HumanActionID)
			must(t, err)
			// A crash between storing the approval and asking, with an action of someone else's already under its key.
			must(t, f.s.W().TxNamed(f.ctx, "unlink", func(tx *store.Tx) error {
				if _, err := tx.SQL.ExecContext(f.ctx, `UPDATE approvals SET human_action_id = '' WHERE id = ?`, a.ID); err != nil {
					return err
				}
				_, err := tx.SQL.ExecContext(f.ctx, `DELETE FROM human_actions WHERE id = ?`, a.HumanActionID)
				return err
			}))
			in := human.CreateInput{Type: human.TypeApproval, Title: genuine.Title, Body: genuine.Body, Request: genuine.Request,
				DedupKey: "approval:" + a.ID, Internal: true, Requester: human.Requester{Kind: genuine.RequesterKind, ID: genuine.RequesterID, Role: genuine.RequesterRole, Run: genuine.RequesterRun, Label: genuine.RequesterLabel}}
			c.edit(&in)
			_, _, err = f.human.Create(f.ctx, admin, "spc_a", in)
			must(t, err)
			finish, err := f.svc.Gate(f.ctx, run1, op, "spaceman", json.RawMessage(args))
			got, gerr := f.s.R().GetApproval(f.ctx, a.ID)
			must(t, gerr)
			if c.name == "same" {
				// The approval flow's own action (a crash after asking) is linked as before.
				if finish != nil || got.HumanActionID == "" || got.State != Pending {
					t.Fatalf("own action not linked: state %s, action %q, err %v", got.State, got.HumanActionID, err)
				}
				return
			}
			code(t, err, apperr.CodeConflict)
			if finish != nil || got.HumanActionID != "" || got.State != Revoked {
				t.Fatalf("approval after a foreign action: state %s, action %q", got.State, got.HumanActionID)
			}
			// The next call asks again under a new ID, with the genuine text.
			n := f.gateRequired(run1, op, args)
			if n.ID == a.ID {
				t.Fatal("the revoked approval was reused")
			}
			act, err := f.human.Get(f.ctx, "spc_a", n.HumanActionID)
			must(t, err)
			if act.Type != human.TypeApproval || !strings.Contains(act.Body, n.ArgsDigest) {
				t.Fatalf("new approval's action: %+v", act)
			}
		})
	}
}
