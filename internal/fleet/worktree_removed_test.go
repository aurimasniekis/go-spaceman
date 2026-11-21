package fleet

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// ADR-0107 decision 4: a worktree.removed report reaches the hook with the node that sent it, only from a session that
// negotiated minor 16, and only when it is well formed.
func TestWorktreeRemovedReport(t *testing.T) {
	e := ulEnv(t)
	got := make(chan proto.WorktreeRemoved, 4)
	e.svc.hub.WorktreeRemoved = func(_ context.Context, nodeID string, rep proto.WorktreeRemoved) {
		if nodeID != "nod_1" {
			t.Errorf("node %s", nodeID)
		}
		got <- rep
	}
	rep := proto.WorktreeRemoved{CommandID: ids.New(ids.Command), SpaceID: "spc_" + strings.Repeat("a", 26), Repo: "repos/api", Name: "wt", Result: proto.WorktreeRefused, Code: "dirty", Reason: "changed", CheckedAt: e.t0}
	send := func(rep proto.WorktreeRemoved, minor int) error {
		t.Helper()
		b, err := proto.Encode(ids.New(ids.Message), proto.TypeWorktreeRemoved, e.t0, rep)
		if err != nil {
			t.Fatal(err)
		}
		env, err := proto.Decode(b)
		if err != nil {
			t.Fatal(err)
		}
		return e.svc.hub.handle(e.ctx, &session{nodeID: "nod_1", seq: 1, minor: minor, log: slog.New(slog.DiscardHandler)}, env)
	}
	if err := send(rep, proto.MinorWorktreeRemove-1); err == nil {
		t.Error("a report below minor 16 accepted")
	}
	bad := rep
	bad.Result = "gone"
	if err := send(bad, proto.MinorWorktreeRemove); err == nil {
		t.Error("a malformed report accepted")
	}
	select {
	case r := <-got:
		t.Fatalf("a refused report reached the hook: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	if err := send(rep, proto.MinorWorktreeRemove); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		if r != rep {
			t.Fatalf("got %+v want %+v", r, rep)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the report did not reach the hook")
	}
}

// ADR-0107, ADR-0057: the repository's key goes on the wire of a space.worktree.remove only, for a node that negotiated
// repository keys; the stored payload holds none.
func TestWorktreeRemoveKeyOnWireOnly(t *testing.T) {
	const pem = "-----BEGIN OPENSSH PRIVATE KEY-----\nx\n-----END OPENSSH PRIVATE KEY-----\n"
	stored := proto.WorktreeRemovePayload{Repository: proto.RepoSource{ID: "rep_api", Path: "repos/api", URL: "git@github.com:acme/api.git", SSHKey: pem}, Name: "wt"}.Redacted()
	b, _ := json.Marshal(stored)
	if strings.Contains(string(b), "PRIVATE KEY") {
		t.Fatalf("stored payload holds the key: %s", b)
	}
	c := db.NodeCommand{ID: "cmd_1", Kind: proto.CmdWorktreeRemove, SpaceID: "spc_a", PayloadJson: string(b)}
	h := &Hub{RepoKey: func(_ context.Context, spaceID, repoID string) (string, error) {
		if spaceID != "spc_a" || repoID != "rep_api" {
			t.Errorf("key asked for %s/%s", spaceID, repoID)
		}
		return pem, nil
	}}
	msg, err := h.materialize(context.Background(), c, "nod_1", proto.MaxMinor)
	if err != nil {
		t.Fatal(err)
	}
	var wire proto.WorktreeRemovePayload
	if err := json.Unmarshal(msg.Payload, &wire); err != nil || wire.Repository.SSHKey != pem || wire.Name != "wt" || wire.Repository.URL != stored.Repository.URL {
		t.Fatalf("wire payload %+v %v", wire, err)
	}
	if strings.Contains(c.PayloadJson, "PRIVATE KEY") {
		t.Fatal("materialize changed the stored payload")
	}
}
