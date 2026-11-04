package messages

import (
	"context"
	"encoding/json"
	"fmt"
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
	// The refused items are due again at once (the next idle report retries), not after the retry interval.
	must(t, f.s.W().TxNamed(f.ctx, "q", func(tx *store.Tx) error {
		row, err := tx.Q.GetMessage(f.ctx, m.ID)
		if err == nil && (row.NoticedAt.Valid || !due(row.NoticedAt, f.clk.Now())) {
			t.Fatalf("a refused notice is not due again: %+v", row.NoticedAt)
		}
		return err
	}))
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

// SM-COMM-003: a message an earlier control saw but never acknowledged (its notice was typed, then the run ended, for
// example because its account was refused) is noticed once to the control that replaces it; a refused notice is tried
// again, and nothing is noticed twice to the same run.
func TestNotifierNewRunInheritsUnacknowledged(t *testing.T) {
	f := newFx(t)
	f.exec(`UPDATE runs SET status = 'idle' WHERE id = '` + ctrlRun + `'`)
	m, _, err := f.svc.Send(f.ctx, op, "spc_a", From{Kind: "system", Label: "SpaceMan"}, SendInput{To: "control", Kind: "notification", Subject: "New inbox item"})
	must(t, err)
	must(t, f.n.Sweep(f.ctx))
	if f.q.count(ctrlRun) != 1 {
		t.Fatalf("first control notices %d", f.q.count(ctrlRun))
	}
	noticeCmd := func() (cmd string) {
		must(t, f.s.W().TxNamed(f.ctx, "q", func(tx *store.Tx) error {
			row, err := tx.Q.GetMessage(f.ctx, m.ID)
			cmd = row.NoticeCommand
			return err
		}))
		return cmd
	}
	ack := func(cmd, result string) {
		must(t, f.s.W().TxNamed(f.ctx, "ack", func(tx *store.Tx) error { return f.n.OnCommandAck(f.ctx, tx, cmd, result) }))
	}
	ack(noticeCmd(), proto.AckAccepted)
	if got, _ := f.svc.Get(f.ctx, "spc_a", m.ID); got.State != Available {
		t.Fatalf("after acceptance %+v", got)
	}

	// The first control ends without reading it; a new one starts later and is idle at its prompt.
	f.clk.Advance(10 * time.Second)
	const ctrl2 = "run_01cccccccccccccccccccccccc"
	at := f.clk.Now().UnixMicro()
	f.exec(`UPDATE runs SET desired = 'stopped', observed = 'exited' WHERE id = '`+ctrlRun+`'`,
		fmt.Sprintf(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, status, launch_json, created_at, created_by, updated_at, started_at)
		 VALUES ('%s', 'spc_a', 'nod_1', 1, 'control', 'control', 'claude-code', 'ins_c2', 'running', 'running', 'idle', '{}', %d, 't', %d, %d)`, ctrl2, at, at, at))
	must(t, f.n.Sweep(f.ctx))
	if f.q.count(ctrl2) != 1 || !strings.Contains(f.q.notices[ctrl2][0], "1 new message") {
		t.Fatalf("new control notices %+v", f.q.notices)
	}
	// A refused notice is tried again at the next sweep.
	ack(noticeCmd(), proto.AckRejected)
	f.clk.Advance(2 * time.Minute)
	must(t, f.n.Sweep(f.ctx))
	if f.q.count(ctrl2) != 2 {
		t.Fatalf("refused notice not retried: %d", f.q.count(ctrl2))
	}
	// Once typed into this run, it is not noticed to it again, however long it stays unacknowledged.
	ack(noticeCmd(), proto.AckAccepted)
	f.clk.Advance(30 * time.Minute)
	must(t, f.n.Sweep(f.ctx))
	if f.q.count(ctrl2) != 2 {
		t.Fatalf("noticed twice to the same run: %d", f.q.count(ctrl2))
	}
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
	// Security review SR-L05: a run that is not a recipient cannot reply to (and so resolve) another's request.
	var opsReq Message
	must(t, call(pc, "spaceman_message_send", map[string]any{"to": "operators", "subject": "May I deploy?", "request": true}, &opsReq))
	code(t, call(pw, "spaceman_message_reply", map[string]any{"message_id": opsReq.ID, "body": "sure"}, nil), apperr.CodeForbidden)
	if got, _ := f.svc.Get(f.ctx, "spc_a", opsReq.ID); got.State == Resolved {
		t.Fatal("a non-recipient's reply resolved the request")
	}
	must(t, call(pc, "spaceman_message_resolve", map[string]any{"message_id": other.ID, "resolution": "obsolete"}, nil))
	sys, _, err := f.svc.Send(f.ctx, audit.System, "spc_a", From{Kind: "system"}, SendInput{To: "control", Kind: "notification", Subject: "New inbox item"})
	must(t, err)
	code(t, call(pc, "spaceman_message_reply", map[string]any{"message_id": sys.ID, "body": "ok"}, nil), apperr.CodeInvalid)
}

// SM-HUM-009: a human response reaches only its target: the run it is routed to, or the space's control. Another run of
// the same role, a run of another role, and control of another space never see it, cannot acknowledge it, and get no
// notice for it. With wake off it is not typed into the terminal but can still be read.
func TestHumanResponsesReachOnlyTheirTarget(t *testing.T) {
	f := newFx(t)
	const (
		peerRun  = "run_01cccccccccccccccccccccccc"
		otherRun = "run_01dddddddddddddddddddddddd"
		farCtrl  = "run_01eeeeeeeeeeeeeeeeeeeeeeee"
	)
	f.exec(
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bbb', 'B', 'nod_1', 'active', 'b', 0, 't', 0)`,
		`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, status, launch_json, created_at, created_by, updated_at)
		 VALUES ('`+peerRun+`', 'spc_a', 'nod_1', 1, 'worker', 'implementor', 'codex', 'ins_p', 'running', 'running', 'idle', '{}', 0, 't', 0)`,
		`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, status, launch_json, created_at, created_by, updated_at)
		 VALUES ('`+otherRun+`', 'spc_a', 'nod_1', 1, 'worker', 'reviewer', 'codex', 'ins_o', 'running', 'running', 'idle', '{}', 0, 't', 0)`,
		`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, status, launch_json, created_at, created_by, updated_at)
		 VALUES ('`+farCtrl+`', 'spc_b', 'nod_1', 1, 'control', 'control', 'codex', 'ins_f', 'running', 'running', 'idle', '{}', 0, 't', 0)`,
		`UPDATE runs SET status = 'idle' WHERE id = '`+ctrlRun+`'`,
		`INSERT INTO human_actions (id, space_id, type, title, requester_kind, requester_run, requester_role, rank, status, outcome, created_at, created_by, updated_at)
		 VALUES ('hum_1', 'spc_a', 'question', 'Which?', 'agent', '`+wrkRun+`', 'implementor', 1, 'resolved', 'answered', 0, 't', 0)`,
		`INSERT INTO human_actions (id, space_id, type, title, requester_kind, requester_run, requester_role, rank, status, outcome, created_at, created_by, updated_at)
		 VALUES ('hum_2', 'spc_a', 'question', 'Gone?', 'agent', 'run_gone', 'implementor', 2, 'resolved', 'answered', 0, 't', 0)`,
		`INSERT INTO human_actions (id, space_id, type, title, requester_kind, requester_run, requester_role, rank, status, outcome, created_at, created_by, updated_at)
		 VALUES ('hum_3', 'spc_a', 'todo', 'Quiet', 'agent', '`+otherRun+`', 'reviewer', 3, 'resolved', 'completed', 0, 't', 0)`,
		`INSERT INTO human_notifications (id, action_id, space_id, target_kind, target_run, kind, created_at) VALUES ('hnt_1', 'hum_1', 'spc_a', 'run', '`+wrkRun+`', 'resolved', 0)`,
		`INSERT INTO human_notifications (id, action_id, space_id, target_kind, target_run, original_run, routed_from, route_reason, kind, created_at)
		 VALUES ('hnt_2', 'hum_2', 'spc_a', 'control', '', 'run_gone', 'run:run_gone', 'requester_ended', 'resolved', 0)`,
		`INSERT INTO human_notifications (id, action_id, space_id, target_kind, target_run, wake, kind, created_at) VALUES ('hnt_3', 'hum_3', 'spc_a', 'run', '`+otherRun+`', 0, 'resolved', 0)`,
		`INSERT INTO human_notifications (id, action_id, space_id, target_kind, target_run, kind, created_at) VALUES ('hnt_4', 'hum_1', 'spc_a', 'operators', '', 'resolved', 0)`,
	)
	ids := func(rc Recipient) (out []string) {
		in, err := f.svc.Fetch(f.ctx, rc)
		must(t, err)
		for _, h := range in.Human {
			out = append(out, h.ID)
		}
		return out
	}
	peer := Recipient{SpaceID: "spc_a", RunID: peerRun, Role: "implementor"}
	other := Recipient{SpaceID: "spc_a", RunID: otherRun, Role: "reviewer"}
	far := Recipient{SpaceID: "spc_b", RunID: farCtrl, Role: "control", Control: true}

	must(t, f.n.Sweep(f.ctx))
	if f.q.count(wrkRun) != 1 || f.q.count(ctrlRun) != 1 || f.q.count(peerRun) != 0 || f.q.count(otherRun) != 0 || f.q.count(farCtrl) != 0 {
		t.Fatalf("notices %+v", f.q.notices)
	}
	for name, c := range map[string]struct {
		rc   Recipient
		want string
	}{"requester": {wrk, "hnt_1"}, "control": {ctrl, "hnt_2"}, "same role": {peer, ""}, "quiet target": {other, "hnt_3"}, "other space control": {far, ""}} {
		if got := strings.Join(ids(c.rc), ","); got != c.want {
			t.Errorf("%s sees %q, want %q", name, got, c.want)
		}
	}
	in, _ := f.svc.Fetch(f.ctx, ctrl)
	if h := in.Human[0]; h.OriginalRun != "run_gone" || h.RouteReason != "requester_ended" || h.RequesterRole != "implementor" {
		t.Fatalf("provenance %+v", h)
	}
	code(t, f.svc.AckRun(f.ctx, op, peer, "hnt_1"), apperr.CodeNotFound)
	code(t, f.svc.AckRun(f.ctx, op, far, "hnt_2"), apperr.CodeNotFound)
	code(t, f.svc.AckRun(f.ctx, op, ctrl, "hnt_4"), apperr.CodeNotFound) // undeliverable: for people, not agents
	must(t, f.svc.AckRun(f.ctx, op, ctrl, "hnt_2"))
	must(t, f.svc.AckRun(f.ctx, op, wrk, "hnt_1"))
}
