package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/events"
	"github.com/aurimasniekis/go-spaceman/internal/events/pulsetest"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/memory"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// SM-STORE-003…006 end to end (simulated: in-memory runner, in-process NATS for the connection): a central with a
// space, a running run with a recording, an open human action, approvals, synced role memory, an attachment and an
// event subscription is backed up through the service, then restored into a new data directory and opened again. It
// comes up paused with new execution authority: the lease generation jumps, the old run is fenced and stopped by its
// node, nothing launches until the space is resumed, the subscription replays from its oldest stored delivery, the
// approval with uses left is revoked while the pending one stays, and the restored files are readable.
func TestBackupRestoreEndToEnd(t *testing.T) {
	// Not parallel: central restarts must finish within the node lease, which parallel tests can starve.
	git, gitErr := exec.LookPath("git")
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "restore", Name: "Restore", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { x, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return x.Lease != nil && x.Lease.Valid })

	// Role memory synced to central (when git is installed).
	role := "implementor-backend"
	if gitErr == nil {
		mrun, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: role, Harness: hns.KindClaude})
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "memory run running", func() bool { return e.run1(mrun.ID).Observed == "running" })
		dir := filepath.Join(e.agentDir, "memory", sp.ID, role, "instances", e.run1(mrun.ID).InstanceID)
		if err := os.WriteFile(filepath.Join(dir, "decisions.md"), []byte("- keep the ledger append-only\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := e.c.Fleet.StopRun(ctx, admin, mrun.ID, "kill", "done"); err != nil {
			t.Fatal(err)
		}
		eventually(t, "memory acknowledged", func() bool { _, err := e.c.Memory.File(ctx, sp.ID, role, "decisions.md"); return err == nil })
	}

	// A run active at backup time, with a recording.
	run, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "run running", func() bool { return e.run1(run.ID).Observed == "running" })
	oldGen := e.run1(run.ID).Generation
	recDir := filepath.Join(e.cfg.DataDir, "recordings", run.ID)
	eventually(t, "recording shipped", func() bool { m, _ := filepath.Glob(filepath.Join(recDir, "seg-*")); return len(m) > 0 })

	att, err := e.c.Attachments.Upload(ctx, admin, sp.ID, "notes.txt", strings.NewReader("restore me"))
	if err != nil {
		t.Fatal(err)
	}
	ha, _, err := e.c.Human.Create(ctx, admin, sp.ID, human.CreateInput{Type: "question", Title: "Which ledger?", Request: json.RawMessage(`{"options":["a","b"]}`),
		Requester: human.Requester{Kind: "operator", ID: admin.ID}})
	if err != nil {
		t.Fatal(err)
	}
	nats := pulsetest.Start(t)
	conn, err := e.c.Events.CreateConnection(ctx, admin, events.ConnectionInput{Name: "pulse", URL: nats.URL, Creds: string(nats.Creds("spaceman")), Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	tpls, err := e.c.Events.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var tv string
	for _, x := range tpls {
		if x.Slug == "github-issue" {
			tv = x.Versions[0].ID
		}
	}
	sub, err := e.c.Events.CreateSubscription(ctx, admin, sp.ID, events.SubscriptionInput{Name: "issues", ConnectionID: conn.ID, TemplateVersionID: tv,
		Vars: map[string]string{"account_key": "example-org", "repository": "example-org/tool"}, Start: events.Cursor{Policy: events.StartNew}})
	if err != nil {
		t.Fatal(err)
	}
	now := e.c.Store.Now().UnixMicro()
	if err := e.c.Store.W().TxNamed(ctx, "test.seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO event_deliveries (subscription_id, event_id, subject, stream_seq, outcome, at) VALUES ('` + sub.ID + `', 'e1', 's', 41, 'accepted', 1)`,
			`INSERT INTO event_deliveries (subscription_id, event_id, subject, stream_seq, outcome, at) VALUES ('` + sub.ID + `', 'e2', 's', 42, 'accepted', 2)`,
		} {
			if _, err := tx.SQL.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		for id, st := range map[string]string{"apv_restore_used": "approved", "apv_restore_wait": "pending"} {
			if _, err := tx.SQL.ExecContext(ctx, `INSERT INTO approvals (id, space_id, policy_id, operation, resource, args_digest, args_preview, requester_run,
				requester_role, state, uses_allowed, uses_left, valid_seconds, valid_until, request_expires_at, created_at, updated_at, decided_by, decided_by_type)
				VALUES (?, ?, 'pol', 'mcp.x', '', ?, '{}', ?, 'implementor', ?, 1, 1, 3600, ?, ?, ?, ?, 'usr_test', 'user')`,
				id, sp.ID, id, run.ID, st, now+3600e6, now+3600e6, now, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Back up through the service (as the API and the schedule do).
	b, err := e.c.Backups.Start(ctx, admin, backup.TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	e.c.Backups.Wait()
	st, err := e.c.Backups.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var it backup.Item
	for _, x := range st.Items {
		if x.ID == b.ID {
			it = x
		}
	}
	if it.State != backup.StateSucceeded || !it.Stored || it.Counts == nil || it.Counts.OpenHumanActions != 1 || it.Counts.PendingApprovals != 1 {
		t.Fatalf("backup: %+v", it)
	}
	if _, err := e.c.Backups.StartVerify(ctx, admin, b.ID); err != nil {
		t.Fatal(err)
	}
	e.c.Backups.Wait()

	// Restore into a new data directory and open central there (the node agent keeps running and reconnects).
	backupDir := filepath.Join(e.cfg.DataDir, "backups", b.ID)
	e.srv.CloseClientConnections()
	_ = e.c.Close()
	kek, err := keyring.LoadKEK(keyring.Source{Kind: "file", File: e.cfg.KEK.File, Production: true})
	if err != nil {
		t.Fatal(err)
	}
	var mg *memory.Git
	if gitErr == nil {
		mg = &memory.Git{Bin: git}
	}
	newDir := filepath.Join(t.TempDir(), "restored")
	if _, err := backup.Restore(ctx, backupDir, backup.RestoreOptions{DataDir: newDir, KEK: kek, Git: mg}); err != nil {
		t.Fatal(err)
	}
	e.cfg.DataDir = newDir
	e.startCentral()
	rs := e.c.Restored
	if rs == nil || rs.BackupID != b.ID || len(rs.PausedSpaces) != 1 || rs.RunsUnknown < 1 || rs.SubscriptionsReset != 1 || rs.ApprovalsRevoked != 1 ||
		rs.PendingApprovals != 1 || rs.OpenHumanActions != 1 {
		t.Fatalf("restore state: %+v", rs)
	}
	got, _ := e.c.Fleet.GetSpace(ctx, sp.ID)
	if got.State != "paused" || got.PauseMode != fleet.PauseHold {
		t.Fatalf("space after restore: %s/%s", got.State, got.PauseMode)
	}
	// New execution authority: the next grant is past the step, and the run from before is fenced and stopped.
	eventually(t, "new lease after reconnect", func() bool {
		x, _ := e.c.Fleet.GetSpace(ctx, sp.ID)
		return x.Lease != nil && x.Lease.Valid && x.Lease.Generation > oldGen+backup.LeaseGenerationStep
	})
	eventually(t, "old run stopped by its node", func() bool {
		o := e.run1(run.ID).Observed
		return o == "exited" || o == "failed" || o == "lost"
	})
	if r := e.run1(run.ID); r.Desired != "stopped" || r.Generation != oldGen {
		t.Fatalf("old run: %+v", r)
	}
	if _, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude}); err == nil {
		t.Fatal("launched in a space paused by the restore")
	}

	// Restored content is readable.
	_, rc, err := e.c.Attachments.Open(ctx, sp.ID, att.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != "restore me" {
		t.Fatalf("attachment: %q", body)
	}
	if m, _ := filepath.Glob(filepath.Join(newDir, "recordings", run.ID, "seg-*")); len(m) == 0 {
		t.Fatal("recording not restored")
	}
	if gitErr == nil {
		if c, err := e.c.Memory.File(ctx, sp.ID, role, "decisions.md"); err != nil || c != "- keep the ledger append-only\n" {
			t.Fatalf("memory after restore: %q %v", c, err)
		}
	}
	if a, err := e.c.Human.Get(ctx, sp.ID, ha.ID); err != nil || a.Status != "open" {
		t.Fatalf("human action after restore: %+v %v", a, err)
	}
	for id, want := range map[string]string{"apv_restore_used": "revoked", "apv_restore_wait": "pending"} {
		if a, err := e.c.Approvals.Get(ctx, sp.ID, id); err != nil || a.State != want {
			t.Fatalf("approval %s: %+v %v", id, a, err)
		}
	}
	// The subscription replays from its oldest stored delivery, never past it.
	s2, err := e.c.Events.GetSubscription(ctx, sp.ID, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Pending || s2.Cursor.Policy != events.StartSeq || s2.Cursor.Seq != 41 || s2.Cursor.Generation != sub.Cursor.Generation+1 {
		t.Fatalf("subscription cursor after restore: %+v", s2.Cursor)
	}

	// An operator resumes the space explicitly; launches work again under the new generation.
	if _, err := e.c.Fleet.SetSpaceState(ctx, admin, sp.ID, got.Version, "active", ""); err != nil {
		t.Fatal(err)
	}
	run2, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "run after resume", func() bool { return e.run1(run2.ID).Observed == "running" })
	if g := e.run1(run2.ID).Generation; g <= oldGen+backup.LeaseGenerationStep {
		t.Fatalf("new run generation %d", g)
	}
	// A second start applies nothing again.
	e.restartCentral()
	if e.c.Restored != nil {
		t.Fatal("the restore was applied twice")
	}
	var n int
	if err := e.c.Store.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE action = 'backup.restored'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("restore audited %d times: %v", n, err)
	}
	if _, err := backup.Restore(ctx, backupDir, backup.RestoreOptions{DataDir: newDir, KEK: kek, Git: mg}); !errors.Is(err, store.ErrLocked) {
		t.Fatalf("restore over a running central: %v", err)
	}
	_ = audit.System
}

// SM-STORE-005 over HTTP: global administrators list, create and verify backups; a space viewer cannot; every signed-in
// principal reads the restore notice; restore is not an API operation.
func TestBackupsOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	var created backup.Item
	r := root.do("POST", "/system/backups", nil)
	expect(t, r, 202)
	r.json(t, &created)
	if created.State != backup.StateRunning || !backup.ValidID(created.ID) {
		t.Fatalf("created %+v", created)
	}
	h.c.Backups.Wait()
	var st backup.Status
	root.do("GET", "/system/backups", nil).json(t, &st)
	if len(st.Items) != 1 || st.Items[0].State != backup.StateSucceeded || !st.Items[0].Stored || st.Target != "local" || len(st.Excluded) == 0 {
		t.Fatalf("status %+v", st)
	}
	expect(t, root.do("POST", "/system/backups/"+created.ID+"/verify", nil), 202)
	h.c.Backups.Wait()
	root.do("GET", "/system/backups", nil).json(t, &st)
	if st.Items[0].VerifyState != "ok" {
		t.Fatalf("verify %+v", st.Items[0])
	}
	expect(t, root.do("POST", "/system/backups/bkp_nope/verify", nil), 422)
	var notice struct{ Restored bool }
	root.do("GET", "/system/restore", nil).json(t, &notice)
	if notice.Restored {
		t.Fatal("fresh installation reports a restore")
	}
	expect(t, root.do("POST", "/system/restore/acknowledge", nil), 200)

	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	en, _ := h.c.Fleet.CreateNode(ctx, actor, fleet.CreateNodeInput{Name: "node-a"})
	sp, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	expect(t, root.do("POST", "/users", map[string]any{"email": "vic@example.com", "display_name": "Vic", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "admin", "domain": sp.ID}}}), 201)
	spaceAdmin := h.client()
	spaceAdmin.bearer = loginAndIssueToken(t, h, "vic@example.com", []map[string]string{{"role": "admin", "domain": sp.ID}})
	expect(t, spaceAdmin.do("GET", "/system/backups", nil), 403)
	expect(t, spaceAdmin.do("POST", "/system/backups", nil), 403)
	expect(t, spaceAdmin.do("POST", "/system/backups/"+created.ID+"/verify", nil), 403)
	expect(t, spaceAdmin.do("POST", "/system/restore/acknowledge", nil), 403)
	expect(t, spaceAdmin.do("GET", "/system/restore", nil), 200)
}
