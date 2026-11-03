package xspace

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/kb"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var op = audit.Actor{Type: audit.ActorUser, ID: "usr_op"}

const (
	ctlA = "run_01aaaaaaaaaaaaaaaaaaaaaaaa"
	ctlB = "run_01bbbbbbbbbbbbbbbbbbbbbbbb"
	ctlC = "run_01cccccccccccccccccccccccc"
	wrkA = "run_01dddddddddddddddddddddddd"
	kbC  = "run_01eeeeeeeeeeeeeeeeeeeeeeee"
)

type fx struct {
	t     *testing.T
	ctx   context.Context
	dir   string
	clk   *clock.Fake
	s     *store.Store
	svc   *Service
	kb    *kb.Service
	q     *queue.Service
	kek   []byte
	kicks int
}

func newFx(t *testing.T) *fx {
	t.Helper()
	kek, err := keyring.GenerateKEK()
	must(t, err)
	f := &fx{t: t, ctx: context.Background(), dir: filepath.Join(t.TempDir(), "data"), kek: kek, clk: clock.NewFake(time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))}
	f.open()
	run := func(id, space, kind, role string) string {
		return `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, status, launch_json, created_at, created_by, updated_at)
		 VALUES ('` + id + `', '` + space + `', 'nod_1', 1, '` + kind + `', '` + role + `', 'claude-code', 'ins_` + id[len(id)-4:] + `', 'running', 'running', 'idle', '{}', 0, 't', 0)`
	}
	f.exec(
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'alpha', 'Alpha', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bravo', 'Bravo', 'nod_1', 'active', 'b', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_c', 'charlie', 'Charlie', 'nod_1', 'active', 'c', 0, 't', 0)`,
		run(ctlA, "spc_a", "control", "control"), run(ctlB, "spc_b", "control", "control"), run(ctlC, "spc_c", "control", "control"),
		run(wrkA, "spc_a", "worker", "implementor"), run(kbC, "spc_c", "kb", "kb-support"),
		`INSERT INTO kb_repos (id, space_id, slug, name, description, repo, space_role, created_at, created_by, updated_at)
		 VALUES ('kbr_1', 'spc_c', 'payments', 'Payments KB', 'Settlement and retries', 'repos/kb', 'kb-support', 0, 't', 0)`,
		`INSERT INTO kb_repos (id, space_id, slug, name, repo, space_role, created_at, created_by, updated_at)
		 VALUES ('kbr_2', 'spc_c', 'secrets', 'Internal KB', 'repos/kb2', 'kb-support', 0, 't', 0)`,
	)
	return f
}

// open (re)opens the store on the same data directory and builds the services.
func (f *fx) open() {
	f.t.Helper()
	f.s = storetest.Open(f.t, storetest.WithClock(f.clk), storetest.WithDir(f.dir))
	w, err := keyring.NewLocalKEK(f.kek)
	must(f.t, err)
	kr, err := keyring.Load(f.ctx, f.s, w)
	must(f.t, err)
	aw, ch := audit.NewWriter(kr), changefeed.New(nil)
	in := inbox.New(f.s, aw, ch, nil)
	f.q = queue.New(f.s, aw, ch)
	f.kb = kb.New(f.s, aw, ch, nil, nil, f.clk, nil)
	f.svc = New(f.s, aw, ch, in, f.q, f.kb, f.clk, nil)
	f.svc.Kick = func() { f.kicks++ }
	f.kb.OnCrossSpace = f.svc.OnKBDone
}

func (f *fx) exec(qs ...string) {
	f.t.Helper()
	must(f.t, f.s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		for _, q := range qs {
			if _, err := tx.SQL.ExecContext(f.ctx, q); err != nil {
				return err
			}
		}
		return nil
	}))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func code(t *testing.T, err error, want apperr.Code) {
	t.Helper()
	if apperr.CodeOf(err) != want {
		t.Fatalf("got %v (%s), want %s", err, apperr.CodeOf(err), want)
	}
}

func principal(run, space, kind string) *fleet.RunPrincipal {
	role := "control"
	if kind != "control" {
		role = "implementor"
	}
	return &fleet.RunPrincipal{RunID: run, SpaceID: space, Kind: kind, Role: role, InstanceID: "ins_" + run[len(run)-4:]}
}

var (
	pa = principal(ctlA, "spc_a", "control")
	pb = principal(ctlB, "spc_b", "control")
	pc = principal(ctlC, "spc_c", "control")
)

// allow configures src → dst: the destination accepts in, the source may send out.
func (f *fx) allow(src, dst string, in Inbound, out Outbound) {
	f.t.Helper()
	f.setPeer(dst, src, in, nil)
	f.setPeer(src, dst, Inbound{}, &out)
}

// setPeer sets a space's configuration for a peer, keeping its other side when only one side is given.
func (f *fx) setPeer(space, peer string, in Inbound, out *Outbound) {
	f.t.Helper()
	cur, err := f.svc.GetPeer(f.ctx, space, peer)
	var version int64
	input := PeerInput{Inbound: in}
	if err == nil {
		version = cur.Version
		if out == nil {
			input.Outbound = cur.Outbound
		} else {
			input.Inbound = cur.Inbound
		}
	}
	if out != nil {
		input.Outbound = *out
	}
	_, err = f.svc.SavePeer(f.ctx, op, space, peer, version, input)
	must(f.t, err)
}

func (f *fx) ask(p *fleet.RunPrincipal, in RequestInput) (*Request, error) {
	r, _, err := f.svc.Create(f.ctx, p, in)
	return r, err
}

func (f *fx) messages(space string) []db.Message {
	f.t.Helper()
	l, err := f.s.R().ListSpaceMessages(f.ctx, db.ListSpaceMessagesParams{SpaceID: space, Limit: 100})
	must(f.t, err)
	return l
}

func (f *fx) messageFor(space, corr, state string) *db.Message {
	for _, m := range f.messages(space) {
		if m.CorrelationID == corr && strings.HasSuffix(m.DedupKey, ":"+state) {
			return &m
		}
	}
	return nil
}

func (f *fx) auditActions(action string) []db.AuditEvent {
	f.t.Helper()
	rows, err := f.s.R().ListAuditPage(f.ctx, db.ListAuditPageParams{Action: action, Lim: 100})
	must(f.t, err)
	return rows
}

var all = Outbound{Questions: true, Delegation: true, Knowledge: true}

// SM-XSPACE-001: discovery, questions, delegation and knowledge are separate grants, each needed on both sides; a
// space that may not discover another learns nothing about it, not even that it exists.
func TestPermissionMatrix(t *testing.T) {
	f := newFx(t)
	q := RequestInput{To: "bravo", Kind: Question, Title: "Retry budget?", Body: "What retry budget does bravo use?"}
	d := RequestInput{To: "bravo", Kind: Delegation, Title: "Rotate the key", Body: "Please rotate the shared key."}
	k := RequestInput{To: "bravo", Kind: Knowledge, Title: "Decision", Body: "Which settlement window?", KB: "payments", KBKind: "decision"}
	_, err := f.ask(pa, q)
	code(t, err, apperr.CodeNotFound) // nothing configured: invisible
	_, err = f.ask(pa, RequestInput{To: "nowhere", Kind: Question, Title: "x"})
	code(t, err, apperr.CodeNotFound)

	// Discovery alone: listed with nothing to send, and every request is forbidden (not hidden).
	f.allow("spc_a", "spc_b", Inbound{Discover: true}, all)
	ds, err := f.svc.Discover(f.ctx, "spc_a")
	must(t, err)
	if len(ds) != 1 || ds[0].Space.Slug != "bravo" || ds[0].Questions || ds[0].Delegation || ds[0].Knowledge {
		t.Fatalf("discover: %+v", ds)
	}
	for _, in := range []RequestInput{q, d} {
		_, err = f.ask(pa, in)
		code(t, err, apperr.CodeForbidden)
	}
	if other, _ := f.svc.Discover(f.ctx, "spc_c"); len(other) != 0 {
		t.Fatalf("charlie discovers bravo: %+v", other)
	}

	// Each request flag on its own, without discovery: that kind only; others look like nothing is there.
	for _, tc := range []struct {
		in   Inbound
		ok   RequestInput
		fail []RequestInput
	}{
		{Inbound{Questions: true}, q, []RequestInput{d}},
		{Inbound{Delegation: true}, d, []RequestInput{q}},
	} {
		f.setPeer("spc_b", "spc_a", tc.in, nil)
		if ds, _ := f.svc.Discover(f.ctx, "spc_a"); len(ds) != 0 {
			t.Fatalf("listed without discovery: %+v", ds)
		}
		r, err := f.ask(pa, tc.ok)
		must(t, err)
		if r.State != Pending || r.Side != Outgoing {
			t.Fatalf("request %+v", r)
		}
		ok(t)(f.svc.Cancel(f.ctx, op, "spc_a", r.ID, ""))
		for _, x := range tc.fail {
			_, err = f.ask(pa, x)
			code(t, err, apperr.CodeNotFound)
		}
		// The source's own outbound grant is needed too.
		f.setPeer("spc_a", "spc_b", Inbound{}, &Outbound{})
		_, err = f.ask(pa, tc.ok)
		code(t, err, apperr.CodeForbidden)
		f.setPeer("spc_a", "spc_b", Inbound{}, &all)
	}

	// Knowledge: for the KBs listed (empty: every enabled KB) and never for the others.
	f.setPeer("spc_c", "spc_a", Inbound{Discover: true, Knowledge: true, KBs: []string{"payments"}}, nil)
	f.setPeer("spc_a", "spc_c", Inbound{}, &Outbound{Knowledge: true})
	k.To = "charlie"
	r, err := f.ask(pa, k)
	must(t, err)
	if r.State != Accepted || r.KB != "payments" || r.KBRequestID != "" {
		t.Fatalf("knowledge request (source view) %+v", r)
	}
	secret := k
	secret.KB = "secrets"
	_, err = f.ask(pa, secret)
	code(t, err, apperr.CodeForbidden)
	cq := q
	cq.To = "charlie"
	_, err = f.ask(pa, cq)
	code(t, err, apperr.CodeForbidden) // discoverable, but questions are not granted
	ds, _ = f.svc.Discover(f.ctx, "spc_a")
	var cd *Destination
	for i := range ds {
		if ds[i].Space.Slug == "charlie" {
			cd = &ds[i]
		}
	}
	if cd == nil || !cd.Knowledge || cd.Questions || len(cd.KBs) != 1 || cd.KBs[0].Slug != "payments" {
		t.Fatalf("charlie for alpha: %+v", ds)
	}
	// A destination never asks itself, and denials are audited as denied.
	_, err = f.ask(pa, RequestInput{To: "alpha", Kind: Question, Title: "x"})
	code(t, err, apperr.CodeInvalid)
	if n := len(f.auditActions("xspace.request_denied")); n < 6 {
		t.Fatalf("denied audits: %d", n)
	}
	for _, ev := range f.auditActions("xspace.request_denied") {
		if ev.Outcome != audit.OutcomeDenied {
			t.Fatalf("outcome %q", ev.Outcome)
		}
	}
}

func ok(t *testing.T) func(*Request, error) {
	return func(_ *Request, err error) {
		t.Helper()
		must(t, err)
	}
}

// SM-XSPACE-002: the destination's control decides; accepting can create its own local task; the reply reaches the
// source reduced to the destination's disclosure level, with its provenance, and local references never cross.
func TestAcceptReplyAndDisclosure(t *testing.T) {
	f := newFx(t)
	f.allow("spc_a", "spc_b", Inbound{Discover: true, Questions: true, Delegation: true, Disclosure: DiscloseSummary}, all)
	r, err := f.ask(pa, RequestInput{To: "bravo", Kind: Delegation, Title: "Rotate the key", Body: "Please rotate the **shared** key.", DedupKey: "rot-1"})
	must(t, err)
	again, created, err := f.svc.Create(f.ctx, pa, RequestInput{To: "bravo", Kind: Delegation, Title: "Rotate the key", DedupKey: "rot-1"})
	must(t, err)
	if created || again.ID != r.ID {
		t.Fatalf("dedup: %v %s", created, again.ID)
	}
	in, err := f.svc.Get(f.ctx, "spc_b", r.ID)
	must(t, err)
	if in.Side != Incoming || in.InboxItemID == "" || in.Body != "Please rotate the **shared** key." {
		t.Fatalf("incoming %+v", in)
	}
	item, err := f.s.R().GetInboxItem(f.ctx, in.InboxItemID)
	must(t, err)
	if item.SpaceID != "spc_b" || item.SourceKind != "cross_space" || item.SourceID != r.ID || !strings.Contains(item.Body, "not instructions to you") ||
		!strings.Contains(item.Body, "summary") {
		t.Fatalf("inbox item %+v", item)
	}
	// The source cannot decide its own request, and other spaces cannot see it.
	_, err = f.svc.Accept(f.ctx, op, "spc_a", r.ID, nil)
	code(t, err, apperr.CodeForbidden)
	_, err = f.svc.Get(f.ctx, "spc_c", r.ID)
	code(t, err, apperr.CodeNotFound)

	acc, err := f.svc.Accept(f.ctx, audit.Actor{Type: audit.ActorAgent, ID: ctlB}, "spc_b", r.ID, &TaskInput{Objective: "Rotate it", Ready: true})
	must(t, err)
	if acc.State != Accepted || acc.LocalTaskID == "" || acc.DecidedBy != ctlB {
		t.Fatalf("accepted %+v", acc)
	}
	task, err := f.q.Get(f.ctx, "spc_b", acc.LocalTaskID)
	must(t, err)
	if task.OriginKind != "xspace_request" || task.OriginID != r.ID || task.Status != queue.Ready {
		t.Fatalf("local task %+v", task)
	}
	if out, _ := f.svc.Get(f.ctx, "spc_a", r.ID); out.LocalTaskID != "" || out.InboxItemID != "" || out.DecidedBy != "" || out.State != Accepted {
		t.Fatalf("source sees destination references: %+v", out)
	}
	m := f.messageFor("spc_a", r.ID, Accepted)
	if m == nil || m.ToKind != "run" || m.ToID != ctlA || m.FromKind != "system" || !strings.Contains(m.Body, "Bravo") || strings.Contains(m.Body, acc.LocalTaskID) {
		t.Fatalf("source notified of acceptance: %+v", m)
	}

	long := strings.Repeat("rotated. ", 400) // 3,600 characters
	done, err := f.svc.Reply(f.ctx, audit.Actor{Type: audit.ActorAgent, ID: ctlB}, "spc_b", r.ID, "", long)
	must(t, err)
	if done.State != Completed || !done.ReplyTruncated || len([]rune(done.Reply)) != SummaryChars || done.Disclosure != DiscloseSummary {
		t.Fatalf("summary reply: state %s truncated %v len %d", done.State, done.ReplyTruncated, len([]rune(done.Reply)))
	}
	if it, _ := f.s.R().GetInboxItem(f.ctx, in.InboxItemID); it.Status != "done" || it.LinkedID != acc.LocalTaskID {
		t.Fatalf("inbox item after reply %+v", it)
	}
	m = f.messageFor("spc_a", r.ID, Completed)
	if m == nil || strings.Contains(m.Body, "rotated.") || !strings.Contains(m.Body, "spaceman_xspace_get") {
		t.Fatalf("completion notice carries no reply text: %+v", m)
	}
	_, err = f.svc.Reply(f.ctx, op, "spc_b", r.ID, "", "again")
	code(t, err, apperr.CodePrecondition)

	// none: the outcome only (no reply text, no decline reason); detailed: the full text.
	f.setPeer("spc_b", "spc_a", Inbound{Discover: true, Questions: true, Disclosure: DiscloseNone}, nil)
	r2, err := f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "Anything internal?"})
	must(t, err)
	dec, err := f.svc.Decline(f.ctx, op, "spc_b", r2.ID, "our customer ACME-SECRET is involved")
	must(t, err)
	if dec.Reason == "" {
		t.Fatal("the destination keeps its own reason")
	}
	if out, _ := f.svc.Get(f.ctx, "spc_a", r2.ID); out.State != Declined || out.Reason != "" {
		t.Fatalf("decline reason leaked at disclosure none: %+v", out)
	}
	r3, err := f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "Budget?"})
	must(t, err)
	ok(t)(f.svc.Reply(f.ctx, op, "spc_b", r3.ID, Completed, "5 per batch"))
	if out, _ := f.svc.Get(f.ctx, "spc_a", r3.ID); out.Reply != "" || out.State != Completed {
		t.Fatalf("reply leaked at disclosure none: %+v", out)
	}
	f.setPeer("spc_b", "spc_a", Inbound{Discover: true, Questions: true, Disclosure: DiscloseDetailed}, nil)
	r4, err := f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "Budget?"})
	must(t, err)
	ok(t)(f.svc.Reply(f.ctx, op, "spc_b", r4.ID, Failed, long))
	if out, _ := f.svc.Get(f.ctx, "spc_a", r4.ID); out.Reply != strings.TrimSpace(long) || out.ReplyTruncated || out.State != Failed {
		t.Fatalf("detailed reply: %+v", out.State)
	}
	if n := len(f.auditActions("xspace.request_completed")); n != 2 {
		t.Fatalf("completed audits: %d", n)
	}
}

// SM-XSPACE-003: deadlines expire on the shared timer and both sides are told; the source cancels, and the
// destination is told while its local work stays its own; a requester that ended is routed to control.
func TestDeadlineCancelAndRouting(t *testing.T) {
	f := newFx(t)
	f.allow("spc_a", "spc_b", Inbound{Questions: true, Delegation: true}, all)
	_, err := f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "x", DeadlineMinutes: 1})
	code(t, err, apperr.CodeInvalid)
	r, err := f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "Soon?", DeadlineMinutes: 30})
	must(t, err)
	next, err := f.svc.NextDeadline(f.ctx)
	must(t, err)
	if !next.Equal(f.clk.Now().Add(30 * time.Minute)) {
		t.Fatalf("next deadline %v", next)
	}
	if n, _ := f.svc.Expire(f.ctx); n != 0 {
		t.Fatalf("expired early: %d", n)
	}
	f.clk.Advance(31 * time.Minute)
	n, err := f.svc.Expire(f.ctx)
	must(t, err)
	if n != 1 {
		t.Fatalf("expired %d", n)
	}
	if out, _ := f.svc.Get(f.ctx, "spc_a", r.ID); out.State != Expired || out.ClosedAt == nil {
		t.Fatalf("expired request %+v", out)
	}
	if f.messageFor("spc_a", r.ID, Expired) == nil {
		t.Fatal("source not told of the expiry")
	}
	if m := f.messageFor("spc_b", r.ID, Expired); m == nil || m.ToKind != "control" {
		t.Fatalf("destination not told of the expiry: %+v", m)
	}
	if next, _ := f.svc.NextDeadline(f.ctx); !next.IsZero() {
		t.Fatalf("deadline left %v", next)
	}

	d, err := f.ask(pa, RequestInput{To: "bravo", Kind: Delegation, Title: "Long job"})
	must(t, err)
	acc, err := f.svc.Accept(f.ctx, op, "spc_b", d.ID, &TaskInput{Title: "Long job for alpha"})
	must(t, err)
	_, err = f.svc.Cancel(f.ctx, op, "spc_b", d.ID, "")
	code(t, err, apperr.CodeForbidden) // the destination declines; it does not cancel
	c, err := f.svc.Cancel(f.ctx, audit.Actor{Type: audit.ActorAgent, ID: ctlA}, "spc_a", d.ID, "no longer needed")
	must(t, err)
	if c.State != Cancelled || c.Reason != "no longer needed" {
		t.Fatalf("cancelled %+v", c)
	}
	m := f.messageFor("spc_b", d.ID, Cancelled)
	if m == nil || m.ToKind != "control" || !strings.Contains(m.Body, acc.LocalTaskID) || !strings.Contains(m.Body, "your decision") {
		t.Fatalf("destination told of the cancellation: %+v", m)
	}
	if f.messageFor("spc_a", d.ID, Cancelled) != nil {
		t.Fatal("the requester was told of its own cancellation")
	}
	if task, _ := f.q.Get(f.ctx, "spc_b", acc.LocalTaskID); task.Status == "cancelled" {
		t.Fatal("the destination's task was cancelled for it")
	}

	// The requester's run ended: the outcome goes to the source's control, with provenance.
	r2, err := f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "Later?"})
	must(t, err)
	f.exec(`UPDATE runs SET observed = 'exited', desired = 'stopped' WHERE id = '` + ctlA + `'`)
	ok(t)(f.svc.Decline(f.ctx, op, "spc_b", r2.ID, ""))
	m = f.messageFor("spc_a", r2.ID, Declined)
	if m == nil || m.ToKind != "control" || !strings.Contains(m.Body, ctlA) || !strings.Contains(m.Body, "requester_ended") {
		t.Fatalf("routed to control: %+v", m)
	}
}

// SM-XSPACE-003: depth, loop protection and quotas.
func TestDepthLoopsAndQuotas(t *testing.T) {
	f := newFx(t)
	f.allow("spc_a", "spc_b", Inbound{Questions: true, Delegation: true}, all)
	f.allow("spc_b", "spc_c", Inbound{Questions: true}, all)
	f.allow("spc_c", "spc_a", Inbound{Questions: true}, all)
	f.allow("spc_c", "spc_b", Inbound{Questions: true}, all)
	root, err := f.ask(pa, RequestInput{To: "bravo", Kind: Delegation, Title: "Root"})
	must(t, err)
	// Bravo has one open incoming request: a request without caused_by inherits its chain.
	hop, err := f.ask(pb, RequestInput{To: "charlie", Kind: Question, Title: "Hop"})
	must(t, err)
	if hop.Depth != 2 || hop.CorrelationID != root.ID || hop.CausedBy != root.ID {
		t.Fatalf("hop %+v", hop)
	}
	// Charlie → Alpha and Charlie → Bravo would close the loop.
	for _, to := range []string{"alpha", "bravo"} {
		_, err = f.ask(pc, RequestInput{To: to, Kind: Question, Title: "Back", CausedBy: hop.ID})
		code(t, err, apperr.CodePrecondition)
		if !strings.Contains(err.Error(), "loop") {
			t.Fatalf("loop error %v", err)
		}
	}
	// Declaring the request independent starts a new chain (audited as such).
	ind, err := f.ask(pc, RequestInput{To: "alpha", Kind: Question, Title: "Unrelated", Independent: true})
	must(t, err)
	if ind.Depth != 1 || ind.CorrelationID != ind.ID {
		t.Fatalf("independent %+v", ind)
	}
	// Two open incoming requests: the caller must name one.
	ok(t)(f.svc.Cancel(f.ctx, op, "spc_c", ind.ID, ""))
	if _, err := f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "Second"}); err != nil {
		t.Fatal(err)
	}
	_, err = f.ask(pb, RequestInput{To: "charlie", Kind: Question, Title: "Which?"})
	code(t, err, apperr.CodePrecondition)
	_, err = f.ask(pb, RequestInput{To: "charlie", Kind: Question, Title: "Bad cause", CausedBy: hop.ID})
	code(t, err, apperr.CodeInvalid) // not a request bravo received

	// Depth: charlie accepts at most depth 2, so a third hop is refused there.
	_, err = f.svc.SaveSettings(f.ctx, op, "spc_c", 0, SettingsInput{MaxDepth: 2})
	must(t, err)
	f.allow("spc_a", "spc_c", Inbound{Questions: true}, all)
	f.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_d', 'delta', 'Delta', 'nod_1', 'active', 'd', 0, 't', 0)`,
		`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, status, launch_json, created_at, created_by, updated_at)
		 VALUES ('run_01ffffffffffffffffffffffff', 'spc_d', 'nod_1', 1, 'control', 'control', 'claude-code', 'ins_ffff', 'running', 'running', 'idle', '{}', 0, 't', 0)`)
	pd := principal("run_01ffffffffffffffffffffffff", "spc_d", "control")
	f.allow("spc_d", "spc_a", Inbound{Questions: true}, all)
	f.allow("spc_d", "spc_b", Inbound{Questions: true}, all)
	d1, err := f.ask(pd, RequestInput{To: "alpha", Kind: Question, Title: "D1"})
	must(t, err)
	a2, err := f.ask(pa, RequestInput{To: "charlie", Kind: Question, Title: "A2", CausedBy: d1.ID})
	must(t, err)
	if a2.Depth != 2 {
		t.Fatalf("depth %d", a2.Depth)
	}
	// d → a → b (depth 2) → c (depth 3 > 2 at charlie).
	a3, err := f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "A3", CausedBy: d1.ID})
	must(t, err)
	_, err = f.ask(pb, RequestInput{To: "charlie", Kind: Question, Title: "Too deep", CausedBy: a3.ID})
	code(t, err, apperr.CodePrecondition)
	if !strings.Contains(err.Error(), "depth 3") {
		t.Fatalf("depth error %v", err)
	}

	// Quotas: open requests per pair, requests per pair per hour, the source's outgoing requests per hour.
	f.setPeer("spc_b", "spc_a", Inbound{Questions: true, Delegation: true, MaxPending: 4, MaxPerHour: 5}, nil)
	// Alpha → bravo has root, second and A3 open (3 of 4).
	if _, err := f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "Fourth", CausedBy: d1.ID}); err != nil {
		t.Fatal(err)
	}
	_, err = f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "Fifth", CausedBy: d1.ID})
	code(t, err, apperr.CodeRateLimited)
	f.setPeer("spc_b", "spc_a", Inbound{Questions: true, Delegation: true, MaxPending: 10, MaxPerHour: 4}, nil)
	_, err = f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "Fifth", CausedBy: d1.ID})
	code(t, err, apperr.CodeRateLimited) // 4 in the last hour
	f.clk.Advance(61 * time.Minute)
	if _, err := f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "Next hour", CausedBy: d1.ID}); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.SaveSettings(f.ctx, op, "spc_a", 0, SettingsInput{MaxOutgoingPerHour: 1})
	must(t, err)
	_, err = f.ask(pa, RequestInput{To: "charlie", Kind: Question, Title: "Over budget", CausedBy: d1.ID})
	code(t, err, apperr.CodeRateLimited)
}

// SM-XSPACE-003: a knowledge request goes to the destination KB's scheduler as a support session for the source run,
// never to its repository; the answer reaches the source within the disclosure level.
func TestKnowledgeRoutedToSupportSession(t *testing.T) {
	f := newFx(t)
	f.allow("spc_a", "spc_c", Inbound{Knowledge: true, Disclosure: DiscloseSummary}, Outbound{Knowledge: true})
	r, err := f.ask(pa, RequestInput{To: "charlie", Kind: Knowledge, Title: "Window", Body: "Which settlement window did you decide on?", KB: "payments",
		KBKind: "decision"})
	must(t, err)
	_, err = f.svc.Accept(f.ctx, op, "spc_c", r.ID, nil)
	code(t, err, apperr.CodePrecondition) // the KB answers, not control
	in, _ := f.svc.Get(f.ctx, "spc_c", r.ID)
	kq, err := f.s.R().GetKBRequest(f.ctx, in.KBRequestID)
	must(t, err)
	if kq.XspaceRequestID != r.ID || kq.SpaceID != "spc_c" || kq.Question != "Which settlement window did you decide on?" || kq.State != "queued" {
		t.Fatalf("KB request %+v", kq)
	}
	sess, err := f.s.R().GetKBSession(f.ctx, kq.SessionID)
	must(t, err)
	if sess.RequesterKey != "space:spc_a/run:"+ctlA || !strings.Contains(sess.RequesterLabel, "Alpha") || sess.SpaceID != "spc_c" {
		t.Fatalf("support session %+v", sess)
	}
	// The session answers through the KB tools; the source gets the answer reduced to a summary (no evidence).
	f.exec(`UPDATE kb_sessions SET run_id = '`+kbC+`', state = 'active' WHERE id = '`+sess.ID+`'`, `UPDATE kb_requests SET state = 'dispatched' WHERE id = '`+kq.ID+`'`)
	_, err = f.kb.Answer(f.ctx, &fleet.RunPrincipal{RunID: kbC, SpaceID: "spc_c", Kind: "kb", Role: "kb-support"}, kb.AnswerInput{RequestID: kq.ID,
		Answer: "T+2, decided in ADR-0007.", Basis: "committed", Evidence: []kb.Evidence{{Path: "decisions/ADR-0007.md", Revision: "abcdef1"}}})
	must(t, err)
	out, _ := f.svc.Get(f.ctx, "spc_a", r.ID)
	if out.State != Completed || out.Reply != "T+2, decided in ADR-0007." || out.ReplyBasis != "committed" || len(out.ReplyEvidence) != 0 {
		t.Fatalf("knowledge answer at summary: %+v", out)
	}
	if f.messageFor("spc_a", r.ID, Completed) == nil {
		t.Fatal("source not told")
	}
	if len(f.messages("spc_c")) != 0 {
		t.Fatalf("the KB's answer was sent as a message in the destination: %+v", f.messages("spc_c"))
	}

	// Detailed disclosure carries the evidence; cancelling withdraws the KB request too.
	f.setPeer("spc_c", "spc_a", Inbound{Knowledge: true, Disclosure: DiscloseDetailed}, nil)
	r2, err := f.ask(pa, RequestInput{To: "charlie", Kind: Knowledge, Title: "W2", Body: "And the cutoff?", KB: "payments", KBKind: "context"})
	must(t, err)
	in2, _ := f.svc.Get(f.ctx, "spc_c", r2.ID)
	f.exec(`UPDATE kb_requests SET state = 'dispatched' WHERE id = '` + in2.KBRequestID + `'`)
	_, err = f.kb.Answer(f.ctx, &fleet.RunPrincipal{RunID: kbC, SpaceID: "spc_c", Kind: "kb"}, kb.AnswerInput{RequestID: in2.KBRequestID, Answer: "17:00 UTC",
		Basis: "committed", Evidence: []kb.Evidence{{Path: "cutoff.md"}}})
	must(t, err)
	if out, _ := f.svc.Get(f.ctx, "spc_a", r2.ID); len(out.ReplyEvidence) != 1 || out.ReplyEvidence[0].Path != "cutoff.md" {
		t.Fatalf("detailed evidence %+v", out.ReplyEvidence)
	}
	r3, err := f.ask(pa, RequestInput{To: "charlie", Kind: Knowledge, Title: "W3", Body: "?", KB: "payments", KBKind: "context"})
	must(t, err)
	ok(t)(f.svc.Cancel(f.ctx, audit.Actor{Type: audit.ActorAgent, ID: ctlA}, "spc_a", r3.ID, ""))
	in3, _ := f.svc.Get(f.ctx, "spc_c", r3.ID)
	if kq, _ := f.s.R().GetKBRequest(f.ctx, in3.KBRequestID); kq.State != "cancelled" {
		t.Fatalf("KB request after cancel: %s", kq.State)
	}
	if f.messageFor("spc_c", r3.ID, Cancelled) != nil {
		t.Fatal("control was told about a KB request it never saw")
	}
}

// SM-MCP-003, SM-XSPACE-002: only control reaches other spaces; a worker has no cross-space tool at all.
func TestOnlyControlAsksOtherSpaces(t *testing.T) {
	f := newFx(t)
	f.allow("spc_a", "spc_b", Inbound{Discover: true, Questions: true}, all)
	reg := agenttools.New(agenttools.Deps{Extra: []func(*agenttools.Registry){Tools(f.svc)}})
	for _, x := range reg.Catalog("worker") {
		if strings.HasPrefix(x.Name, "spaceman_xspace") {
			t.Fatalf("workers see %s", x.Name)
		}
	}
	names := []string{}
	for _, x := range reg.Catalog("control") {
		if strings.HasPrefix(x.Name, "spaceman_xspace") {
			names = append(names, x.Name)
			if strings.Contains(x.Name, "request") && !strings.Contains(x.Description, "notified") {
				t.Fatalf("%s does not say the caller is notified", x.Name)
			}
		}
	}
	if len(names) != 8 {
		t.Fatalf("control tools %v", names)
	}
	args, _ := json.Marshal(map[string]any{"to": "bravo", "kind": "question", "title": "From a worker"})
	for _, kind := range []string{"worker", "kb", "dreamer", "diagnostic"} {
		_, err := reg.Dispatch(f.ctx, principal(wrkA, "spc_a", kind), "spaceman_xspace_request", args)
		code(t, err, apperr.CodeForbidden)
	}
	_, _, err := f.svc.Create(f.ctx, principal(wrkA, "spc_a", "worker"), RequestInput{To: "bravo", Kind: Question, Title: "Direct"})
	code(t, err, apperr.CodeForbidden)
	raw, err := reg.Dispatch(f.ctx, pa, "spaceman_xspace_request", args)
	must(t, err)
	if !strings.Contains(string(raw), `"state":"pending"`) {
		t.Fatalf("control request %s", raw)
	}
	n, _ := f.s.R().CountXSpaceOutgoingSince(f.ctx, db.CountXSpaceOutgoingSinceParams{SourceSpaceID: "spc_a"})
	if n != 1 {
		t.Fatalf("requests %d", n)
	}
}

// Requests, deadlines and their notifications survive a restart (the store is the only state).
func TestRestartDurability(t *testing.T) {
	f := newFx(t)
	f.allow("spc_a", "spc_b", Inbound{Questions: true}, all)
	r, err := f.ask(pa, RequestInput{To: "bravo", Kind: Question, Title: "Survive?", DeadlineMinutes: 10})
	must(t, err)
	must(t, f.s.Close())
	f.open()
	f.clk.Advance(11 * time.Minute)
	if n, err := f.svc.Expire(f.ctx); err != nil || n != 1 {
		t.Fatalf("expire after restart: %d %v", n, err)
	}
	if out, _ := f.svc.Get(f.ctx, "spc_b", r.ID); out.State != Expired || out.InboxItemID == "" {
		t.Fatalf("after restart %+v", out)
	}
}

// Configuration is versioned, validated and audited; each space edits only its own side.
func TestPeerConfiguration(t *testing.T) {
	f := newFx(t)
	_, err := f.svc.SavePeer(f.ctx, op, "spc_a", "spc_a", 0, PeerInput{})
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.SavePeer(f.ctx, op, "spc_a", "spc_zz", 0, PeerInput{})
	code(t, err, apperr.CodeNotFound)
	_, err = f.svc.SavePeer(f.ctx, op, "spc_a", "spc_b", 0, PeerInput{Inbound: Inbound{Disclosure: "everything"}})
	code(t, err, apperr.CodeInvalid)
	p, err := f.svc.SavePeer(f.ctx, op, "spc_a", "spc_b", 0, PeerInput{Inbound: Inbound{Questions: true}})
	must(t, err)
	if p.Version != 1 || p.Inbound.Disclosure != DiscloseNone || p.Inbound.MaxPerHour != DefaultMaxPerHour || p.Peer.Slug != "bravo" {
		t.Fatalf("defaults %+v", p)
	}
	_, err = f.svc.SavePeer(f.ctx, op, "spc_a", "spc_b", 0, PeerInput{})
	code(t, err, apperr.CodeAlreadyExists)
	_, err = f.svc.SavePeer(f.ctx, op, "spc_a", "spc_b", 7, PeerInput{})
	code(t, err, apperr.CodePrecondition)
	must(t, f.svc.DeletePeer(f.ctx, op, "spc_a", "spc_b"))
	code(t, f.svc.DeletePeer(f.ctx, op, "spc_a", "spc_b"), apperr.CodeNotFound)
	if len(f.auditActions("xspace.peer_created")) != 1 || len(f.auditActions("xspace.peer_deleted")) != 1 {
		t.Fatal("peer changes not audited")
	}
	st, err := f.svc.GetSettings(f.ctx, "spc_a")
	must(t, err)
	if st.MaxDepth != DefaultMaxDepth || st.Version != 0 {
		t.Fatalf("default settings %+v", st)
	}
	_, err = f.svc.SaveSettings(f.ctx, op, "spc_a", 0, SettingsInput{MaxDepth: 9})
	code(t, err, apperr.CodeInvalid)
	st, err = f.svc.SaveSettings(f.ctx, op, "spc_a", 0, SettingsInput{Profile: "Payments platform team", MaxDepth: 2})
	must(t, err)
	_, err = f.svc.SaveSettings(f.ctx, op, "spc_a", 0, SettingsInput{})
	code(t, err, apperr.CodePrecondition)
	if _, err := f.svc.SaveSettings(f.ctx, op, "spc_a", st.Version, SettingsInput{Profile: "x"}); err != nil {
		t.Fatal(err)
	}
}
