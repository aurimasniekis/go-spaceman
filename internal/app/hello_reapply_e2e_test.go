package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// helloRuns is the number of ended runs the node's journal re-reports in every hello.
const helloRuns = 400

// Incident regression (a node with a long journal reconnecting): every hello re-reports every run in the node's
// journal, ended ones included. A report that changes nothing must write nothing: no version bump and no change-feed
// entry. Otherwise each reconnect makes every open UI tab refetch every space's runs list once per run. It must also
// apply quickly, because the connection's session reads nothing while it does. A report that does change something
// still applies.
func TestHelloReapplyOfUnchangedRunsWritesNothing(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	n, sp := hvNode(t, e, "journal", proto.MaxMinor, true)
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Role: "free", Kind: "worker", Harness: "claude-code", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	n.next(t, proto.TypeCommand)
	// The node's history: helloRuns copies of the run, all live as far as central knows.
	ids := make([]string, 0, helloRuns)
	for i := range helloRuns {
		ids = append(ids, fmt.Sprintf("run_01m7hello%016d", i))
	}
	if err := e.c.Store.W().Tx(ctx, func(tx *store.Tx) error {
		if _, err := tx.SQL.ExecContext(ctx, `CREATE TEMP TABLE hist AS SELECT * FROM runs WHERE id = ?`, r.ID); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.SQL.ExecContext(ctx, `UPDATE hist SET id = ?, observed = 'running'`, id); err != nil {
				return err
			}
			if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO runs SELECT * FROM hist`); err != nil {
				return err
			}
		}
		_, err := tx.SQL.ExecContext(ctx, `DROP TABLE hist`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_ = n.conn.CloseNow()

	reports := func(status string) []proto.RunReport {
		out := make([]proto.RunReport, 0, len(ids))
		for _, id := range ids {
			code := 0
			out = append(out, proto.RunReport{RunID: id, SpaceID: sp.ID, Generation: r.Generation, State: proto.RunExited, TerminalID: "term_1",
				CoreID: "core_1", Identity: "ident", PID: 4242, ExitCode: &code, Reason: "exited", Status: status, StatusKind: status, Turn: 3,
				SessionID: "0193e0b6-1d7c-7c8e-9d2b-0123456789ab", HarnessVersion: "2.1.323", HarnessRequested: "latest", ObservedAt: time.Now()})
		}
		return out
	}
	var changes, versions int64
	counts := func() (int64, int64) {
		t.Helper()
		var c, v int64
		if err := e.c.Store.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM changes WHERE resource_type = 'run'`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		if err := e.c.Store.ReadDB().QueryRowContext(ctx, `SELECT coalesce(sum(version), 0) FROM runs`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return c, v
	}
	connect := func(runs []proto.RunReport) time.Duration {
		t.Helper()
		start := time.Now()
		n := dialRawNodeHello(t, e, proto.Hello{AgentVersion: "test", MinMinor: proto.MinMinor, MaxMinor: proto.MaxMinor, Runs: runs, Leases: []proto.LeaseState{}})
		n.next(t, proto.TypeWelcome)
		// The reconcile is sent once every hello report is applied.
		deadline := time.After(2 * time.Minute)
		for {
			select {
			case env, ok := <-n.ch:
				if !ok {
					t.Fatal("connection closed before the reconcile")
				}
				if env.Type != proto.TypeReconcile {
					continue
				}
			case <-deadline:
				t.Fatal("no reconcile")
			}
			break
		}
		took := time.Since(start)
		_ = n.conn.CloseNow()
		eventually(t, "disconnected", func() bool { nn, _ := e.c.Fleet.GetNode(ctx, n2id(t, e)); return !nn.Connected })
		return took
	}

	// The first hello ends the runs central still thinks are live: each is a real transition, with exactly one change.
	before, _ := counts()
	connect(reports("done"))
	changes, versions = counts()
	if changes-before != helloRuns {
		t.Fatalf("a hello ending %d live runs wrote %d change entries; want one each", helloRuns, changes-before)
	}
	if got := e.run1(ids[0]); got.Observed != proto.RunExited || got.EndedAt == nil {
		t.Fatalf("ended run %+v", got)
	}
	// The same journal again (the node reconnected): nothing changes, so nothing is written.
	took := connect(reports("done"))
	c, v := counts()
	if c != changes || v != versions {
		t.Fatalf("a hello of %d unchanged runs wrote %d change entries and %d version bumps; want none", helloRuns, c-changes, v-versions)
	}
	// Generous for -race on a small runner; the unfixed per-run writes take several seconds per thousand runs on a disk.
	if took > 20*time.Second {
		t.Fatalf("applying a hello of %d unchanged runs took %v", helloRuns, took)
	}
	t.Logf("hello of %d unchanged runs applied in %v", helloRuns, took)

	// One run's report differs: exactly that run is updated. Another differs but names an older lease generation: it
	// is fenced, as before.
	again := reports("done")
	again[7].Status, again[7].StatusKind = "error", "error"
	again[8].Status, again[8].Generation = "error", r.Generation-1
	connect(again)
	c, v = counts()
	if c != changes+1 || v <= versions {
		t.Fatalf("a hello with one changed run wrote %d change entries (version sum %d → %d); want 1", c-changes, versions, v)
	}
	if got := e.run1(ids[8]); got.Status != "done" {
		t.Fatalf("a report of an older generation was applied: status %q", got.Status)
	}
	if got := e.run1(ids[7]); got.Status != "error" {
		t.Fatalf("changed run's status %q", got.Status)
	}
}

// n2id is the only enrolled node's ID.
func n2id(t *testing.T, e *fleetEnv) string {
	t.Helper()
	nodes, err := e.c.Fleet.ListNodes(context.Background())
	if err != nil || len(nodes) == 0 {
		t.Fatalf("nodes %v %v", nodes, err)
	}
	return nodes[0].ID
}

// Run lists leave out each run's manifest (its resolved prompts and settings: large, and the UI refetches lists on every
// run change) unless include=manifest asks for it; a single run always has it.
func TestRunListOmitsManifest(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	e.enroll()
	ctx := context.Background()
	n, sp := hvNode(t, e, "lists", proto.MaxMinor, true)
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Role: "free", Kind: "worker", Harness: "claude-code", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	n.next(t, proto.TypeCommand)
	call := fleetCaller{t: t, url: e.srv.URL}
	tok := userToken(t, e.c.Auth, "vera@example.com", []auth.GrantInput{{Role: authz.RoleViewer, Domain: sp.ID}})
	base := "/spaces/" + sp.ID + "/runs"
	var list struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	call.want(tok, "GET", base, nil, 200, &list)
	if len(list.Items) != 1 || list.Items[0]["manifest"] != nil || list.Items[0]["id"] == nil {
		t.Fatalf("list %v", list.Items)
	}
	call.want(tok, "GET", base+"?include=manifest", nil, 200, &list)
	if len(list.Items) != 1 || len(list.Items[0]["manifest"]) < 10 {
		t.Fatalf("list with manifests %v", list.Items)
	}
	call.want(tok, "GET", base+"?include=everything", nil, 422, nil)
	var one map[string]json.RawMessage
	call.want(tok, "GET", base+"/"+r.ID, nil, 200, &one)
	if len(one["manifest"]) < 10 {
		t.Fatalf("run %v", one)
	}
}
