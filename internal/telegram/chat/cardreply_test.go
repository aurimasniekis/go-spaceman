package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

func TestMatchOption(t *testing.T) {
	opts := []string{"red", "blue", "green"}
	for _, c := range []struct {
		in   string
		idx  int
		ambi bool
	}{
		{"a", 0, false}, {"B", 1, false}, {" c. ", 2, false}, {"2", 1, false}, {"3)", 2, false},
		{"Blue", 1, false}, {" GREEN ", 2, false}, {"d", -1, false}, {"4", -1, false}, {"0", -1, false}, {"purple", -1, false}, {"", -1, false},
	} {
		if i, amb := matchOption(opts, c.in); i != c.idx || amb != c.ambi {
			t.Errorf("%q: got %d %v, want %d %v", c.in, i, amb, c.idx, c.ambi)
		}
	}
	// A label that is also another option's letter or number is ambiguous; a label alone naming the same option is not.
	if _, amb := matchOption([]string{"B", "x", "y"}, "b"); !amb {
		t.Error("label B (option 1) vs letter B (option 2) should be ambiguous")
	}
	if i, amb := matchOption([]string{"x", "B", "y"}, "b"); i != 1 || amb {
		t.Errorf("label and letter name the same option: got %d %v", i, amb)
	}
	if _, amb := matchOption([]string{"3", "y", "z"}, "3"); !amb {
		t.Error("label 3 vs number 3 should be ambiguous")
	}

}

func (e *env) itemsInSpace(space string) int {
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	n := 0
	for _, it := range e.api.items {
		if it.SpaceID == space {
			n++
		}
	}
	return n
}

// A reply to another space's question card answers that card by letter, number or label; the current space gets
// nothing.
func TestCardReplyAnswersChoice(t *testing.T) {
	for i, in := range []string{"B", "b", "2", "Blue", " blue "} {
		e := newEnv(t)
		reg := e.withCards()
		e.pickAlpha() // the current space is not the card's
		a := e.addHuman(spS, hid('1'), "question", "Which one?", `{"options":["red","blue"]}`)
		c := e.postCard(reg, a, "tgl_1")
		e.reply(in, c.MessageID, chatID, e.l)
		if e.responded() != 1 || e.api.responds[0]["answer"] != "blue" || e.api.respHdr[0] != `"4"` {
			t.Fatalf("case %d %q: %v %v", i, in, e.api.responds, e.api.respHdr)
		}
		if got := e.last().Text; got != "Answered: blue" {
			t.Fatalf("confirmation %q", got)
		}
		if e.createdN() != 0 {
			t.Fatalf("case %d: the reply was also captured", i)
		}
		if got := e.cardOf(c.MessageID); !strings.HasPrefix(got.Text, "✅ Answered") {
			t.Fatalf("card after the answer: %q", got.Text)
		}
	}
}

func TestCardReplyFreeText(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	e.pickAlpha()
	a := e.addHuman(spS, hid('1'), "question", "Name?", `{}`)
	c := e.postCard(reg, a, "tgl_1")
	e.reply("Mallory the Great", c.MessageID, chatID, e.l)
	if e.responded() != 1 || e.api.responds[0]["answer"] != "Mallory the Great" || e.createdN() != 0 {
		t.Fatalf("%v created %d", e.api.responds, e.createdN())
	}
	// Options plus allow_text: a non-option reply is the free-text answer.
	b := e.addHuman(spS, hid('2'), "question", "Pick or say", `{"options":["red","blue"],"allow_text":true}`)
	c2 := e.postCard(reg, b, "tgl_1")
	e.reply("teal", c2.MessageID, chatID, e.l)
	if e.responded() != 2 || e.api.responds[1]["answer"] != "teal" {
		t.Fatalf("%v", e.api.responds)
	}
}

func TestCardReplyResolved(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	e.pickAlpha()
	a := e.addHuman(spS, hid('1'), "question", "Which one?", `{"options":["red","blue"]}`)
	c := e.postCard(reg, a, "tgl_1")
	a.Status = "resolved"
	e.reply("A", c.MessageID, chatID, e.l)
	if e.responded() != 0 || e.last().Text != TextCardReplyResolved || e.createdN() != 0 {
		t.Fatalf("%d responds, %q, created %d", e.responded(), e.last().Text, e.createdN())
	}
}

func TestCardReplyAmbiguous(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	e.pickAlpha()
	a := e.addHuman(spS, hid('1'), "question", "Which one?", `{"options":["B","x","y"]}`)
	c := e.postCard(reg, a, "tgl_1")
	e.reply("b", c.MessageID, chatID, e.l)
	if e.responded() != 0 || e.createdN() != 0 || !strings.HasPrefix(e.last().Text, "Which option?") {
		t.Fatalf("%d responds, created %d, %q", e.responded(), e.createdN(), e.last().Text)
	}
}

// A reply that can't be an answer goes to the card's own space, not the current one.
func TestCardReplyUnmatchedGoesToCardSpace(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	e.pickAlpha()
	a := e.addHuman(spS, hid('1'), "question", "Which one?", `{"options":["red","blue"]}`)
	c := e.postCard(reg, a, "tgl_1")
	s0, a0 := e.itemsInSpace(spS), e.itemsInSpace(spA)
	e.reply("neither of those", c.MessageID, chatID, e.l)
	if e.responded() != 0 || e.itemsInSpace(spS) != s0+1 || e.itemsInSpace(spA) != a0 {
		t.Fatalf("%d responds, items S %d A %d", e.responded(), e.itemsInSpace(spS), e.itemsInSpace(spA))
	}
	// A todo has no text answer either.
	b := e.addHuman(spS, hid('2'), "todo", "Do", `{"steps":["a"]}`)
	c2 := e.postCard(reg, b, "tgl_1")
	e.reply("done", c2.MessageID, chatID, e.l)
	if e.responded() != 0 || e.itemsInSpace(spS) != s0+2 || e.itemsInSpace(spA) != a0 {
		t.Fatalf("todo: items S %d A %d", e.itemsInSpace(spS), e.itemsInSpace(spA))
	}
}

// An approval is never decided from text, and the reply isn't routed anywhere.
func TestCardReplyNeverDecidesApproval(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	e.pickAlpha()
	a := e.addHuman(spS, hid('1'), "approval", "Deploy", `{"action":"deploy"}`)
	c := e.postCard(reg, a, "tgl_1")
	e.reply("approve", c.MessageID, chatID, e.l)
	e.reply("yes", c.MessageID, chatID, e.l)
	if e.responded() != 0 || e.createdN() != 0 || e.last().Text != TextCardReplyApproval {
		t.Fatalf("%d responds, created %d, %q", e.responded(), e.createdN(), e.last().Text)
	}
}

// Another link's card, a stale card and an unknown message are not the reply's to answer.
func TestCardReplyChecks(t *testing.T) {
	e := newEnv(t)
	reg := e.withCards()
	e.pickAlpha()
	a := e.addHuman(spS, hid('1'), "question", "Which one?", `{"options":["red","blue"]}`)
	theirs := e.postCard(reg, a, "tgl_other")
	e.reply("A", theirs.MessageID, chatID, e.l)
	if e.responded() != 0 {
		t.Fatal("answered another link's card")
	}
	n0 := e.createdN()
	mine := e.postCard(reg, a, "tgl_1")
	a.Version = 5 // the card shows 4
	e.reply("A", mine.MessageID, chatID, e.l)
	if e.responded() != 0 || e.last().Text != TextCardChanged || e.createdN() != n0 {
		t.Fatalf("stale: %d responds, %q", e.responded(), e.last().Text)
	}
	a.Version = 4
	e.h.o.Card = nil // no card lookup: no answer from a reply
	e.reply("A", mine.MessageID, chatID, e.l)
	if e.responded() != 0 {
		t.Fatal("responded without a card lookup")
	}
}

// A reply to a card of something other than an action goes to the card's space.
func TestCardSpaceReply(t *testing.T) {
	e := newEnv(t)
	e.pickAlpha()
	e.h.o.CardSpace = func(_ context.Context, chat int64, msg int) (string, string, bool) {
		return spS, "tgl_1", chat == chatID && msg == 777
	}
	s0, a0 := e.itemsInSpace(spS), e.itemsInSpace(spA)
	e.reply("a note", 777, chatID, e.l)
	if e.itemsInSpace(spS) != s0+1 || e.itemsInSpace(spA) != a0 {
		t.Fatalf("items S %d A %d", e.itemsInSpace(spS), e.itemsInSpace(spA))
	}
	e.reply("another", 778, chatID, e.l) // not a card: the current space, as before
	if e.itemsInSpace(spA) != a0+1 {
		t.Fatalf("plain reply: items A %d", e.itemsInSpace(spA))
	}
}

var _ = botapi.Message{}
