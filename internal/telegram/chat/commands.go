package chat

import (
	"context"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/commands"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// Texts of the commands that point at the Mini App.
const (
	TextNoMiniApp = "The Mini App isn't set up for this bot yet. Ask an administrator."
	TextOpenApp   = "Open SpaceMan in Telegram."
)

// privateHandlers answers each command of commands.All in a private chat; groupHandlers each one with Group set.
// Both are checked against the table (commands_test.go).
var (
	privateHandlers map[string]func(h *Handler, ctx context.Context, l updates.Link, arg string)
	groupHandlers   map[string]func(h *Handler, ctx context.Context, m *botapi.Message, l updates.Link, arg string)
)

func init() {
	needs := (*Handler).cmdNeedsYou
	groupOf := func(f func(*Handler, context.Context, updates.Link, string)) func(*Handler, context.Context, *botapi.Message, updates.Link, string) {
		return func(h *Handler, ctx context.Context, _ *botapi.Message, l updates.Link, arg string) {
			f(h, ctx, l, arg)
		}
	}
	// They don't apply to a group; the reply shows nothing of any space.
	fixed := func(h *Handler, ctx context.Context, m *botapi.Message, _ updates.Link, _ string) {
		h.replyIn(ctx, m, TextGroupFixed)
	}
	privateHandlers = map[string]func(*Handler, context.Context, updates.Link, string){
		"needs": needs, "needsyou": needs, "inbox": (*Handler).cmdInbox, "queue": (*Handler).cmdQueue, "runs": (*Handler).cmdRuns,
		"messages": (*Handler).cmdMessages, "control": (*Handler).cmdControl, "space": (*Handler).cmdSpace,
		"spaces":   func(h *Handler, ctx context.Context, l updates.Link, _ string) { h.cmdSpaces(ctx, l) },
		"settings": (*Handler).cmdSettings, "help": (*Handler).cmdHelp,
		// /unlink is the dispatcher's (updates); it never reaches here.
		"unlink": func(*Handler, context.Context, updates.Link, string) {},
	}
	groupHandlers = map[string]func(*Handler, context.Context, *botapi.Message, updates.Link, string){
		"needsyou": groupOf(needs), "inbox": groupOf((*Handler).cmdInbox), "queue": groupOf((*Handler).cmdQueue),
		"runs": groupOf((*Handler).cmdRuns), "messages": groupOf((*Handler).cmdMessages),
		"control": fixed, "spaces": fixed, "space": fixed,
	}
}

func (h *Handler) cmdHelp(ctx context.Context, l updates.Link, _ string) {
	var b strings.Builder
	b.WriteString("SpaceMan on Telegram. Send any text, photo or file to add it to the current space's inbox.\n")
	for _, c := range commands.Menu() {
		b.WriteString("\n/" + c.Command + " - " + c.Description)
	}
	h.send(ctx, l.ChatID, reply{text: b.String()})
}

// cmdSettings opens the Mini App's settings page; without the Mini App host it says so.
func (h *Handler) cmdSettings(ctx context.Context, l updates.Link, _ string) {
	var u string
	if h.o.AppPage != nil {
		u = h.o.AppPage("/settings")
	}
	if u == "" {
		h.send(ctx, l.ChatID, reply{text: TextNoMiniApp})
		return
	}
	h.send(ctx, l.ChatID, reply{text: TextOpenApp, markup: markup([]botapi.InlineKeyboardButton{{Text: "Settings", WebApp: &botapi.WebAppInfo{URL: u}}})})
}
