package notify_test

import (
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/card"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
)

// A card is retracted when the link loses sight of the item (grant revoked, space muted or hidden) without the item
// changing: the change itself is enough, and the continuations go with it.
func TestCardRetractedEagerlyOnAccessChange(t *testing.T) {
	for name, c := range map[string]struct {
		revoke func(e *env)
		topic  string
	}{
		"grants": {topic: eventbus.TopicGrantsChanged, revoke: func(e *env) {
			e.az.mu.Lock()
			delete(e.az.space["telegram:tgl_1"], "spc_1")
			e.az.mu.Unlock()
		}},
		"muted": {topic: eventbus.TopicTelegramLink, revoke: func(e *env) {
			muted := []string{"spc_1"}
			e.editLink(func(l *notify.Link) { l.Prefs.MutedSpaces = &muted })
		}},
		"hidden": {topic: eventbus.TopicTelegramSettings, revoke: func(e *env) { e.pol.hide("spc_1") }},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, 1)
			defer e.run(nil)()
			r := question("hum_1", 1)
			r.Detail = longBody(12)
			e.change("hum_1", r, 1, changefeed.Created)
			e.waitExactly(3)
			c.revoke(e)
			e.bus.Publish(eventbus.Event{Topic: c.topic})
			e.waitExactly(1)
			deadline := time.Now().Add(10 * time.Second)
			for e.fake.Messages(1001)[0].Text != card.Gone {
				if time.Now().After(deadline) {
					t.Fatalf("card still shows %q", e.fake.Messages(1001)[0].Text)
				}
				time.Sleep(2 * time.Millisecond)
			}
		})
	}
}

// A sweep leaves alone the cards whose link still sees the item, and never shows more: a card of a link that keeps
// access isn't touched by another link's loss.
func TestSweepOnlyRetractsWhatIsLost(t *testing.T) {
	e := newEnv(t, 2)
	defer e.run(nil)()
	e.change("hum_1", question("hum_1", 1), 1, changefeed.Created)
	e.waitCalls("sendMessage", 2)
	e.az.mu.Lock()
	delete(e.az.space["telegram:tgl_1"], "spc_1")
	e.az.mu.Unlock()
	e.bus.Publish(eventbus.Event{Topic: eventbus.TopicGrantsChanged})
	deadline := time.Now().Add(10 * time.Second)
	for len(e.fake.Messages(1001)) != 1 || e.fake.Messages(1001)[0].Text != card.Gone {
		if time.Now().After(deadline) {
			t.Fatal("link 1's card was not retracted")
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if m := e.fake.Messages(1002); len(m) != 1 || m[0].Text == card.Gone {
		t.Fatalf("link 2's card %+v was retracted", m)
	}
}

// The last alert is remembered across a restart: the reminder after it deletes it, and the settled card deletes the
// last one, as without a restart.
func TestAlertSurvivesARestart(t *testing.T) {
	e := newEnv(t, 1)
	stop := e.run(e.maps)
	e.change("hum_1", action("hum_1", 1, false), 1, changefeed.Created)
	e.waitExactlyMsgs(1)
	a := action("hum_1", 1, false)
	a.RemindedAt = e.clk.Now().Add(time.Hour).UnixMicro()
	e.update(a, 1)
	first := e.waitExactlyMsgs(2)[1]
	time.Sleep(100 * time.Millisecond) // the alert is recorded just after the call returns
	stop()                             // flushes the mappings
	e.maps.mu.Lock()
	var alerts int
	for _, r := range e.maps.rows {
		if r.Kind == notify.MappingAlert && r.MessageID == first.MessageID {
			alerts++
		}
	}
	e.maps.mu.Unlock()
	if alerts != 1 {
		t.Fatalf("%d alert rows after shutdown, want 1", alerts)
	}

	stop = e.run(e.maps)
	defer stop()
	a.RemindedAt = e.clk.Now().Add(2 * time.Hour).UnixMicro()
	e.update(a, 1)
	deadline := time.Now().Add(10 * time.Second)
	for {
		ms := e.fake.Messages(1001)
		if len(ms) == 2 && ms[1].MessageID != first.MessageID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("messages %d, want the card and the newest reminder only (the one before the restart deleted)", len(ms))
		}
		time.Sleep(2 * time.Millisecond)
	}
	e.update(action("hum_1", 2, true), 2)
	e.waitExactlyMsgs(1)
	time.Sleep(100 * time.Millisecond)
	stop()
	e.maps.mu.Lock()
	defer e.maps.mu.Unlock()
	for _, r := range e.maps.rows {
		if r.Kind == notify.MappingAlert {
			t.Errorf("alert row %d left after the card settled", r.MessageID)
		}
	}
}
