package app_test

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/retention"
	"github.com/aurimasniekis/go-spaceman/internal/spaceexport"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// SM-HUI-007, SM-ATT-006, SM-SPACE-001 over HTTP: retention periods are limits with installation defaults and space
// overrides; a pass deletes finished human actions and attachments of finished items once their period passed, keeps
// what open items use, audits every deletion and records it in the change feed. A space export is a plaintext ZIP of
// the space's records and files, admins only, refused over the space's export limit.
func TestRetentionAndExportOverHTTP(t *testing.T) {
	h := newHarnessWith(t, func(c *config.Config) { c.Limits.RetentionHumanActionsDays = 30 })
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	en, _ := h.c.Fleet.CreateNode(ctx, actor, fleet.CreateNodeInput{Name: "node-a"})
	sp, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}

	// Installation defaults and the space's effective values.
	var st retention.Status
	root.do("GET", "/retention", nil).json(t, &st)
	if st.AuditDays != 0 || st.Interval != "24h0m0s" || len(st.Defaults) != 4 || st.NextAt == nil {
		t.Fatalf("status %+v", st)
	}
	var eff limits.Effective
	root.do("GET", "/spaces/"+sp.ID+"/limits", nil).json(t, &eff)
	values := map[string]limits.Limit{}
	for _, l := range eff.Limits {
		values[l.Key] = l
	}
	if l := values[limits.RetainHumanActions]; l.Value != 30 || l.Source != "installation" || l.Unit != "days" {
		t.Fatalf("installation default %+v", l)
	}
	if l := values[limits.RecordingQuota]; l.Value != proto.DefaultRecordingMaxBytes {
		t.Fatalf("recording quota %+v", l)
	}
	expect(t, root.do("PUT", "/spaces/"+sp.ID+"/limits", map[string]any{"values": map[string]int64{limits.RetainAttachments: 1, limits.RetainHumanActions: 1}}), 200)

	// Data: a finished item with an attachment, an open item with another, a resolved and an open human action.
	upload := func(name string) string {
		var a api.AttachmentOut
		r := root.upload("/spaces/"+sp.ID+"/attachments", name, []byte("content of "+name))
		expect(t, r, 201)
		r.json(t, &a)
		return a.ID
	}
	attDone, attOpen := upload("done.txt"), upload("open.txt")
	done, _, err := h.c.Inbox.Create(ctx, actor, sp.ID, inbox.CreateInput{SourceKind: "operator", Title: "Done", ContentType: inbox.Markdown, Body: "x",
		AttachmentIDs: []string{attDone}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.Inbox.SetStatus(ctx, actor, sp.ID, done.ID, done.Version, inbox.StatusInput{Status: inbox.StatusDone}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.c.Inbox.Create(ctx, actor, sp.ID, inbox.CreateInput{SourceKind: "operator", Title: "Open", ContentType: inbox.Markdown, Body: "y",
		AttachmentIDs: []string{attOpen}}); err != nil {
		t.Fatal(err)
	}
	resolved, _, err := h.c.Human.Create(ctx, actor, sp.ID, human.CreateInput{Type: human.TypeQuestion, Title: "Which?", Requester: human.Requester{Kind: "operator", ID: "usr_test"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.c.Human.Respond(ctx, actor, sp.ID, resolved.ID, resolved.Version, human.RespondInput{Outcome: "answered", Response: human.Response{Answer: "this one"}}); err != nil {
		t.Fatal(err)
	}
	pending, _, err := h.c.Human.Create(ctx, actor, sp.ID, human.CreateInput{Type: human.TypeQuestion, Title: "Still open", Requester: human.Requester{Kind: "operator", ID: "usr_test"}})
	if err != nil {
		t.Fatal(err)
	}

	// Export: summary, then the archive.
	var sum spaceexport.Summary
	root.do("GET", "/spaces/"+sp.ID+"/export/summary", nil).json(t, &sum)
	if sum.Counts.InboxItems != 2 || sum.Counts.HumanActions != 2 || sum.Counts.Attachments != 2 || !sum.WithinLimit || sum.Encrypted || sum.Warning == "" {
		t.Fatalf("summary %+v", sum)
	}
	r := root.raw("GET", "/spaces/"+sp.ID+"/export", nil)
	expect(t, r, 200)
	if r.header.Get("Content-Type") != "application/zip" || !strings.Contains(r.header.Get("Content-Disposition"), "spaceman-export-alpha-") ||
		r.header.Get("X-Spaceman-Export-Encrypted") != "false" {
		t.Fatalf("export headers %v", r.header)
	}
	zr, err := zip.NewReader(bytes.NewReader(r.body), int64(len(r.body)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		files[f.Name] = string(b)
	}
	if files["attachments/"+attDone] != "content of done.txt" || !strings.Contains(files["human_actions.jsonl"], `"answer":"this one"`) ||
		!strings.Contains(files["inbox_items.jsonl"], done.ID) || !strings.Contains(files["manifest.json"], spaceexport.Format) {
		t.Fatalf("archive files %v", files)
	}
	// Only admins export: an operator of the space gets nothing.
	expect(t, root.do("POST", "/users", map[string]any{"email": "ops@example.com", "display_name": "Ops", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "operator", "domain": sp.ID}}}), 201)
	ops := h.client()
	ops.bearer = loginAndIssueToken(t, h, "ops@example.com", []map[string]string{{"role": "operator", "domain": sp.ID}})
	expect(t, ops.raw("GET", "/spaces/"+sp.ID+"/export", nil), 404)
	expect(t, ops.do("GET", "/spaces/"+sp.ID+"/export/summary", nil), 403)
	expect(t, ops.do("POST", "/retention/run", nil), 403)
	// Operators change other limits but not retention periods (a shorter period deletes data): that needs retention:write.
	expect(t, ops.do("PUT", "/spaces/"+sp.ID+"/limits", map[string]any{"values": map[string]int64{limits.RetainAttachments: 1}}, "If-Match", `"1"`), 403)
	expect(t, ops.do("PUT", "/spaces/"+sp.ID+"/limits", map[string]any{"values": map[string]int64{limits.RetainAttachments: 1, limits.RetainHumanActions: 1,
		limits.ConcurrentRuns: 3}}, "If-Match", `"1"`), 200)
	// Over the space's export limit it is refused before anything is written.
	expect(t, root.do("PUT", "/spaces/"+sp.ID+"/limits", map[string]any{"values": map[string]int64{limits.RetainAttachments: 1, limits.RetainHumanActions: 1,
		limits.ExportMax: 1 << 20}}, "If-Match", `"2"`), 200)
	h.c.Store.W().TxNamed(ctx, "big", func(tx *store.Tx) error { //nolint:errcheck // checked through the summary below
		_, err := tx.SQL.ExecContext(ctx, `UPDATE inbox_items SET body = ? WHERE id = ?`, strings.Repeat("b", 2<<20), done.ID)
		return err
	})
	if p := root.raw("GET", "/spaces/"+sp.ID+"/export", nil); p.status != 409 || !strings.Contains(string(p.body), "export_max_bytes") {
		t.Fatalf("over the limit: %d %s", p.status, p.body)
	}

	// Nothing is old enough yet.
	var pass retention.Pass
	root.do("POST", "/retention/run", nil).json(t, &pass)
	if pass.State != retention.StateSucceeded || pass.Deleted[retention.KindAttachments] != 0 || pass.Deleted[retention.KindHumanActions] != 0 {
		t.Fatalf("early pass %+v", pass)
	}
	// Two days later the finished item's attachment and the resolved action go; the open ones stay.
	h.clk.Advance(49 * time.Hour)
	before := maxChangeSeq(t, h)
	root.do("POST", "/retention/run", nil).json(t, &pass)
	if pass.State != retention.StateSucceeded || pass.Deleted[retention.KindAttachments] != 1 || pass.Deleted[retention.KindHumanActions] != 1 {
		t.Fatalf("pass %+v", pass)
	}
	expect(t, root.do("GET", "/spaces/"+sp.ID+"/attachments/"+attDone, nil), 404)
	expect(t, root.do("GET", "/spaces/"+sp.ID+"/attachments/"+attOpen, nil), 200)
	expect(t, root.do("GET", "/spaces/"+sp.ID+"/human-actions/"+resolved.ID, nil), 404)
	expect(t, root.do("GET", "/spaces/"+sp.ID+"/human-actions/"+pending.ID, nil), 200)
	var deletions int
	if err := h.c.Store.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM changes WHERE seq > ? AND action = 'deleted' AND space_id = ?`, before, sp.ID).Scan(&deletions); err != nil || deletions != 2 {
		t.Fatalf("change feed deletions %d %v", deletions, err)
	}
	var page struct{ Items []api.AuditRecord }
	root.do("GET", "/audit?action=attachment.deleted", nil).json(t, &page)
	if len(page.Items) != 1 || page.Items[0].TargetID != attDone || page.Items[0].ActorID != "retention" || page.Items[0].Details["reason"] != "retention" {
		t.Fatalf("audit %+v", page.Items)
	}
	root.do("GET", "/retention", nil).json(t, &st)
	if len(st.Passes) != 2 || st.Passes[0].ID != pass.ID {
		t.Fatalf("passes %+v", st.Passes)
	}
}

func maxChangeSeq(t *testing.T, h *harness) int64 {
	t.Helper()
	var n int64
	if err := h.c.Store.ReadDB().QueryRowContext(context.Background(), `SELECT coalesce(max(seq), 0) FROM changes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// SM-OBS-003 over HTTP: the audit log pages forward from a durable position (after=, next_after), and once retention
// pruned events past a reader's position the page carries a gap like the change feed's; the chain still verifies.
func TestAuditForwardCursorAndRetentionGap(t *testing.T) {
	h := newHarnessWith(t, func(c *config.Config) { c.Retention.AuditDays = 30 })
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	for i := range 4 {
		if _, err := h.c.Fleet.CreateNode(ctx, actor, fleet.CreateNodeInput{Name: "node-" + strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	type page struct {
		Items     []api.AuditRecord
		NextAfter *int64 `json:"next_after"`
		Gap       *api.AuditGap
		Pruned    *audit.Pruned
	}
	var p page
	root.do("GET", "/audit?after=0&limit=2", nil).json(t, &p)
	if len(p.Items) != 2 || p.Items[0].Seq != 1 || p.Items[1].Seq != 2 || p.NextAfter == nil || *p.NextAfter != 2 || p.Gap != nil {
		t.Fatalf("first page %+v", p)
	}
	var next page
	root.do("GET", "/audit?after="+strconv.FormatInt(*p.NextAfter, 10)+"&limit=200", nil).json(t, &next)
	if len(next.Items) == 0 || next.Items[0].Seq != 3 {
		t.Fatalf("resumed page %+v", next)
	}
	head := *next.NextAfter
	var empty page
	root.do("GET", "/audit?after="+strconv.FormatInt(head, 10), nil).json(t, &empty)
	if len(empty.Items) != 0 || *empty.NextAfter != head {
		t.Fatalf("caught up %+v", empty)
	}
	expect(t, root.do("GET", "/audit?after=1&cursor=abc", nil), 422)
	expect(t, root.do("GET", "/audit?after=-1", nil), 422)

	// 31 days later retention prunes everything older than 30 days (the head is always kept).
	h.clk.Advance(31 * 24 * time.Hour)
	var pass retention.Pass
	root.do("POST", "/retention/run", nil).json(t, &pass)
	if pass.Deleted[retention.KindAudit] == 0 {
		t.Fatalf("pass %+v", pass)
	}
	var gapped page
	root.do("GET", "/audit?after=2", nil).json(t, &gapped)
	if gapped.Gap == nil || gapped.Gap.After != 2 || gapped.Pruned == nil || gapped.Gap.Oldest != gapped.Pruned.Through+1 || gapped.Items[0].Seq != gapped.Gap.Oldest {
		t.Fatalf("gap %+v pruned %+v", gapped.Gap, gapped.Pruned)
	}
	var nogap page
	root.do("GET", "/audit?after="+strconv.FormatInt(gapped.Pruned.Through, 10), nil).json(t, &nogap)
	if nogap.Gap != nil {
		t.Fatalf("no gap expected at the pruned boundary: %+v", nogap.Gap)
	}
	var newest page
	root.do("GET", "/audit", nil).json(t, &newest)
	if newest.Pruned == nil || newest.Items[0].Action != "retention.finished" {
		t.Fatalf("newest first %+v", newest.Items[0])
	}
	var res audit.VerifyResult
	root.do("POST", "/audit/verify", nil).json(t, &res)
	if !res.OK || res.PrunedThrough != gapped.Pruned.Through {
		t.Fatalf("verify %+v", res)
	}
}

// SM-REC-003 with a real agent: the space's recording quota reaches the node at launch, and recording retention deletes
// central's copy of an ended run's recording (audited, in the change feed), never an active run's.
func TestRecordingQuotaAndRetention(t *testing.T) {
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "recorded", Name: "Recorded", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	if _, err := e.c.Limits.Set(ctx, admin, sp.ID, 0, limits.Input{Values: map[string]int64{limits.RecordingQuota: 3 << 20, limits.RetainRecordings: 1}}); err != nil {
		t.Fatal(err)
	}
	ended, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	active, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "reviewer", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "runs running", func() bool { return e.run1(ended.ID).Observed == "running" && e.run1(active.ID).Observed == "running" })
	b, err := os.ReadFile(filepath.Join(e.agentDir, "runs", ended.ID, "recording-quota"))
	if err != nil || string(b) != strconv.Itoa(3<<20) {
		t.Fatalf("recording bound on the node: %q %v", b, err)
	}
	for _, id := range []string{ended.ID, active.ID} {
		e.run.Output(e.run.LiveByLabel("spaceman.run", id)[0].ID, "output of "+id+"\r\n")
	}
	recorded := func(id string) int {
		res, err := e.c.Fleet.Recordings().Read(context.Background(), id, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(res.Records)
	}
	eventually(t, "recordings shipped", func() bool { return recorded(ended.ID) > 0 && recorded(active.ID) > 0 })
	if _, err := e.c.Fleet.StopRun(ctx, admin, ended.ID, "kill", "done"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "run ended", func() bool { return e.run1(ended.ID).EndedAt != nil })
	// Make both runs look three days old; only the ended one may lose its recording.
	if err := e.c.Store.W().TxNamed(ctx, "age", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `UPDATE runs SET ended_at = ? WHERE id = ?`, time.Now().Add(-72*time.Hour).UnixMicro(), ended.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p, err := e.c.Retention.Run(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if p.Deleted[retention.KindRecordings] != 1 || recorded(ended.ID) != 0 || recorded(active.ID) == 0 {
		t.Fatalf("pass %+v; ended %d active %d records", p.Deleted, recorded(ended.ID), recorded(active.ID))
	}
	if n := e.auditCount("recording.deleted", "success"); n != 1 {
		t.Fatalf("%d recording.deleted events", n)
	}
	// An operator cannot delete an active run's recording either.
	if err := e.c.Fleet.DeleteRecording(ctx, admin, sp.ID, active.ID, nil); err == nil {
		t.Fatal("deleted an active run's recording")
	}
}
