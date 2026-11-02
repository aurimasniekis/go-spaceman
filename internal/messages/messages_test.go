package messages

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var op = audit.Actor{Type: audit.ActorUser, ID: "usr_op"}

// fakeQueue records notices instead of sending them to a node.
type fakeQueue struct {
	mu      sync.Mutex
	notices map[string][]string // run → texts
	n       int
}

func (q *fakeQueue) QueueNotice(_ context.Context, _ *store.Tx, runID, text string) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.n++
	q.notices[runID] = append(q.notices[runID], text)
	return "cmd_" + runID + "_" + string(rune('a'+q.n)), nil
}

func (q *fakeQueue) KickRun(context.Context, string) {}

func (q *fakeQueue) count(run string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.notices[run])
}

type fx struct {
	t   *testing.T
	ctx context.Context
	s   *store.Store
	clk *clock.Fake
	svc *Service
	n   *Notifier
	q   *fakeQueue
}

const (
	ctrlRun = "run_01aaaaaaaaaaaaaaaaaaaaaaaa"
	wrkRun  = "run_01bbbbbbbbbbbbbbbbbbbbbbbb"
)

func newFx(t *testing.T) *fx {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))
	s := storetest.Open(t, storetest.WithClock(clk))
	kr := keyringtest.Load(t, s)
	svc := New(s, audit.NewWriter(kr), changefeed.New(nil))
	q := &fakeQueue{notices: map[string][]string{}}
	f := &fx{t: t, ctx: context.Background(), s: s, clk: clk, svc: svc, q: q}
	f.n = NewNotifier(svc, q, nil, clk)
	f.exec(
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, status, launch_json, created_at, created_by, updated_at)
		 VALUES ('`+ctrlRun+`', 'spc_a', 'nod_1', 1, 'control', 'control', 'claude-code', 'ins_c', 'running', 'running', 'working', '{}', 0, 't', 0)`,
		`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, status, launch_json, created_at, created_by, updated_at)
		 VALUES ('`+wrkRun+`', 'spc_a', 'nod_1', 1, 'worker', 'implementor', 'codex', 'ins_w', 'running', 'running', 'idle', '{}', 0, 't', 0)`,
	)
	return f
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

var (
	ctrl = Recipient{SpaceID: "spc_a", RunID: ctrlRun, Role: "control", Control: true}
	wrk  = Recipient{SpaceID: "spc_a", RunID: wrkRun, Role: "implementor"}
)

// SM-COMM-001/004: addressing, validation, dedup, replies resolve requests.
func TestSendFetchReply(t *testing.T) {
	f := newFx(t)
	opFrom := From{Kind: "operator", ID: "usr_op", Label: "Op"}
	for _, in := range []SendInput{
		{To: "everyone", Subject: "x"}, {To: "role:Bad Role", Subject: "x"}, {To: "run:nope", Subject: "x"}, {To: "control"},
		{To: "control", Subject: "x", Kind: "shout"}, {To: "control", Subject: "x", Kind: "reply"},
	} {
		_, _, err := f.svc.Send(f.ctx, op, "spc_a", opFrom, in)
		code(t, err, apperr.CodeInvalid)
	}
	_, _, err := f.svc.Send(f.ctx, op, "spc_a", opFrom, SendInput{To: "run:run_01cccccccccccccccccccccccc", Subject: "x"})
	code(t, err, apperr.CodeInvalid) // not a run of this space

	req, created, err := f.svc.Send(f.ctx, op, "spc_a", opFrom, SendInput{To: "control", Kind: "request", Subject: "Status?", CorrelationID: "c-1", DedupKey: "k"})
	must(t, err)
	again, created2, err := f.svc.Send(f.ctx, op, "spc_a", opFrom, SendInput{To: "control", Kind: "request", Subject: "Status?", DedupKey: "k"})
	must(t, err)
	if !created || created2 || again.ID != req.ID || req.State != Stored {
		t.Fatalf("dedup: %v %v %+v", created, created2, again)
	}
	_, _, err = f.svc.Send(f.ctx, op, "spc_a", From{Kind: "run", ID: wrkRun}, SendInput{To: "role:implementor", Subject: "to my own role"})
	must(t, err)

	// The worker does not see control's mail, nor its own broadcast; control sees the request.
	in, err := f.svc.Fetch(f.ctx, wrk)
	must(t, err)
	if len(in.Messages) != 0 {
		t.Fatalf("worker inbox %+v", in.Messages)
	}
	in, err = f.svc.Fetch(f.ctx, ctrl)
	must(t, err)
	if len(in.Messages) != 1 || in.Messages[0].State != Available {
		t.Fatalf("control inbox %+v", in.Messages)
	}
	got, _ := f.svc.Get(f.ctx, "spc_a", req.ID)
	if got.State != Available || got.DeliveredVia != "mcp_fetch" {
		t.Fatalf("after fetch %+v", got)
	}
	// A reply from control resolves the request and keeps the correlation.
	rep, _, err := f.svc.Send(f.ctx, op, "spc_a", From{Kind: "run", ID: ctrlRun}, SendInput{To: "operators", Kind: "reply", Subject: "Re: Status?", ReplyTo: req.ID})
	must(t, err)
	got, _ = f.svc.Get(f.ctx, "spc_a", req.ID)
	if got.State != Resolved || rep.CorrelationID != "c-1" {
		t.Fatalf("reply %+v / %+v", got, rep)
	}
	_, _, err = f.svc.Send(f.ctx, op, "spc_a", opFrom, SendInput{To: "control", Kind: "reply", Subject: "x", ReplyTo: "msg_nope"})
	code(t, err, apperr.CodeInvalid)
	n, err := f.svc.OperatorPending(f.ctx, "spc_a")
	must(t, err)
	if n != 1 {
		t.Fatalf("operators pending %d", n)
	}
	l, err := f.svc.List(f.ctx, "spc_a", 0)
	must(t, err)
	if len(l) != 3 {
		t.Fatalf("list %d", len(l))
	}
	_, err = f.svc.Get(f.ctx, "spc_b", req.ID)
	code(t, err, apperr.CodeNotFound)
}

// SM-COMM-002: acknowledgement only by a recipient, explicitly; operators acknowledge only operator mail.
func TestAcknowledge(t *testing.T) {
	f := newFx(t)
	m, _, err := f.svc.Send(f.ctx, op, "spc_a", From{Kind: "operator", ID: "usr_op"}, SendInput{To: "role:implementor", Subject: "Heads up"})
	must(t, err)
	code(t, f.svc.AckRun(f.ctx, op, ctrl, m.ID), apperr.CodeForbidden)
	must(t, f.svc.AckRun(f.ctx, op, wrk, m.ID))
	got, _ := f.svc.Get(f.ctx, "spc_a", m.ID)
	if got.State != Acknowledged || got.AcknowledgedBy != wrkRun || got.AvailableAt == nil {
		t.Fatalf("ack %+v", got)
	}
	code(t, f.svc.AckRun(f.ctx, op, wrk, m.ID), apperr.CodeConflict)
	code(t, f.svc.AckRun(f.ctx, op, wrk, "msg_nope"), apperr.CodeNotFound)
	_, err = f.svc.AckOperator(f.ctx, op, "spc_a", m.ID)
	code(t, err, apperr.CodeForbidden)
	toOps, _, err := f.svc.Send(f.ctx, op, "spc_a", From{Kind: "run", ID: wrkRun}, SendInput{To: "operators", Subject: "Need a key"})
	must(t, err)
	acked, err := f.svc.AckOperator(f.ctx, op, "spc_a", toOps.ID)
	must(t, err)
	if acked.State != Acknowledged {
		t.Fatalf("operator ack %+v", acked)
	}
	res, err := f.svc.Resolve(f.ctx, op, "spc_a", toOps.ID, "added")
	must(t, err)
	if res.State != Resolved || res.Resolution != "added" {
		t.Fatalf("resolve %+v", res)
	}
	_, err = f.svc.Resolve(f.ctx, op, "spc_a", toOps.ID, "again")
	code(t, err, apperr.CodeConflict)
	code(t, f.svc.AckRun(f.ctx, op, wrk, "hnt_nope"), apperr.CodeNotFound)
}

// SM-COMM-003: the notifier types a content-free notice into idle recipients only, throttles, retries later, and an
// accepted notice makes the items available. Human responses for the requester ride the same path.
func TestNotifier(t *testing.T) {
	f := newFx(t)
	m, _, err := f.svc.Send(f.ctx, op, "spc_a", From{Kind: "operator", ID: "usr_op"}, SendInput{To: "role:implementor", Subject: "Secret plan", Body: "do X"})
	must(t, err)
	_, _, err = f.svc.Send(f.ctx, op, "spc_a", From{Kind: "operator", ID: "usr_op"}, SendInput{To: "control", Subject: "For control"})
	must(t, err)
	f.exec(`INSERT INTO human_actions (id, space_id, type, title, requester_kind, requester_run, rank, status, outcome, created_at, created_by, updated_at)
	        VALUES ('hum_1', 'spc_a', 'question', 'Which?', 'agent', '`+wrkRun+`', 1, 'resolved', 'answered', 0, 't', 0)`,
		`INSERT INTO human_notifications (id, action_id, space_id, target_run, kind, created_at) VALUES ('hnt_1', 'hum_1', 'spc_a', '`+wrkRun+`', 'resolved', 0)`)

	must(t, f.n.Sweep(f.ctx))
	if f.q.count(wrkRun) != 1 || f.q.count(ctrlRun) != 0 { // control is working: no notice
		t.Fatalf("notices %+v", f.q.notices)
	}
	text := f.q.notices[wrkRun][0]
	if !strings.Contains(text, "1 new message and 1 human response") || strings.Contains(text, "Secret plan") || strings.Contains(text, "do X") {
		t.Fatalf("notice text %q", text)
	}
	// Throttled within a minute, and nothing is due before the retry interval.
	must(t, f.n.Sweep(f.ctx))
	f.clk.Advance(2 * time.Minute)
	must(t, f.n.Sweep(f.ctx))
	if f.q.count(wrkRun) != 1 {
		t.Fatalf("re-noticed too early: %d", f.q.count(wrkRun))
	}
	// A refused notice changes nothing; an accepted one makes the items available.
	cmd := ""
	must(t, f.s.W().TxNamed(f.ctx, "q", func(tx *store.Tx) error {
		row, err := tx.Q.GetMessage(f.ctx, m.ID)
		cmd = row.NoticeCommand
		return err
	}))
	ack := func(result string) {
		must(t, f.s.W().TxNamed(f.ctx, "ack", func(tx *store.Tx) error { return f.n.OnCommandAck(f.ctx, tx, cmd, result) }))
	}
	ack(proto.AckRejected)
	if got, _ := f.svc.Get(f.ctx, "spc_a", m.ID); got.State != Stored {
		t.Fatalf("after refusal %+v", got)
	}
	ack(proto.AckAccepted)
	if got, _ := f.svc.Get(f.ctx, "spc_a", m.ID); got.State != Available || got.DeliveredVia != "terminal_notice" {
		t.Fatalf("after acceptance %+v", got)
	}
	// After the retry interval, control (now idle) gets its notice; the worker has nothing new due.
	f.exec(`UPDATE runs SET status = 'done' WHERE id = '` + ctrlRun + `'`)
	f.clk.Advance(5 * time.Minute)
	must(t, f.n.Sweep(f.ctx))
	if f.q.count(ctrlRun) != 1 {
		t.Fatalf("control notices %d", f.q.count(ctrlRun))
	}
	// The worker reads and acknowledges the human response through its tools.
	in, err := f.svc.Fetch(f.ctx, wrk)
	must(t, err)
	if len(in.Human) != 1 || in.Human[0].Outcome != "answered" {
		t.Fatalf("human responses %+v", in.Human)
	}
	must(t, f.svc.AckRun(f.ctx, op, wrk, "hnt_1"))
	if in, _ := f.svc.Fetch(f.ctx, wrk); len(in.Human) != 0 {
		t.Fatalf("acknowledged response still listed: %+v", in.Human)
	}
	f.n.Kick()
	f.n.Kick() // never blocks
}

// The messaging tools enforce sender and recipient identity from the run credential.
func TestTools(t *testing.T) {
	f := newFx(t)
	r := agenttools.New(agenttools.Deps{Extra: []func(*agenttools.Registry){Tools(f.svc)}})
	pc := &fleet.RunPrincipal{RunID: ctrlRun, SpaceID: "spc_a", Kind: "control", Role: "control"}
	pw := &fleet.RunPrincipal{RunID: wrkRun, SpaceID: "spc_a", Kind: "worker", Role: "implementor"}
	call := func(p *fleet.RunPrincipal, tool string, args any, out any) error {
		b, _ := json.Marshal(args)
		raw, err := r.Dispatch(f.ctx, p, tool, b)
		if err == nil && out != nil {
			must(t, json.Unmarshal(raw, out))
		}
		return err
	}
	var req Message
	must(t, call(pw, "spaceman_message_send", map[string]any{"to": "control", "subject": "Can I touch the schema?", "request": true}, &req))
	if req.FromKind != "run" || req.FromID != wrkRun || req.Kind != "request" {
		t.Fatalf("sent %+v", req)
	}
	var in Inbox
	must(t, call(pc, "spaceman_messages", nil, &in))
	if len(in.Messages) != 1 {
		t.Fatalf("control inbox %+v", in)
	}
	code(t, call(pw, "spaceman_message_ack", map[string]any{"id": req.ID}, nil), apperr.CodeForbidden)
	must(t, call(pc, "spaceman_message_ack", map[string]any{"id": req.ID}, nil))
	var rep Message
	must(t, call(pc, "spaceman_message_reply", map[string]any{"message_id": req.ID, "body": "Yes, behind a flag."}, &rep))
	if rep.ToKind != "run" || rep.ToID != wrkRun {
		t.Fatalf("reply %+v", rep)
	}
	if got, _ := f.svc.Get(f.ctx, "spc_a", req.ID); got.State != Resolved {
		t.Fatalf("request not resolved: %+v", got)
	}
	must(t, call(pw, "spaceman_messages", nil, &in))
	if len(in.Messages) != 1 || in.Messages[0].Body != "Yes, behind a flag." {
		t.Fatalf("worker inbox %+v", in)
	}
	var other Message
	must(t, call(pc, "spaceman_message_send", map[string]any{"to": "operators", "subject": "FYI"}, &other))
	code(t, call(pw, "spaceman_message_resolve", map[string]any{"message_id": other.ID}, nil), apperr.CodeForbidden)
	must(t, call(pc, "spaceman_message_resolve", map[string]any{"message_id": other.ID, "resolution": "obsolete"}, nil))
	sys, _, err := f.svc.Send(f.ctx, audit.System, "spc_a", From{Kind: "system"}, SendInput{To: "control", Kind: "notification", Subject: "New inbox item"})
	must(t, err)
	code(t, call(pc, "spaceman_message_reply", map[string]any{"message_id": sys.ID, "body": "ok"}, nil), apperr.CodeInvalid)
}
