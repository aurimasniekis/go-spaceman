package notify_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
)

// counts is a settable source of pending counts.
type counts struct {
	mu    sync.Mutex
	rows  []notify.SpaceCount
	reads int
}

func (c *counts) set(rows ...notify.SpaceCount) {
	c.mu.Lock()
	c.rows = rows
	c.mu.Unlock()
}

func (c *counts) read(context.Context) ([]notify.SpaceCount, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	return append([]notify.SpaceCount(nil), c.rows...), nil
}

func homeEnv(t *testing.T, nLinks int) (*env, *counts) {
	t.Helper()
	e := newEnv(t, nLinks)
	c := &counts{}
	e.pending = c.read
	e.homeWake = make(chan time.Time, 1)
	e.needsURL = "https://app.example.com/human"
	return e, c
}

// touch is a window in which a human action changed (the reader knows nothing of it, so no card is sent: the calls
// the tests count are the summary's).
func (e *env) touch(id string, v int64) {
	e.t.Helper()
	before := e.feed.reads.Load()
	e.feed.add("spc_1", notify.TypeHumanAction, id, changefeed.Updated, v)
	e.ring()
	e.endWindow()
	e.settle(before)
}

func lastText(e *env, chat int64) string {
	ms := e.fake.Messages(chat)
	if len(ms) == 0 {
		return ""
	}
	return ms[len(ms)-1].Text
}

// The summary is sent once, pinned once, and then only edited; the web_app button opens /human.
func TestHomeSummarySentPinnedOnceThenEdited(t *testing.T) {
	e, c := homeEnv(t, 1)
	e.allow(linkFor(1), "spc_2")
	c.set(notify.SpaceCount{SpaceID: "spc_1", Name: "Alpha", N: 3}, notify.SpaceCount{SpaceID: "spc_2", Name: "Beta", N: 2})
	stop := e.run(e.maps)
	defer stop()
	e.touch("hum_1", 1)
	sends := e.waitCalls("sendMessage", 1)
	e.waitCalls("pinChatMessage", 1)
	got := sends[0].Params
	if txt := lastText(e, 1001); !strings.Contains(txt, "Needs you — 5") || !strings.Contains(txt, "• Alpha — 3") || !strings.Contains(txt, "• Beta — 2") {
		t.Fatalf("summary text %q", txt)
	}
	if !strings.Contains(got["reply_markup"], "web_app") {
		t.Errorf("no web_app button in %v", got)
	}
	// A change within the window is held back until the wait is over, then edited in place.
	c.set(notify.SpaceCount{SpaceID: "spc_1", Name: "Alpha", N: 1})
	e.touch("hum_1", 2)
	time.Sleep(50 * time.Millisecond)
	if n := len(e.fake.Calls("editMessageText")); n != 0 {
		t.Fatalf("%d edits inside the 30 s window, want 0", n)
	}
	e.clk.Advance(31 * time.Second)
	e.homeWake <- e.clk.Now()
	e.waitCalls("editMessageText", 1)
	if txt := lastText(e, 1001); !strings.Contains(txt, "Needs you — 1") || strings.Contains(txt, "Beta") {
		t.Errorf("edited text %q", txt)
	}
	if n, p := len(e.fake.Calls("sendMessage")), len(e.fake.Calls("pinChatMessage")); n != 1 || p != 1 {
		t.Errorf("%d sends and %d pins, want 1 and 1", n, p)
	}
}

// Edits are at most one per 30 s per chat: several changes inside the window make one edit.
func TestHomeSummaryEditsAreDebounced(t *testing.T) {
	e, c := homeEnv(t, 1)
	c.set(notify.SpaceCount{SpaceID: "spc_1", Name: "Alpha", N: 1})
	stop := e.run(e.maps)
	defer stop()
	e.touch("hum_1", 1)
	e.waitCalls("sendMessage", 1)
	for i := int64(2); i <= 5; i++ {
		c.set(notify.SpaceCount{SpaceID: "spc_1", Name: "Alpha", N: i})
		e.touch("hum_1", i)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(e.fake.Calls("editMessageText")); n != 0 {
		t.Fatalf("%d edits before the wait is over", n)
	}
	e.clk.Advance(30 * time.Second)
	e.homeWake <- e.clk.Now()
	e.waitCalls("editMessageText", 1)
	time.Sleep(50 * time.Millisecond)
	if n := len(e.fake.Calls("editMessageText")); n != 1 {
		t.Errorf("%d edits, want 1", n)
	}
	if txt := lastText(e, 1001); !strings.Contains(txt, "Needs you — 5") {
		t.Errorf("text %q", txt)
	}
}

// A link that can't read a space (capped link), a hidden space and a muted one never appear in its summary, nor in its
// total.
func TestHomeSummaryNeverShowsWhatTheLinkCannotRead(t *testing.T) {
	e, c := homeEnv(t, 2)
	// Link 1 reads spc_1, spc_2 and spc_3 (muted); link 2 only spc_1 ("capped"); spc_4 is hidden from Telegram.
	for _, sp := range []string{"spc_2", "spc_3", "spc_4"} {
		e.allow(linkFor(1), sp)
		e.allow(linkFor(2), sp) // the capped link holds spc_1 only below
	}
	e.az.mu.Lock()
	for _, s := range []string{"telegram:" + linkFor(2).ID, "user:" + linkFor(2).UserID} {
		for _, sp := range []string{"spc_2", "spc_3", "spc_4"} {
			delete(e.az.space[s], sp)
		}
	}
	e.az.mu.Unlock()
	e.pol.hide("spc_4")
	l1 := linkFor(1)
	l1.Prefs.MutedSpaces = &[]string{"spc_3"}
	l2 := linkFor(2)
	e.links.Set([]notify.Link{l1, l2})
	c.set(notify.SpaceCount{SpaceID: "spc_1", Name: "Alpha", N: 1}, notify.SpaceCount{SpaceID: "spc_2", Name: "Beta", N: 2},
		notify.SpaceCount{SpaceID: "spc_3", Name: "Muted", N: 4}, notify.SpaceCount{SpaceID: "spc_4", Name: "Hidden", N: 8})
	stop := e.run(e.maps)
	defer stop()
	e.touch("hum_1", 1)
	e.waitCalls("sendMessage", 2)
	if txt := lastText(e, 1001); !strings.Contains(txt, "Needs you — 3") || !strings.Contains(txt, "Alpha") || !strings.Contains(txt, "Beta") ||
		strings.Contains(txt, "Muted") || strings.Contains(txt, "Hidden") {
		t.Errorf("link 1 sees %q", txt)
	}
	if txt := lastText(e, 1002); !strings.Contains(txt, "Needs you — 1") || strings.Contains(txt, "Beta") || strings.Contains(txt, "Muted") || strings.Contains(txt, "Hidden") {
		t.Errorf("the capped link sees %q", txt)
	}
}

// The summary survives a restart: its row is reloaded, so the new process edits the message and doesn't send or pin
// another one.
func TestHomeSummarySurvivesRestart(t *testing.T) {
	e, c := homeEnv(t, 1)
	c.set(notify.SpaceCount{SpaceID: "spc_1", Name: "Alpha", N: 2})
	stop := e.run(e.maps)
	e.touch("hum_1", 1)
	e.waitCalls("pinChatMessage", 1)
	time.Sleep(100 * time.Millisecond) // the row is recorded after the pin
	stop()                             // flushes the rows
	var home int
	e.maps.mu.Lock()
	for _, r := range e.maps.rows {
		if r.Kind == notify.MappingHome && r.Version == 1 && r.LinkID == "tgl_1" {
			home++
		}
	}
	e.maps.mu.Unlock()
	if home != 1 {
		t.Fatalf("%d home rows stored as pinned, want 1", home)
	}

	e.clk.Advance(time.Minute)
	c.set(notify.SpaceCount{SpaceID: "spc_1", Name: "Alpha", N: 7})
	stop = e.run(e.maps)
	defer stop()
	e.touch("hum_1", 2)
	e.waitCalls("editMessageText", 1)
	if txt := lastText(e, 1001); !strings.Contains(txt, "Needs you — 7") {
		t.Errorf("text after the restart %q", txt)
	}
	if n, p := len(e.fake.Calls("sendMessage")), len(e.fake.Calls("pinChatMessage")); n != 1 || p != 1 {
		t.Errorf("%d sends and %d pins after the restart, want 1 and 1", n, p)
	}
}

// Without a Mini App host the button is the startapp=needs link, a plain URL button.
func TestHomeSummaryFallsBackToTheStartAppLink(t *testing.T) {
	e, c := homeEnv(t, 1)
	e.needsURL = ""
	c.set(notify.SpaceCount{SpaceID: "spc_1", Name: "Alpha", N: 1})
	stop := e.run(e.maps)
	defer stop()
	e.touch("hum_1", 1)
	m := e.waitCalls("sendMessage", 1)[0]
	if s := m.Params["reply_markup"]; strings.Contains(s, "web_app") {
		t.Errorf("a web_app button without the host: %s", s)
	}
}

// A summary nobody has needed yet isn't sent, and one for a link with nothing waiting says so once there is a summary.
func TestHomeSummaryNotSentWhenNothingWaits(t *testing.T) {
	e, c := homeEnv(t, 1)
	c.set()
	stop := e.run(e.maps)
	defer stop()
	e.touch("hum_1", 1)
	time.Sleep(100 * time.Millisecond)
	if n := len(e.fake.Calls("sendMessage")); n != 0 {
		t.Fatalf("%d sends with nothing waiting", n)
	}
}
