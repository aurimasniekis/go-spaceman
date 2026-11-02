package inbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var op = audit.Actor{Type: audit.ActorUser, ID: "usr_op"}

type fx struct {
	ctx context.Context
	s   *store.Store
	svc *Service
	att *attachments.Service
	clk *clock.Fake
}

func newFx(t *testing.T) *fx {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))
	s := storetest.Open(t, storetest.WithClock(clk))
	kr := keyringtest.Load(t, s)
	aw := audit.NewWriter(kr)
	att := attachments.New(s, attachments.NewLocal(t.TempDir()), aw, attachments.Options{Clock: clk})
	f := &fx{ctx: context.Background(), s: s, att: att, svc: New(s, aw, changefeed.New(nil), att), clk: clk}
	err := s.W().TxNamed(f.ctx, "test.seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bbb', 'B', 'nod_1', 'active', 'b', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_x', 'xxx', 'X', 'nod_1', 'archived', 'x', 0, 't', 0)`,
		} {
			if _, err := tx.SQL.ExecContext(f.ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func operator(body string) CreateInput {
	return CreateInput{SourceKind: "operator", SourceID: op.ID, SourceLabel: "Ada", Body: body}
}

// SM-INBOX-001/002, SM-ATT-001: operator Markdown with attachments; the title comes from the body; source fields and
// trusted metadata are stored as given by the authenticated caller.
func TestCreateAndGet(t *testing.T) {
	f := newFx(t)
	a, err := f.att.Upload(f.ctx, op, "spc_a", "trace.txt", strings.NewReader("stack trace"))
	if err != nil {
		t.Fatal(err)
	}
	in := operator("## Settlement retries fail\n\nSee spaceman://attachment/" + a.ID)
	in.AttachmentIDs = []string{a.ID}
	in.Urgency = "high"
	in.Trusted = map[string]any{"via": "web"}
	it, created, err := f.svc.Create(f.ctx, op, "spc_a", in)
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	if it.Title != "Settlement retries fail" || it.Status != StatusNew || it.Urgency != "high" || it.ContentType != Markdown ||
		len(it.Attachments) != 1 || it.Attachments[0].ID != a.ID || it.Trusted["via"] != "web" || it.SourceKind != "operator" {
		t.Fatalf("item %+v", it)
	}
	if _, err := f.svc.Get(f.ctx, "spc_b", it.ID); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("cross-space get: %v", err)
	}
}

func TestValidation(t *testing.T) {
	f := newFx(t)
	cases := map[string]CreateInput{
		"bad json":       {SourceKind: "operator", ContentType: JSON, Body: "{nope"},
		"bad type":       {SourceKind: "operator", ContentType: "text/html", Body: "<b>x</b>"},
		"bad urgency":    {SourceKind: "operator", Body: "x", Urgency: "asap"},
		"bad source":     {SourceKind: "email", Body: "x"},
		"empty":          {SourceKind: "operator"},
		"too large body": {SourceKind: "operator", Body: strings.Repeat("a", MaxBody+1)},
	}
	for name, in := range cases {
		if _, _, err := f.svc.Create(f.ctx, op, "spc_a", in); apperr.CodeOf(err) != apperr.CodeInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	it, _, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{SourceKind: "operator", ContentType: JSON, Body: `{"deploy":"v2"}`})
	if err != nil || it.Title != "JSON message" {
		t.Fatalf("json item %+v %v", it, err)
	}
	if _, _, err := f.svc.Create(f.ctx, op, "spc_x", operator("x")); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("archived space accepted an item: %v", err)
	}
	other, _ := f.att.Upload(f.ctx, op, "spc_b", "b.txt", strings.NewReader("b"))
	in := operator("x")
	in.AttachmentIDs = []string{other.ID}
	if _, _, err := f.svc.Create(f.ctx, op, "spc_a", in); apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Fatalf("another space's attachment accepted: %v", err)
	}
}

// SM-EVT-012: a duplicate delivery with the same source dedup key returns the original item and changes nothing.
func TestDedup(t *testing.T) {
	f := newFx(t)
	in := CreateInput{SourceKind: "event", SourceID: "sub_1", Title: "Issue opened", Body: "{}", ContentType: JSON, DedupKey: "msg-1"}
	first, created, err := f.svc.Create(f.ctx, audit.Actor{Type: audit.ActorSystem, ID: "ingest"}, "spc_a", in)
	if err != nil || !created {
		t.Fatal(err)
	}
	in.Title = "Changed title"
	again, created, err := f.svc.Create(f.ctx, audit.Actor{Type: audit.ActorSystem, ID: "ingest"}, "spc_a", in)
	if err != nil || created || again.ID != first.ID || again.Title != "Issue opened" {
		t.Fatalf("duplicate delivery: created=%v %+v %v", created, again, err)
	}
	// The key is per source kind and per space.
	if _, created, _ := f.svc.Create(f.ctx, op, "spc_b", in); !created {
		t.Fatal("dedup leaked across spaces")
	}
	in.SourceKind = "webhook"
	if _, created, _ := f.svc.Create(f.ctx, op, "spc_a", in); !created {
		t.Fatal("dedup leaked across source kinds")
	}
}

// SM-INBOX-003: status changes are optimistic, audited and validated; deferring needs a time.
func TestStatus(t *testing.T) {
	f := newFx(t)
	it, _, _ := f.svc.Create(f.ctx, op, "spc_a", operator("Please look at the flaky test"))
	if _, err := f.svc.SetStatus(f.ctx, op, "spc_a", it.ID, it.Version, StatusInput{Status: StatusDeferred}); apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Fatalf("defer without time: %v", err)
	}
	until := f.clk.Now().Add(48 * time.Hour)
	d, err := f.svc.SetStatus(f.ctx, op, "spc_a", it.ID, it.Version, StatusInput{Status: StatusDeferred, Reason: "after the release", DeferUntil: &until})
	if err != nil || d.Status != StatusDeferred || d.DeferUntil == nil || d.Version != it.Version+1 {
		t.Fatalf("defer: %+v %v", d, err)
	}
	if _, err := f.svc.SetStatus(f.ctx, op, "spc_a", it.ID, it.Version, StatusInput{Status: StatusDone}); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("stale version accepted: %v", err)
	}
	done, err := f.svc.SetStatus(f.ctx, op, "spc_a", it.ID, d.Version, StatusInput{Status: StatusDone, LinkedKind: "task", LinkedID: "tsk_1"})
	if err != nil || done.LinkedID != "tsk_1" || done.DeferUntil != nil {
		t.Fatalf("done: %+v %v", done, err)
	}
	rows, _ := f.s.R().ListAuditPage(f.ctx, db.ListAuditPageParams{Lim: 50})
	n := 0
	for _, r := range rows {
		if r.Action == "inbox.item_status" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("status audit events = %d, want 2", n)
	}
}

func TestListPagesAndCounts(t *testing.T) {
	f := newFx(t)
	for i := 0; i < 5; i++ {
		f.clk.Advance(time.Second)
		if _, _, err := f.svc.Create(f.ctx, op, "spc_a", operator("item "+string(rune('a'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	p1, err := f.svc.List(f.ctx, "spc_a", "", "", 2)
	if err != nil || len(p1.Items) != 2 || p1.NextCursor == "" || p1.Items[0].Title != "item e" {
		t.Fatalf("page 1 %+v %v", p1, err)
	}
	p2, _ := f.svc.List(f.ctx, "spc_a", "", p1.NextCursor, 2)
	p3, _ := f.svc.List(f.ctx, "spc_a", "", p2.NextCursor, 2)
	if len(p2.Items) != 2 || len(p3.Items) != 1 || p3.NextCursor != "" || p3.Items[0].Title != "item a" {
		t.Fatalf("pages %+v %+v", p2, p3)
	}
	if _, err := f.svc.List(f.ctx, "spc_a", "", "!!", 2); apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Fatalf("bad cursor: %v", err)
	}
	c, _ := f.svc.Counts(f.ctx, "spc_a")
	if c[StatusNew] != 5 || c[StatusDone] != 0 {
		t.Fatalf("counts %v", c)
	}
	if p, _ := f.svc.List(f.ctx, "spc_a", StatusDone, "", 10); len(p.Items) != 0 {
		t.Fatalf("status filter %+v", p)
	}
}
