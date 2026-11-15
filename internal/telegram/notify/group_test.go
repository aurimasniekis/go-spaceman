package notify_test

import (
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/notify"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
)

// SM-TG-002 (TG-13-1): a card for a group chat is rendered at summary whatever the space's content level: the same
// full space sends the body to a private chat and not to a group.
func TestGroupChatGetsOnlyTheSummary(t *testing.T) {
	for chat, wantBody := range map[int64]bool{1001: true, -100500: false} {
		e := newEnv(t, 0)
		l := notify.Link{ID: "tgl_g", UserID: "usr_g", ChatID: chat}
		e.allow(l, "spc_1")
		e.links.Set([]notify.Link{l})
		e.pol.level = settings.ContentFull
		stop := e.run(nil)
		e.rd.set(action("hum_1", 1, false))
		e.feed.add("spc_1", notify.TypeHumanAction, "hum_1", changefeed.Created, 1)
		e.ring()
		e.endWindow()
		c := e.waitCalls("sendMessage", 1)
		if got := strings.Contains(c[0].Params["text"], "Please approve"); got != wantBody {
			t.Errorf("chat %d: body shown = %v in %q", chat, got, c[0].Params["text"])
		}
		stop()
	}
}
