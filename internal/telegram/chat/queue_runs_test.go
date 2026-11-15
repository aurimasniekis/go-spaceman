package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

func tid(c byte) string { return "tsk_" + strings.Repeat(string(c), 26) }
func rid(c byte) string { return "run_" + strings.Repeat(string(c), 26) }
func mid(c byte) string { return "msg_" + strings.Repeat(string(c), 26) }

// qrAPI answers the queue, run and message operations for spA (and spViewer, where writes are refused), and passes
// everything else to the inbox fake.
type qrAPI struct {
	next http.Handler
	mu   sync.Mutex

	tasks []*queueTask // in the queue's order
	runs  map[string]*runInfo
	msgs  map[string]*opMessage

	calls    []string         // "METHOD path" of every write
	bodies   []map[string]any // the bodies of those writes
	hdrs     []http.Header    // and their headers
	conflict bool             // the next task write answers 412
	moves    []map[string]any

	extraMsgs, extraPending int // filler messages and waiting ones beyond the list
}

// otherMessage is a bot message that is not any confirmation (a forged callback is tapped on it).
func (e *env) say2(string) botapi.Message {
	return botapi.Message{MessageID: 987654, Chat: botapi.Chat{ID: chatID, Type: botapi.ChatPrivate}}
}

func (a *qrAPI) writes() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

func (a *qrAPI) task(id string) *queueTask {
	for _, t := range a.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (a *qrAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	if path == "/spaces" {
		// The space list also has the viewer space.
		rec := httptest.NewRecorder()
		a.next.ServeHTTP(rec, r)
		var l struct {
			Items []spaceRef `json:"items"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &l)
		l.Items = append(l.Items, spaceRef{ID: spViewer, Slug: "viewer", Name: "Viewer", State: "active"})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(l)
		return
	}
	seg := strings.Split(strings.Trim(path, "/"), "/")
	if len(seg) < 3 || seg[0] != "spaces" || (seg[2] != "tasks" && seg[2] != "runs" && seg[2] != "messages") {
		a.next.ServeHTTP(w, r)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	var body map[string]any
	if r.Method != http.MethodGet {
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		a.calls = append(a.calls, r.Method+" "+r.URL.Path)
		a.bodies = append(a.bodies, body)
		a.hdrs = append(a.hdrs, r.Header.Clone())
		if seg[1] == spViewer {
			out(403, map[string]any{"status": 403, "detail": "forbidden"})
			return
		}
	}
	switch {
	case seg[2] == "tasks" && len(seg) == 3:
		out(200, map[string]any{"tasks": a.tasks})
	case seg[2] == "tasks" && len(seg) == 4:
		if t := a.task(seg[3]); t != nil && t.SpaceID == seg[1] {
			out(200, t)
			return
		}
		out(404, map[string]any{"status": 404})
	case seg[2] == "tasks" && len(seg) == 5:
		t := a.task(seg[3])
		if t == nil {
			out(404, map[string]any{"status": 404})
			return
		}
		switch seg[4] {
		case "status", "incident":
			if a.conflict || r.Header.Get("If-Match") != fmt.Sprintf(`"%d"`, t.Version) {
				out(412, map[string]any{"status": 412, "detail": "changed"})
				return
			}
			if seg[4] == "status" {
				t.Status, _ = body["status"].(string)
			} else {
				t.IncidentProposedBy = ""
				if body["decision"] == "confirm" {
					t.Kind = "incident"
				}
			}
			t.Version++
		case "move":
			a.moves = append(a.moves, body)
			t.Version++
		case "resume":
			t.PreemptedBy = ""
			t.Version++
		}
		out(200, t)
	case seg[2] == "runs" && len(seg) == 3:
		var items []*runInfo
		for _, ru := range a.runs {
			items = append(items, ru)
		}
		out(200, map[string]any{"items": items})
	case seg[2] == "runs" && len(seg) == 4:
		if ru := a.runs[seg[3]]; ru != nil && ru.SpaceID == seg[1] {
			out(200, ru)
			return
		}
		out(404, map[string]any{"status": 404})
	case seg[2] == "runs" && len(seg) == 5 && seg[4] == "stop":
		a.runs[seg[3]].Observed = "stopping"
		out(200, a.runs[seg[3]])
	case seg[2] == "messages" && len(seg) == 3 && r.Method == http.MethodGet:
		var items []*opMessage
		var pend int
		for _, m := range a.msgs {
			items = append(items, m)
			if m.waiting() && m.SpaceID == seg[1] {
				pend++
			}
		}
		for i := 0; i < a.extraMsgs; i++ { // a long history, to cut the list
			items = append(items, &opMessage{ID: mid('q'), SpaceID: seg[1], ToKind: "control", State: "resolved"})
		}
		out(200, map[string]any{"items": items, "operators_pending": pend + a.extraPending})
	case seg[2] == "messages" && len(seg) == 3:
		out(201, map[string]any{"id": mid('z')})
	case seg[2] == "messages" && len(seg) == 4:
		if m := a.msgs[seg[3]]; m != nil && m.SpaceID == seg[1] {
			out(200, m)
			return
		}
		out(404, map[string]any{"status": 404})
	case seg[2] == "messages" && len(seg) == 5:
		m := a.msgs[seg[3]]
		if m == nil {
			out(404, map[string]any{"status": 404})
			return
		}
		switch seg[4] {
		case "ack":
			m.State = "acknowledged"
			m.Version++
		case "resolve":
			m.State = "resolved"
			m.Version++
		}
		out(200, m)
	default:
		out(404, map[string]any{"status": 404})
	}
}

func newQR(t *testing.T) (*env, *qrAPI) {
	t.Helper()
	e := newEnv(t)
	a := &qrAPI{next: e.api, runs: map[string]*runInfo{}, msgs: map[string]*opMessage{}}
	now := e.clk.Now()
	mk := func(c byte, title, status string, v int64) *queueTask {
		return &queueTask{ID: tid(c), SpaceID: spA, Title: title, Status: status, Priority: "normal", Position: int(c - '0'), Version: v,
			Objective: "do **it**", Acceptance: "it works"}
	}
	a.tasks = []*queueTask{mk('1', "Ready one", "ready", 4), mk('2', "Running two", "running", 2), mk('3', "Blocked three", "blocked", 1),
		mk('4', "Ready four", "ready", 6), mk('5', "Done five", "done", 1), mk('6', "Review six", "review", 1)}
	a.runs[rid('1')] = &runInfo{ID: rid('1'), SpaceID: spA, Kind: "worker", Role: "implementor", Desired: "running", Observed: "running", Status: "working", CreatedAt: now.Add(-2 * time.Hour)}
	a.runs[rid('2')] = &runInfo{ID: rid('2'), SpaceID: spA, Kind: "control", Desired: "running", Observed: "running", Status: "idle", CreatedAt: now.Add(-30 * time.Minute)}
	a.runs[rid('3')] = &runInfo{ID: rid('3'), SpaceID: spA, Kind: "worker", Role: "old", Desired: "stopped", Observed: "exited", CreatedAt: now.Add(-48 * time.Hour)}
	a.msgs[mid('1')] = &opMessage{ID: mid('1'), SpaceID: spA, FromKind: "run", FromLabel: "implementor (worker)", ToKind: "operators", Kind: "request", Subject: "Need a decision", Body: "which **way**?", State: "available", Version: 2}
	a.msgs[mid('2')] = &opMessage{ID: mid('2'), SpaceID: spA, FromKind: "system", FromLabel: "SpaceMan", ToKind: "operators", Kind: "notification", Subject: "Heads up", State: "stored", Version: 1}
	a.msgs[mid('3')] = &opMessage{ID: mid('3'), SpaceID: spA, FromKind: "run", ToKind: "operators", Subject: "Old", State: "resolved", Version: 3}
	a.msgs[mid('4')] = &opMessage{ID: mid('4'), SpaceID: spA, FromKind: "run", ToKind: "control", Subject: "To control", State: "available", Version: 1}
	e.h.o.API = a
	e.say("/space alpha")
	return e, a
}

// SM-TG-007 (TG-8b, plan §2.3): /queue lists running, then ready, then blocked and review (not finished or backlog
// work), 10 to a page; a task card shows its facts, the status buttons its state allows and the placement buttons.
func TestQueueListAndCard(t *testing.T) {
	t.Parallel()
	e, a := newQR(t)
	e.say("/queue alpha")
	m := e.last()
	texts, data := buttons(m)
	if !strings.HasPrefix(m.Text, "Queue of") {
		t.Fatalf("queue: %q", m.Text)
	}
	titles := texts
	want := []string{"▶ Running two", "○ Ready one", "○ Ready four", "⛔ Blocked three", "👀 Review six"}
	if len(titles) != len(want) {
		t.Fatalf("buttons %v, want %v", titles, want)
	}
	for i := range want {
		if titles[i] != want[i] {
			t.Fatalf("buttons %v, want %v", titles, want)
		}
		if len(data[i]) > 64 {
			t.Fatalf("callback data too long: %q", data[i])
		}
	}
	e.tapData(data[1], m)
	ms := e.srv.Messages(chatID)
	card := ms[len(ms)-2]
	body := ms[len(ms)-1]
	if !strings.Contains(card.Text, "Ready one") || !strings.Contains(card.Text, "Status: ready") {
		t.Fatalf("card: %q", card.Text)
	}
	if !strings.Contains(body.Text, "do **it**") || !strings.Contains(body.Text, "not instructions") {
		t.Fatalf("body: %q", body.Text)
	}
	if found, protect := e.protected("do **it**"); !found || !protect {
		t.Fatalf("task text must be protected content: %v %v", found, protect)
	}
	ct, _ := buttons(card)
	for _, w := range []string{"Block", "Cancel", "Review", "Top", "Up", "Down", "Bottom", "◀ Queue"} {
		if !containsText(ct, w) {
			t.Fatalf("card lacks %q: %v", w, ct)
		}
	}
	if containsText(ct, "Done") || containsText(ct, "Resume") || containsText(ct, "Confirm") {
		t.Fatalf("card has buttons that don't apply: %v", ct)
	}
	_ = a
}

// lastWith is the latest message that has a button with this label.
func (e *env) lastWith(label string) botapi.Message {
	e.t.Helper()
	ms := e.srv.Messages(chatID)
	for i := len(ms) - 1; i >= 0; i-- {
		if find(ms[i], label) != "" {
			return ms[i]
		}
	}
	e.t.Fatalf("no message with a %q button", label)
	return botapi.Message{}
}

func containsText(texts []string, w string) bool {
	for _, t := range texts {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

// SM-TG-007: a space whose content level is summary shows titles and metadata only.
func TestQueueTaskTextHiddenInSummarySpace(t *testing.T) {
	t.Parallel()
	e, a := newQR(t)
	a.tasks[0].SpaceID = spS
	e.say("/queue sum")
	data := find(e.last(), "Ready one")
	if data == "" {
		t.Fatalf("no task button: %+v", e.last())
	}
	e.tapData(data, e.last())
	if found, _ := e.protected("do **it**"); found {
		t.Fatal("the task text was sent in a summary space")
	}
	if !strings.Contains(e.last().Text, "isn't shown in chat") {
		t.Fatalf("last: %q", e.last().Text)
	}
}

// SM-TG-007: a status button carries the card's version as If-Match; a stale card gets a conflict and is refreshed.
func TestQueueStatusAndStaleCard(t *testing.T) {
	t.Parallel()
	e, a := newQR(t)
	e.say("/queue")
	list := e.last()
	e.tapData(find(list, "Ready one"), list)
	card := e.srv.Messages(chatID)
	card = card[len(card)-2:]
	blockData := find(card[0], "Block")
	if blockData == "" {
		t.Fatal("no Block button")
	}
	e.tapData(blockData, card[0])
	if got := a.hdrs[len(a.hdrs)-1].Get("If-Match"); got != `"4"` {
		t.Fatalf("If-Match %q", got)
	}
	if a.task(tid('1')).Status != "blocked" || e.lastAnswer() != "Done" {
		t.Fatalf("status %q, answer %q", a.task(tid('1')).Status, e.lastAnswer())
	}
	// The card edits in place to the new state.
	var edited botapi.Message
	for _, m := range e.srv.Messages(chatID) {
		if m.MessageID == card[0].MessageID {
			edited = m
		}
	}
	if !strings.Contains(edited.Text, "Status: blocked") {
		t.Fatalf("card after: %q", edited.Text)
	}
	// The old card is stale now (version 5): the same tap conflicts and the card is refreshed, nothing changes.
	n := len(a.writes())
	a.task(tid('1')).Status = "ready"
	e.tapData(blockData, card[0])
	if e.lastAnswer() != TextStale {
		t.Fatalf("stale answer %q", e.lastAnswer())
	}
	if a.task(tid('1')).Status != "ready" || len(a.writes()) != n+1 {
		t.Fatal("a stale tap changed the task")
	}
	for _, m := range e.srv.Messages(chatID) {
		if m.MessageID == card[0].MessageID && !strings.Contains(m.Text, "Status: ready") {
			t.Fatalf("stale card not refreshed: %q", m.Text)
		}
	}
}

// SM-TG-007: moves change the task's rank through move-task: to the top and bottom, and one up or down among its
// siblings; a stale card is refused with a refresh and no move.
func TestQueueMoves(t *testing.T) {
	t.Parallel()
	e, a := newQR(t)
	e.say("/queue")
	list := e.last()
	e.tapData(find(list, "Ready four"), list)
	ms := e.srv.Messages(chatID)
	card := ms[len(ms)-2]
	for _, c := range []struct{ label, key, val string }{{"Top", "to", "top"}, {"Bottom", "to", "bottom"}, {"Up", "before_id", tid('3')}} {
		// Each move bumps the version; the card is re-read from its message.
		for _, m := range e.srv.Messages(chatID) {
			if m.MessageID == card.MessageID {
				card = m
			}
		}
		e.tapData(find(card, c.label), card)
		got := a.moves[len(a.moves)-1]
		if len(got) != 1 || got[c.key] != c.val {
			t.Fatalf("%s: move body %v, want %s=%s", c.label, got, c.key, c.val)
		}
	}
	// Down one from the card of "Ready one": its next sibling in the queue's order is Running two.
	e.tapData(find(list, "Ready one"), list)
	ms = e.srv.Messages(chatID)
	c1 := ms[len(ms)-2]
	e.tapData(find(c1, "Down"), c1)
	if got := a.moves[len(a.moves)-1]; got["after_id"] != tid('2') {
		t.Fatalf("down: %v", got)
	}
	// The first task can't go up.
	n := len(a.moves)
	first := e.srv.Messages(chatID)
	for _, m := range first {
		if m.MessageID == c1.MessageID {
			c1 = m
		}
	}
	e.tapData(find(c1, "Up"), c1)
	if len(a.moves) != n || e.lastAnswer() != "It is already there." {
		t.Fatalf("up on the first task: moves %d→%d, answer %q", n, len(a.moves), e.lastAnswer())
	}
	// A stale card: the task changed after the card was shown.
	a.task(tid('4')).Version += 5
	stale := card
	e.tapData(find(stale, "Top"), stale)
	if len(a.moves) != n || e.lastAnswer() != TextStale {
		t.Fatalf("stale move: moves %d→%d, answer %q", n, len(a.moves), e.lastAnswer())
	}
}

// SM-TG-007: an incident proposal is decided with a confirmation step; the card's version is the If-Match.
func TestQueueIncidentNeedsConfirmation(t *testing.T) {
	t.Parallel()
	e, a := newQR(t)
	a.task(tid('4')).IncidentProposedBy = "subscription:x"
	e.say("/queue")
	list := e.last()
	e.tapData(find(list, "Ready four"), list)
	ms := e.srv.Messages(chatID)
	card := ms[len(ms)-2]
	ask := find(card, "Confirm incident")
	if ask == "" {
		t.Fatalf("no incident button: %+v", card.ReplyMarkup)
	}
	e.tapData(ask, card)
	if len(a.writes()) != 0 {
		t.Fatalf("asking decided already: %v", a.writes())
	}
	conf := e.last()
	if !strings.Contains(conf.Text, "Make this an incident") {
		t.Fatalf("confirmation: %q", conf.Text)
	}
	e.tapData(find(conf, "Confirm incident"), conf)
	if w := a.writes(); len(w) != 1 || !strings.HasSuffix(w[0], "/incident") || a.bodies[0]["decision"] != "confirm" {
		t.Fatalf("writes %v %v", w, a.bodies)
	}
	if a.hdrs[0].Get("If-Match") != `"6"` {
		t.Fatalf("If-Match %q", a.hdrs[0].Get("If-Match"))
	}
	if a.task(tid('4')).Kind != "incident" {
		t.Fatal("not confirmed")
	}
	// The confirmation is single use.
	e.tapData(find(conf, "Confirm incident"), conf)
	if e.lastAnswer() != TextConfirmGone {
		t.Fatalf("answer %q", e.lastAnswer())
	}
	// A confirmation whose task changed since conflicts.
	a.task(tid('3')).IncidentProposedBy = "subscription:y"
	card3 := e.taskCardMsg(a.task(tid('3')))
	e.tapData(find(card3, "Confirm incident"), card3)
	conf3 := e.last()
	a.task(tid('3')).Version++
	e.tapData(find(conf3, "Confirm incident"), conf3)
	if e.lastAnswer() != TextStale {
		t.Fatalf("answer %q", e.lastAnswer())
	}
}

// SM-TG-007: Resume is offered for a preempted task and calls resume-task.
func TestQueueResume(t *testing.T) {
	t.Parallel()
	e, a := newQR(t)
	a.task(tid('1')).PreemptedBy = tid('9')
	e.say("/queue")
	list := e.last()
	e.tapData(find(list, "Ready one"), list)
	ms := e.srv.Messages(chatID)
	card := ms[len(ms)-2]
	e.tapData(find(card, "Resume"), card)
	if w := a.writes(); len(w) != 1 || !strings.HasSuffix(w[0], "/tasks/"+tid('1')+"/resume") {
		t.Fatalf("writes %v", w)
	}
}

// SM-TG-007: there is no task creation, and a viewer link's writes are refused by the API and answered as unavailable.
func TestQueueViewerRefusedAndNoCreate(t *testing.T) {
	t.Parallel()
	e, a := newQR(t)
	a.tasks = append(a.tasks, &queueTask{ID: tid('v'), SpaceID: spViewer, Title: "Viewer task", Status: "ready", Priority: "normal", Version: 1})
	e.say("/queue viewer")
	e.say("/queue viewer")
	list := e.last()
	e.tapData(find(list, "Viewer task"), list)
	card := e.lastWith("Top")
	e.tapData(find(card, "Top"), card)
	if e.lastAnswer() != TextUnavailable {
		t.Fatalf("viewer move answered %q", e.lastAnswer())
	}
	e.tapData(find(card, "Block"), card)
	if e.lastAnswer() != TextUnavailable {
		t.Fatalf("viewer status answered %q", e.lastAnswer())
	}
	for _, w := range a.writes() {
		if strings.HasPrefix(w, "POST /api/v1/spaces/"+spViewer+"/tasks ") || w == "POST /spaces/"+spViewer+"/tasks" {
			t.Fatalf("a task was created: %s", w)
		}
	}
	e.say("/queue new thing")
	if !strings.HasPrefix(e.last().Text, "That isn't available") {
		t.Fatalf("/queue with a title: %q", e.last().Text)
	}
}

// SM-TG-007: forged or foreign IDs and malformed data are refused without calls.
func TestQueueForgedData(t *testing.T) {
	t.Parallel()
	e, a := newQR(t)
	e.say("/queue")
	list := e.last()
	sp := strings.TrimPrefix(spA, "spc_")
	for _, d := range []string{
		"qk:" + sp + ":short", "qs:" + sp + ":" + strings.Repeat("1", 26) + ":x:1", "qs:" + sp + ":" + strings.Repeat("1", 26) + ":r:0",
		"qm:" + sp + ":" + strings.Repeat("1", 26) + ":z:1", "qi:" + sp + ":" + strings.Repeat("1", 26) + ":x:1", "ql:" + sp + ":nope",
		"qk:" + strings.TrimPrefix(spD, "spc_") + ":" + strings.Repeat("1", 26), "qs", "qs:a:b",
	} {
		e.tapData(d, list)
	}
	if w := a.writes(); len(w) != 0 {
		t.Fatalf("forged data wrote: %v", w)
	}
}

// SM-TG-007 (plan §2.5): /runs shows the live runs of the space; the card has Stop, which asks first and then stops.
func TestRunsListAndStop(t *testing.T) {
	t.Parallel()
	e, a := newQR(t)
	e.say("/runs")
	list := e.last()
	if !strings.Contains(list.Text, "2 live") {
		t.Fatalf("runs: %q", list.Text)
	}
	texts, _ := buttons(list)
	if containsText(texts, "old") {
		t.Fatalf("an ended run is listed: %v", texts)
	}
	e.tapData(find(list, "implementor"), list)
	card := e.last()
	if !strings.Contains(card.Text, "Observed: running") || !strings.Contains(card.Text, "2 h ago") {
		t.Fatalf("run card: %q", card.Text)
	}
	ask := find(card, "Stop")
	e.tapData(ask, card)
	if len(a.writes()) != 0 {
		t.Fatalf("asking stopped the run: %v", a.writes())
	}
	conf := e.last()
	if !strings.Contains(conf.Text, "Stop implementor") {
		t.Fatalf("confirmation: %q", conf.Text)
	}
	e.tapData(find(conf, "Stop run"), conf)
	if w := a.writes(); len(w) != 1 || w[0] != "POST /api/v1/spaces/"+spA+"/runs/"+rid('1')+"/stop" {
		t.Fatalf("writes %v", w)
	}
	// The run already stopped: another tap says so and changes nothing.
	e.tapData(find(conf, "Stop run"), conf)
	if len(a.writes()) != 1 {
		t.Fatalf("a second stop was sent: %v", a.writes())
	}
	// A control run carries a warning.
	e.tapData(find(list, "control"), list)
	e.tapData(find(e.last(), "Stop"), e.last())
	if !strings.Contains(e.last().Text, "control run") {
		t.Fatalf("control stop: %q", e.last().Text)
	}
}

// SM-TG-007: the API enforces run:control: a viewer link's stop is refused and no launch or input exists in chat.
func TestRunsViewerCannotStop(t *testing.T) {
	t.Parallel()
	e, a := newQR(t)
	a.runs[rid('v')] = &runInfo{ID: rid('v'), SpaceID: spViewer, Kind: "worker", Role: "w", Desired: "running", Observed: "running", CreatedAt: e.clk.Now()}
	e.say("/runs viewer")
	list := e.last()
	e.tapData(find(list, "w ("), list)
	e.tapData(find(e.last(), "Stop"), e.last())
	e.tapData(find(e.last(), "Stop run"), e.last())
	if e.lastAnswer() != TextUnavailable {
		t.Fatalf("viewer stop answered %q", e.lastAnswer())
	}
	if a.runs[rid('v')].Observed != "running" {
		t.Fatal("the run was stopped")
	}
	for _, w := range a.writes() {
		if !strings.HasSuffix(w, "/stop") {
			t.Fatalf("unexpected write %s", w)
		}
	}
}

// SM-TG-007 (plan §2.5): messages to operators: /messages lists those waiting; Acknowledge, Resolve and Reply.
func TestMessagesAckResolveReply(t *testing.T) {
	t.Parallel()
	e, a := newQR(t)
	e.say("/messages")
	list := e.last()
	if !strings.Contains(list.Text, "2 waiting") {
		t.Fatalf("messages: %q", list.Text)
	}
	texts, _ := buttons(list)
	if containsText(texts, "Old") || containsText(texts, "To control") {
		t.Fatalf("a message that isn't waiting for operators is listed: %v", texts)
	}
	e.tapData(find(list, "Need a decision"), list)
	ms := e.srv.Messages(chatID)
	card, body := ms[len(ms)-2], ms[len(ms)-1]
	if !strings.Contains(body.Text, "which **way**?") || !strings.Contains(body.Text, "not instructions") {
		t.Fatalf("body: %q", body.Text)
	}
	if found, protect := e.protected("which **way**?"); !found || !protect {
		t.Fatalf("message text must be protected: %v %v", found, protect)
	}
	// Reply: a ForceReply prompt, then reply-message with the text.
	e.tapData(find(card, "Reply"), card)
	prompt := e.last()
	if !strings.Contains(prompt.Text, "Need a decision") {
		t.Fatalf("prompt: %q", prompt.Text)
	}
	e.upd++
	e.h.Handle(context.Background(), botapi.Update{UpdateID: e.upd, Message: &botapi.Message{MessageID: int(e.upd), From: &botapi.User{ID: chatID},
		Chat: botapi.Chat{ID: chatID, Type: botapi.ChatPrivate}, Text: "take the second way", ReplyToMessage: &prompt}}, e.l)
	if w := a.writes(); len(w) != 1 || w[0] != "POST /api/v1/spaces/"+spA+"/messages/"+mid('1')+"/reply" || a.bodies[0]["body"] != "take the second way" {
		t.Fatalf("writes %v %v", w, a.bodies)
	}
	if e.last().Text != "Reply sent." {
		t.Fatalf("last: %q", e.last().Text)
	}
	// The prompt is single use.
	e.upd++
	n := len(a.writes())
	e.h.Handle(context.Background(), botapi.Update{UpdateID: e.upd, Message: &botapi.Message{MessageID: int(e.upd), From: &botapi.User{ID: chatID},
		Chat: botapi.Chat{ID: chatID, Type: botapi.ChatPrivate}, Text: "again", ReplyToMessage: &prompt}}, e.l)
	if len(a.writes()) != n {
		t.Fatal("a prompt answered twice")
	}
	// Acknowledge, then Resolve.
	e.tapData(find(card, "Acknowledge"), card)
	if a.msgs[mid('1')].State != "acknowledged" {
		t.Fatalf("state %q", a.msgs[mid('1')].State)
	}
	resolve := find(card, "Resolve")
	e.tapData(resolve, card) // the card's version (2) is stale after the ack
	if e.lastAnswer() != TextStale || a.msgs[mid('1')].State != "acknowledged" {
		t.Fatalf("stale resolve: %q state %q", e.lastAnswer(), a.msgs[mid('1')].State)
	}
	// An acknowledged message keeps Resolve and Reply on its card (and no Acknowledge), and is still listed.
	ackd := e.last()
	if find(ackd, "Resolve") == "" || find(ackd, "Reply") == "" || find(ackd, "Acknowledge") != "" {
		t.Fatalf("acknowledged card: %+v", ackd.ReplyMarkup)
	}
	e.tapData(find(ackd, "Resolve"), ackd)
	if a.msgs[mid('1')].State != "resolved" {
		t.Fatalf("state %q", a.msgs[mid('1')].State)
	}
	a.msgs[mid('1')].State, a.msgs[mid('1')].Version = "available", 2
	// A system notification can be acknowledged and resolved, but not replied to.
	e.tapData(find(list, "Heads up"), list)
	sys := e.lastWith("Resolve")
	if find(sys, "Reply") != "" {
		t.Fatal("a system message offers Reply")
	}
	e.tapData(find(sys, "Resolve"), sys)
	if a.msgs[mid('2')].State != "resolved" {
		t.Fatalf("state %q", a.msgs[mid('2')].State)
	}
}

// SM-TG-007: a Telegram reply to a notifier card of a message is a reply to that message, only for the link the card
// went to.
func TestMessageReplyToNotifierCard(t *testing.T) {
	t.Parallel()
	e, a := newQR(t)
	e.h.o.ResolveMessage = func(_ context.Context, chat int64, msg int) (string, string, string, bool) {
		if chat == chatID && msg == 777 {
			return spA, mid('1'), "tgl_1", true
		}
		if chat == chatID && msg == 778 {
			return spA, mid('1'), "tgl_other", true
		}
		return "", "", "", false
	}
	say := func(replyTo int, text string) {
		e.upd++
		e.h.Handle(context.Background(), botapi.Update{UpdateID: e.upd, Message: &botapi.Message{MessageID: int(e.upd), From: &botapi.User{ID: chatID},
			Chat: botapi.Chat{ID: chatID, Type: botapi.ChatPrivate}, Text: text, ReplyToMessage: &botapi.Message{MessageID: replyTo}}}, e.l)
	}
	say(778, "not mine")
	say(999, "unknown card")
	if len(a.writes()) != 0 {
		t.Fatalf("writes %v", a.writes())
	}
	say(777, "go on")
	if w := a.writes(); len(w) != 1 || !strings.HasSuffix(w[0], "/messages/"+mid('1')+"/reply") {
		t.Fatalf("writes %v", a.writes())
	}
}

// SM-TG-007 (plan §2.5, Q13): talk to control. After /control, plain text becomes a draft with a Send to control
// button, which sends a request to control; media still goes to the inbox; /control off ends the mode.
func TestTalkToControl(t *testing.T) {
	t.Parallel()
	e, a := newQR(t)
	// Without control mode, plain text is captured into the inbox.
	e.say("ordinary note")
	if len(e.api.created) == 0 {
		t.Fatal("plain text was not captured without control mode")
	}
	captured := len(e.api.created)
	e.say("/control alpha")
	if !strings.Contains(e.last().Text, "Control mode for") {
		t.Fatalf("control: %q", e.last().Text)
	}
	e.say("how is the build?\nsecond line")
	draft := e.last()
	if !strings.Contains(draft.Text, "Send to the control run") || find(draft, "Send to control") == "" {
		t.Fatalf("draft: %q", draft.Text)
	}
	if len(e.api.created) != captured || len(a.writes()) != 0 {
		t.Fatal("the draft was sent or captured before it was confirmed")
	}
	e.tapData("cs:1", draft)
	if w := a.writes(); len(w) != 1 || w[0] != "POST /api/v1/spaces/"+spA+"/messages" {
		t.Fatalf("writes %v", w)
	}
	b := a.bodies[0]
	if b["to"] != "control" || b["subject"] != "how is the build?" || b["body"] != "how is the build?\nsecond line" || b["request"] != true {
		t.Fatalf("body %v", b)
	}
	if k := a.hdrs[0].Get("Idempotency-Key"); !strings.HasPrefix(k, "tg-control-") {
		t.Fatalf("Idempotency-Key %q", k)
	}
	// Sent: the draft message says so, and a second tap does nothing.
	var sentMsg botapi.Message
	for _, m := range e.srv.Messages(chatID) {
		if m.MessageID == draft.MessageID {
			sentMsg = m
		}
	}
	if !strings.Contains(sentMsg.Text, "Sent to control") {
		t.Fatalf("draft after send: %q", sentMsg.Text)
	}
	e.tapData("cs:1", draft)
	if len(a.writes()) != 1 {
		t.Fatalf("a second send: %v", a.writes())
	}
	// Cancel sends nothing.
	e.say("never mind this")
	d2 := e.last()
	e.tapData("cx:1", d2)
	if len(a.writes()) != 1 {
		t.Fatalf("cancel wrote: %v", a.writes())
	}
	// A photo is captured into the inbox even in control mode.
	e.upd++
	e.h.Handle(context.Background(), botapi.Update{UpdateID: e.upd, Message: &botapi.Message{MessageID: int(e.upd), From: &botapi.User{ID: chatID},
		Chat: botapi.Chat{ID: chatID, Type: botapi.ChatPrivate}, Caption: "a photo"}}, e.l)
	if !strings.Contains(e.last().Text, "inbox") && !strings.Contains(e.last().Text, "Nothing was added") {
		t.Logf("caption-only message answered %q", e.last().Text)
	}
	// Off: plain text is captured again.
	e.say("/control off")
	if e.last().Text != TextControlOff {
		t.Fatalf("off: %q", e.last().Text)
	}
	before := len(e.api.created)
	e.say("back to the inbox")
	if len(e.api.created) != before+1 {
		t.Fatal("text was not captured after /control off")
	}
}

// SM-TG-007: control mode expires, drafts expire and are bound to their link; a draft can't be sent by another link.
func TestControlModeExpiresAndDraftsAreBound(t *testing.T) {
	t.Parallel()
	e, a := newQR(t)
	e.say("/control alpha")
	e.say("first")
	draft := e.last()
	e.clk.Advance(controlModeTTL + time.Minute)
	e.tapData("cs:1", draft)
	if e.lastAnswer() != TextControlGone || len(a.writes()) != 0 {
		t.Fatalf("expired draft: %q writes %v", e.lastAnswer(), a.writes())
	}
	// The mode ended after an hour without use.
	n := len(e.api.created)
	e.say("later")
	if len(e.api.created) != n+1 {
		t.Fatal("expired control mode still took the text")
	}
	// Another link's tap on the draft finds nothing.
	e.say("/control alpha")
	e.say("mine")
	d := e.last()
	other := e.l
	other.ID = "tgl_2"
	e.upd++
	u := botapi.Update{UpdateID: e.upd, CallbackQuery: &botapi.CallbackQuery{ID: "x1", From: botapi.User{ID: chatID}, Message: &d, Data: "cs:1"}}
	e.srv.PushUpdate(u)
	e.h.Handle(context.Background(), u, other)
	if len(a.writes()) != 0 {
		t.Fatalf("another link sent the draft: %v", a.writes())
	}
}

// F5/F3: /messages counts waiting from the API's operators_pending, lists acknowledged-but-unresolved messages
// separately, and says when the history read is cut.
func TestMessagesListAcknowledgedAndCount(t *testing.T) {
	t.Parallel()
	e, a := newQR(t)
	a.msgs[mid('5')] = &opMessage{ID: mid('5'), SpaceID: spA, FromKind: "run", ToKind: "operators", Subject: "Seen already", State: "acknowledged", Version: 3}
	a.extraPending = 5 // waiting beyond what the cut list shows
	a.extraMsgs = messagesListLimit
	e.say("/messages")
	list := e.last()
	if !strings.Contains(list.Text, "(7 waiting)") || !strings.Contains(list.Text, "Acknowledged, not resolved: 1") ||
		!strings.Contains(list.Text, "200 most recent") {
		t.Fatalf("list: %q", list.Text)
	}
	texts, _ := buttons(list)
	if !containsText(texts, "⏳ Need a decision") || !containsText(texts, "✔ Seen already") || containsText(texts, "Old") {
		t.Fatalf("buttons: %v", texts)
	}
}

// F4: free text on the cards is content: shown at the full level only.
func TestCardsGateFreeTextOnContentLevel(t *testing.T) {
	t.Parallel()
	e, a := newQR(t)
	tk := *a.tasks[0]
	tk.StatusReason, tk.Waiting = "SECRET-REASON", []string{"SECRET-WAIT"}
	tk.Blockers = []queueBlocker{{Kind: "human", Note: "SECRET-NOTE"}}
	m := *a.msgs[mid('3')]
	m.Resolution = "SECRET-RES"
	for _, c := range []struct {
		sp   string
		full bool
	}{{spA, true}, {spS, false}} {
		s := spaceRef{ID: c.sp, Slug: "x", Name: "X"}
		got := e.h.taskCard(tk, s).text + "\n" + e.h.messageCard(m, s).text
		for _, w := range []string{"SECRET-REASON", "SECRET-WAIT", "SECRET-NOTE", "SECRET-RES"} {
			if strings.Contains(got, w) != c.full {
				t.Fatalf("space %s: %q present=%v, want %v in %q", c.sp, w, !c.full, c.full, got)
			}
		}
		if !strings.Contains(got, "Blocked: human") {
			t.Fatalf("blocker kind missing: %q", got)
		}
	}
}

// tapAs taps data on a message as the given link.
func (e *env) tapAs(l updates.Link, data string, on botapi.Message) {
	e.upd++
	u := botapi.Update{UpdateID: e.upd, CallbackQuery: &botapi.CallbackQuery{ID: fmt.Sprintf("cb%d", e.upd), From: botapi.User{ID: chatID}, Message: &on, Data: data}}
	e.srv.PushUpdate(u)
	e.h.Handle(context.Background(), u, l)
}

// F1/F2: a confirmation is bound to its own message, link, TTL and one use; a forged Confirm callback on any other
// message, an expired or another link's confirmation and a second tap are refused.
func TestConfirmationsAreBound(t *testing.T) {
	t.Parallel()
	type scenario struct {
		name  string
		start func(e *env, a *qrAPI) (conf botapi.Message, label string)
	}
	stop := func(e *env, a *qrAPI) (botapi.Message, string) {
		e.say("/runs")
		list := e.last()
		e.tapData(find(list, "implementor"), list)
		card := e.last()
		e.tapData(find(card, "Stop"), card)
		return e.last(), "Stop run"
	}
	inc := func(e *env, a *qrAPI) (botapi.Message, string) {
		a.task(tid('4')).IncidentProposedBy = "subscription:x"
		e.say("/queue")
		list := e.last()
		e.tapData(find(list, "Ready four"), list)
		card := e.lastWith("Confirm incident")
		e.tapData(find(card, "Confirm incident"), card)
		return e.last(), "Confirm incident"
	}
	for _, sc := range []scenario{{"stop", stop}, {"incident", inc}} {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			// A forged tap on a message that is not the confirmation.
			e, a := newQR(t)
			conf, label := sc.start(e, a)
			data := find(conf, label)
			other := e.say2("")
			e.tapData(data, other)
			if len(a.writes()) != 0 || e.lastAnswer() != TextConfirmGone {
				t.Fatalf("forged confirm: writes %v answer %q", a.writes(), e.lastAnswer())
			}
			// Another link's tap on the real confirmation is refused, and leaves it usable.
			o := e.l
			o.ID = "tgl_2"
			e.tapAs(o, data, conf)
			if len(a.writes()) != 0 || e.lastAnswer() != TextConfirmGone {
				t.Fatalf("other link: writes %v answer %q", a.writes(), e.lastAnswer())
			}
			// The link's own tap works once; the second is refused.
			e.tapData(data, conf)
			if len(a.writes()) != 1 {
				t.Fatalf("confirm: writes %v", a.writes())
			}
			e.tapData(data, conf)
			if len(a.writes()) != 1 || e.lastAnswer() != TextConfirmGone {
				t.Fatalf("second tap: writes %v answer %q", a.writes(), e.lastAnswer())
			}
			// An expired confirmation is refused.
			e2, a2 := newQR(t)
			conf2, label2 := sc.start(e2, a2)
			e2.clk.Advance(pendingTTL + time.Minute)
			e2.tapData(find(conf2, label2), conf2)
			if len(a2.writes()) != 0 || e2.lastAnswer() != TextConfirmGone {
				t.Fatalf("expired: writes %v answer %q", a2.writes(), e2.lastAnswer())
			}
		})
	}
}

// taskCardMsg sends a task's card and returns it.
func (e *env) taskCardMsg(t *queueTask) botapi.Message {
	e.h.send(context.Background(), chatID, e.h.taskCard(*t, spaceRef{ID: spA, Slug: "alpha", Name: "Alpha"}))
	return e.last()
}
