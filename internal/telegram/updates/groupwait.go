package updates

import (
	"context"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// The bot's membership of a group that isn't bound (ADR-0083 amendment TG-13-2). Binding checks that the bot is a
// member, so it can't leave a forum supergroup the moment it is added; instead it waits there, silent and reading
// nothing, for at most GroupGrace. A group that is still unbound by then is left, so nobody can park the bot in
// groups. The wait is kept in memory: after a restart the bot has forgotten which groups it waits in, so the first
// update of any unbound group it finds itself in makes it leave at once (a restart never opens a window that doesn't
// close). Chats that can never be bound (channels, basic groups, supergroups without topics) are left at once.

// DefaultGroupGrace is how long the bot waits in an unbound forum supergroup for it to be bound.
const DefaultGroupGrace = 15 * time.Minute

type groupWait struct {
	mu       sync.Mutex
	deadline map[int64]time.Time
}

// canBeBound reports whether a chat could be bound to a space: only a supergroup with topics (forum mode) can.
func canBeBound(c botapi.Chat) bool { return c.Type == botapi.ChatSupergroup && c.IsForum }

// startWaiting makes the bot wait for chatID's binding until now+grace (the first add counts; a repeat doesn't extend it).
func (d *Dispatcher) startWaiting(chatID int64) {
	d.wait.mu.Lock()
	defer d.wait.mu.Unlock()
	if d.wait.deadline == nil {
		d.wait.deadline = map[int64]time.Time{}
	}
	if _, ok := d.wait.deadline[chatID]; !ok {
		d.wait.deadline[chatID] = d.clock.Now().Add(d.grace)
	}
}

func (d *Dispatcher) forgetWait(chatID int64) {
	d.wait.mu.Lock()
	defer d.wait.mu.Unlock()
	delete(d.wait.deadline, chatID)
}

func (d *Dispatcher) waiting(chatID int64) bool {
	d.wait.mu.Lock()
	defer d.wait.mu.Unlock()
	_, ok := d.wait.deadline[chatID]
	return ok
}

// SweepGroups leaves every group whose wait is over and that is still unbound, and forgets the ones that got bound.
// The service calls it every minute; it is also cheap enough to call on every update.
func (d *Dispatcher) SweepGroups(ctx context.Context) {
	now := d.clock.Now()
	var due []int64
	d.wait.mu.Lock()
	for id, at := range d.wait.deadline {
		if !now.Before(at) {
			due = append(due, id)
			delete(d.wait.deadline, id)
		}
	}
	d.wait.mu.Unlock()
	for _, id := range due {
		if d.groups != nil {
			if _, bound := d.groups.GroupSpace(id); bound {
				continue
			}
		}
		d.LeaveGroup(ctx, id)
	}
}

// groupActivity handles a message or a button tap from a group the bot isn't bound to. It reports whether the update was
// one (the caller drops it): nothing in it is read. A chat that can't be bound, or one the bot isn't waiting in (it was
// forgotten at a restart), makes the bot leave.
func (d *Dispatcher) groupActivity(ctx context.Context, u *botapi.Update) bool {
	var chat botapi.Chat
	switch {
	case u.CallbackQuery != nil && u.CallbackQuery.Message != nil:
		chat = u.CallbackQuery.Message.Chat
	case u.Message != nil:
		chat = u.Message.Chat
	default:
		return false
	}
	if chat.Type == botapi.ChatPrivate {
		return false
	}
	if _, ok := d.boundGroup(chat); ok {
		return false
	}
	d.metrics.TelegramUpdate(metrics.TelegramUpdateIgnored)
	if !canBeBound(chat) || !d.waiting(chat.ID) {
		d.LeaveGroup(ctx, chat.ID)
	}
	return true
}
