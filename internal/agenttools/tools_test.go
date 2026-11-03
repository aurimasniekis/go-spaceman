package agenttools

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

type fx struct {
	t    *testing.T
	ctx  context.Context
	r    *Registry
	d    Deps
	ctrl *fleet.RunPrincipal
	wrk  *fleet.RunPrincipal
	diag *fleet.RunPrincipal
}

func newFx(t *testing.T) *fx {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))
	s := storetest.Open(t, storetest.WithClock(clk))
	kr := keyringtest.Load(t, s)
	aw, ch := audit.NewWriter(kr), changefeed.New(nil)
	att := attachments.New(s, attachments.NewLocal(t.TempDir()), aw, attachments.Options{MaxBytes: 4 << 20, QuotaBytes: 64 << 20, Clock: clk})
	d := Deps{Inbox: inbox.New(s, aw, ch, att), Queue: queue.New(s, aw, ch), Attachments: att, Human: human.New(s, aw, ch, clk)}
	if err := s.W().TxNamed(context.Background(), "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		} {
			if _, err := tx.SQL.ExecContext(context.Background(), q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p := func(id, kind, role string) *fleet.RunPrincipal {
		return &fleet.RunPrincipal{RunID: id, SpaceID: "spc_a", Kind: kind, Role: role, InstanceID: "ins_" + id}
	}
	return &fx{t: t, ctx: context.Background(), r: New(d), d: d, ctrl: p("run_c", KindControl, "control"), wrk: p("run_w", KindWorker, "implementor"),
		diag: p("run_d", KindDiagnostic, "diagnostic")}
}

func (f *fx) call(p *fleet.RunPrincipal, tool string, args any, out any) error {
	f.t.Helper()
	b, _ := json.Marshal(args)
	raw, err := f.r.Dispatch(f.ctx, p, tool, b)
	if err == nil && out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			f.t.Fatal(err)
		}
	}
	return err
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

// Catalogs follow the run kind; every schema is an object (the MCP SDK requires it).
func TestCatalogs(t *testing.T) {
	f := newFx(t)
	names := func(kind string) string {
		var l []proto.AgentTool
		raw, err := f.r.Dispatch(f.ctx, &fleet.RunPrincipal{Kind: kind}, proto.ToolCatalog, nil)
		must(t, err)
		_ = json.Unmarshal(raw, &l)
		var out []string
		for _, x := range l {
			var sch map[string]any
			if json.Unmarshal(x.InputSchema, &sch) != nil || sch["type"] != "object" || x.Description == "" {
				t.Fatalf("%s: %s", x.Name, x.InputSchema)
			}
			out = append(out, x.Name)
		}
		return strings.Join(out, ",")
	}
	c, w, d := names(KindControl), names(KindWorker), names(KindDiagnostic)
	if !strings.Contains(c, "spaceman_task_create") || !strings.Contains(c, "spaceman_inbox_set_status") || !strings.Contains(c, "spaceman_queue_next") {
		t.Fatalf("control: %s", c)
	}
	if strings.Contains(w, "spaceman_task_create") || !strings.Contains(w, "spaceman_task_set_status") || !strings.Contains(w, "spaceman_inbox_add") {
		t.Fatalf("worker: %s", w)
	}
	if strings.Contains(d, "spaceman_inbox_add") || strings.Contains(d, "spaceman_task_set_status") || !strings.Contains(d, "spaceman_human_ask") {
		t.Fatalf("diagnostic: %s", d)
	}
	code(t, f.call(f.diag, "spaceman_inbox_add", map[string]any{"body": "x"}, nil), apperr.CodeForbidden)
	// SM-INV-012: every kind of space agent can ask a person directly, without going through control.
	for _, k := range allKinds {
		if !slices.Contains(strings.Split(names(k), ","), "spaceman_human_ask") {
			t.Fatalf("%s runs cannot request human attention", k)
		}
	}
}

func TestInboxAndTasks(t *testing.T) {
	f := newFx(t)
	var item inbox.Item
	must(t, f.call(f.wrk, "spaceman_inbox_add", map[string]any{"title": "Finding", "body": "details", "urgency": "high"}, &item))
	if item.SourceKind != "worker" || item.SourceLabel != "implementor (worker)" {
		t.Fatalf("item %+v", item)
	}
	var page struct {
		Items []struct{ ID, Title string }
	}
	must(t, f.call(f.wrk, "spaceman_inbox_list", map[string]any{"status": "new"}, &page))
	if len(page.Items) != 1 || page.Items[0].Title != "Finding" {
		t.Fatalf("list %+v", page)
	}
	var got inbox.Item
	must(t, f.call(f.diag, "spaceman_inbox_get", map[string]any{"item_id": item.ID}, &got))
	must(t, f.call(f.ctrl, "spaceman_inbox_set_status", map[string]any{"item_id": item.ID, "status": "deferred", "defer_hours": 2, "reason": "later"}, &got))
	if got.Status != "deferred" || got.DeferUntil == nil {
		t.Fatalf("deferred %+v", got)
	}
	code(t, f.call(f.ctrl, "spaceman_inbox_set_status", map[string]any{"item_id": item.ID, "status": "new", "version": 1}, nil), apperr.CodePrecondition)

	var task queue.Task
	must(t, f.call(f.ctrl, "spaceman_task_create", map[string]any{"title": "Do it", "ready": true, "priority": "high"}, &task))
	var tasks []queue.Task
	must(t, f.call(f.wrk, "spaceman_tasks_list", nil, &tasks))
	if len(tasks) != 1 {
		t.Fatalf("tasks %+v", tasks)
	}
	var sel queue.Selection
	must(t, f.call(f.ctrl, "spaceman_queue_next", nil, &sel))
	if sel.Selected == nil || sel.Selected.ID != task.ID {
		t.Fatalf("next %+v", sel)
	}
	code(t, f.call(f.wrk, "spaceman_task_set_status", map[string]any{"task_id": task.ID, "status": "review"}, nil), apperr.CodeForbidden)
	// Assigned to the worker's run: it may mark review or blocked, nothing else.
	_, err := f.d.Queue.Update(f.ctx, audit.System, "spc_a", task.ID, task.Version, queue.UpdateInput{Title: task.Title, Priority: task.Priority, AssigneeRun: f.wrk.RunID})
	must(t, err)
	must(t, f.call(f.ctrl, "spaceman_task_set_status", map[string]any{"task_id": task.ID, "status": "running"}, nil))
	code(t, f.call(f.wrk, "spaceman_task_set_status", map[string]any{"task_id": task.ID, "status": "done"}, nil), apperr.CodeForbidden)
	must(t, f.call(f.wrk, "spaceman_task_set_status", map[string]any{"task_id": task.ID, "status": "review", "result": "Fixed in PR #12"}, &task))
	if task.Status != "review" || task.Result != "Fixed in PR #12" {
		t.Fatalf("review %+v", task)
	}
	var one queue.Task
	must(t, f.call(f.wrk, "spaceman_task_get", map[string]any{"task_id": task.ID}, &one))
	code(t, f.call(f.wrk, "spaceman_task_get", map[string]any{"task_id": task.ID, "extra": 1}, nil), apperr.CodeInvalid)
	code(t, f.call(f.wrk, "spaceman_unknown", nil, nil), apperr.CodeNotFound)
}

func TestAttachmentsAndHuman(t *testing.T) {
	f := newFx(t)
	txt, err := f.d.Attachments.Upload(f.ctx, audit.System, "spc_a", "notes.txt", strings.NewReader("hello attachments"))
	must(t, err)
	bin, err := f.d.Attachments.Upload(f.ctx, audit.System, "spc_a", "pixel.png", strings.NewReader("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"))
	must(t, err)
	var out struct {
		Encoding, Content string
		Name              string
	}
	must(t, f.call(f.wrk, "spaceman_attachment_get", map[string]any{"attachment": attachments.URIScheme + txt.ID}, &out))
	if out.Encoding != "text" || out.Content != "hello attachments" || out.Name != "notes.txt" {
		t.Fatalf("text attachment %+v", out)
	}
	must(t, f.call(f.wrk, "spaceman_attachment_get", map[string]any{"attachment": bin.ID}, &out))
	if out.Encoding != "base64" {
		t.Fatalf("binary attachment %+v", out)
	}
	code(t, f.call(f.wrk, "spaceman_attachment_get", map[string]any{"attachment": "att_missing"}, nil), apperr.CodeNotFound)

	var asked struct {
		ActionID string `json:"action_id"`
		Created  bool   `json:"created"`
	}
	must(t, f.call(f.wrk, "spaceman_human_ask", map[string]any{"type": "approval", "title": "Deploy?", "request": map[string]any{"action": "deploy"},
		"expires_in_minutes": 30, "dedup_key": "deploy-1"}, &asked))
	if !asked.Created {
		t.Fatalf("ask %+v", asked)
	}
	must(t, f.call(f.wrk, "spaceman_human_ask", map[string]any{"type": "approval", "title": "Deploy?", "request": map[string]any{"action": "deploy"}, "dedup_key": "deploy-1"}, &asked))
	if asked.Created {
		t.Fatal("dedup created a second action")
	}
	must(t, f.call(f.ctrl, "spaceman_human_ask", map[string]any{"type": "todo", "title": "Check the dashboard"}, nil))
	var mine, all []human.Action
	must(t, f.call(f.wrk, "spaceman_human_list", map[string]any{"pending": true}, &mine))
	must(t, f.call(f.wrk, "spaceman_human_list", map[string]any{"all": true}, &all))
	if len(mine) != 1 || len(all) != 2 || mine[0].RequesterRun != "run_w" || mine[0].ExpiresAt == nil {
		t.Fatalf("mine %d all %d", len(mine), len(all))
	}
	var a human.Action
	must(t, f.call(f.diag, "spaceman_human_get", map[string]any{"action_id": asked.ActionID}, &a))
	code(t, f.call(f.diag, "spaceman_human_cancel", map[string]any{"action_id": asked.ActionID}, nil), apperr.CodeForbidden)
	must(t, f.call(f.ctrl, "spaceman_human_cancel", map[string]any{"action_id": asked.ActionID, "reason": "not needed"}, &a))
	if a.Status != "cancelled" {
		t.Fatalf("cancel %+v", a)
	}
	// SM-HUM-009: the requester reads the outcome; an unrelated run sees the request only.
	var mineA, theirs human.Action
	must(t, f.call(f.wrk, "spaceman_human_get", map[string]any{"action_id": asked.ActionID}, &mineA))
	must(t, f.call(f.diag, "spaceman_human_get", map[string]any{"action_id": asked.ActionID}, &theirs))
	if mineA.ResponseHidden || mineA.StatusReason != "not needed" || !theirs.ResponseHidden || theirs.StatusReason != "" {
		t.Fatalf("visibility: requester %+v, unrelated %+v", mineA, theirs)
	}
	code(t, f.call(f.wrk, "spaceman_human_ask", map[string]any{"type": "poll", "title": "x"}, nil), apperr.CodeInvalid)
}

// The agent tools guide lists every tool, so the documented grants never drift from the registry.
func TestGuideListsEveryTool(t *testing.T) {
	b, err := os.ReadFile("../../site/guide/agent-tools.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range allKinds {
		for _, x := range newFx(t).r.Catalog(kind) {
			if !strings.Contains(string(b), "`"+x.Name+"`") {
				t.Errorf("site/guide/agent-tools.md does not document %s", x.Name)
			}
		}
	}
}

// fakeUpstream stands in for the MCP gateway.
type fakeUpstream struct{ calls []string }

func (u *fakeUpstream) Catalog(context.Context, *fleet.RunPrincipal) []proto.AgentTool {
	return []proto.AgentTool{{Name: "up__echo", InputSchema: json.RawMessage(`{"type":"object"}`), Upstream: true}}
}
func (u *fakeUpstream) Owns(name string) bool { return strings.Contains(name, "__") }
func (u *fakeUpstream) Call(_ context.Context, _ *fleet.RunPrincipal, name string, _ json.RawMessage) (json.RawMessage, error) {
	u.calls = append(u.calls, name)
	return json.RawMessage(`{"content":[]}`), nil
}

// ADR-0023: gateway tools join the catalog and are routed to the gateway; built-in names never are.
func TestUpstreamRouting(t *testing.T) {
	f := newFx(t)
	up := &fakeUpstream{}
	r := New(Deps{Inbox: f.r.d.Inbox, Queue: f.r.d.Queue, Attachments: f.r.d.Attachments, Human: f.r.d.Human, Upstream: up})
	p := &fleet.RunPrincipal{Kind: KindWorker, RunID: "run_1", SpaceID: "spc_a"}
	raw, err := r.Dispatch(f.ctx, p, proto.ToolCatalog, nil)
	must(t, err)
	if !strings.Contains(string(raw), `"up__echo"`) || !strings.Contains(string(raw), `"spaceman_inbox_add"`) {
		t.Fatalf("catalog: %s", raw)
	}
	if _, err := r.Dispatch(f.ctx, p, "up__echo", nil); err != nil || len(up.calls) != 1 {
		t.Fatalf("routed: %v %v", err, up.calls)
	}
	if _, err := r.Dispatch(f.ctx, p, "nope", nil); apperr.CodeOf(err) != apperr.CodeNotFound || len(up.calls) != 1 {
		t.Fatalf("unknown tool: %v", err)
	}
}
