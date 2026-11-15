package app_test

import (
	"context"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

// Security review SR-L06/SR-L07: deleting a transcript needs the same step-up as deleting a recording, and a viewer may
// ask the queue what is next but not store the decision.
func TestReviewDeleteStepUpAndQueueRecord(t *testing.T) {
	t.Parallel()
	rec, tr := api.RecordingRules["delete-recording"], api.UsageRules["delete-transcript"]
	if !tr.Reauth || !tr.MFA || tr.Perm != rec.Perm || !tr.Space {
		t.Fatalf("delete-transcript rule %+v, want the delete-recording step-up %+v", tr, rec)
	}

	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, err := h.c.Auth.IssueRootToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root := h.client()
	root.bearer = rootSecret
	en, _ := h.c.Fleet.CreateNode(ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_test"}, fleet.CreateNodeInput{Name: "node-a"})
	sp, err := h.c.Fleet.CreateSpace(ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_test"}, fleet.CreateSpaceInput{Slug: "queue-review", Name: "Q", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	grants := []map[string]string{{"role": "viewer", "domain": sp.ID}}
	expect(t, root.do("POST", "/users", map[string]any{"email": "vic@example.com", "display_name": "Vic", "password": "correct horse battery staple",
		"grants": grants}), 201)
	viewer := h.client()
	viewer.bearer = loginAndIssueToken(t, h, "vic@example.com", grants)
	expect(t, viewer.do("GET", "/spaces/"+sp.ID+"/queue/next", nil), 200)
	expect(t, viewer.do("GET", "/spaces/"+sp.ID+"/queue/next?record=true", nil), 403)
	expect(t, root.do("GET", "/spaces/"+sp.ID+"/queue/next?record=true", nil), 200)
}
