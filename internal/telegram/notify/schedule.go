package notify

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/sender"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
)

// Quiet hours and the digest are wall-clock times in the link's user's own time zone (Link.Zone, from the user's
// notification preferences). A user without a zone gets neither: there is no server-wide fallback (operator answer Q9).

// quiet reports whether now falls inside the link's quiet hours. A window whose end is before its start runs past
// midnight. Evaluating the instant in the user's zone makes the daylight-saving changes follow the wall clock.
func quiet(p settings.EffectivePrefs, loc *time.Location, now time.Time) bool {
	if !p.QuietHours || loc == nil {
		return false
	}
	s, err1 := settings.ParseClock(p.QuietHoursStart)
	e, err2 := settings.ParseClock(p.QuietHoursEnd)
	if err1 != nil || err2 != nil || s == e {
		return false
	}
	t := now.In(loc)
	m := t.Hour()*60 + t.Minute()
	if s < e {
		return m >= s && m < e
	}
	return m >= s || m < e
}

// nextClock returns the first instant after `after` at which the wall clock in loc reads hh:mm. A time that doesn't
// exist on a daylight-saving day (the clock skips it) reads as the first instant after the gap, and one that exists
// twice as its first occurrence; neither repeats within a day.
func nextClock(loc *time.Location, clock string, after time.Time) (time.Time, bool) {
	mins, err := settings.ParseClock(clock)
	if err != nil {
		return time.Time{}, false
	}
	a := after.In(loc)
	for d := 0; d <= 2; d++ {
		c := time.Date(a.Year(), a.Month(), a.Day()+d, mins/60, mins%60, 0, 0, loc)
		if c.After(after) {
			return c, true
		}
	}
	return time.Time{}, false
}

// silent reports whether a notification to the link sent at now is sent without sound (disable_notification).
func (n *Notifier) silent(l *Link, now time.Time) bool {
	return quiet(l.Prefs.Effective(n.o.Policy.Current().NotificationDefaults), l.Zone, now)
}

// Digest bounds: items kept per link, and lines shown in one digest.
const (
	maxDigestItems = 50
	maxDigestLines = 20
)

// Digest sections, in the order they are shown.
const (
	secReminders = iota
	secInbox
	secTasks
	secRuns
	numSections
)

var sectionTitle = [numSections]string{"Reminders", "New inbox items", "Tasks in review", "Failed or lost runs"}

type digestItem struct {
	res     Resource // the state when it was noted; the digest shows the title and label only
	section int
}

// digestBuf holds, per link, what the link wants in its digest instead of as a message. It lives in memory only and is
// bounded; a restart drops it, and an item whose resource later finished or stopped being notable is removed.
type digestBuf struct {
	mu    sync.Mutex
	items map[string][]digestItem // link ID -> items, oldest first
	more  map[string]int          // items that did not fit
	last  map[string]time.Time    // link ID -> when its last digest was due (set on first sight)
}

func newDigestBuf() *digestBuf {
	return &digestBuf{items: map[string][]digestItem{}, more: map[string]int{}, last: map[string]time.Time{}}
}

func (d *digestBuf) add(linkID string, r *Resource, section int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	list := d.items[linkID]
	for i := range list {
		if list[i].res.Key == r.Key {
			list[i] = digestItem{*r, section}
			return
		}
	}
	if len(list) >= maxDigestItems {
		d.more[linkID]++
		return
	}
	d.items[linkID] = append(list, digestItem{*r, section})
}

// drop removes a resource from every link's digest (it finished, or is no longer worth a notification).
func (d *digestBuf) drop(k Key) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, list := range d.items {
		for i := range list {
			if list[i].res.Key == k {
				d.items[id] = append(list[:i:i], list[i+1:]...)
				break
			}
		}
	}
}

func (d *digestBuf) take(linkID string) ([]digestItem, int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	list, more := d.items[linkID], d.more[linkID]
	delete(d.items, linkID)
	delete(d.more, linkID)
	return list, more
}

// forgetExcept drops the state of links that are gone.
func (d *digestBuf) forgetExcept(live map[string]bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id := range d.items {
		if !live[id] {
			delete(d.items, id)
			delete(d.more, id)
		}
	}
	for id := range d.last {
		if !live[id] {
			delete(d.last, id)
		}
	}
}

// next is the link's next digest instant: the first wall-clock DigestTime in the user's zone after the last one (the
// first sight of the link counts as the last, so a restart sends nothing at once).
func (n *Notifier) nextDigestFor(l *Link, p settings.EffectivePrefs, now time.Time) (time.Time, bool) {
	if !p.Digest || l.Zone == nil {
		return time.Time{}, false
	}
	n.digest.mu.Lock()
	last, ok := n.digest.last[l.ID]
	if !ok {
		last = now
		n.digest.last[l.ID] = now
	}
	n.digest.mu.Unlock()
	return nextClock(l.Zone, p.DigestTime, last)
}

// NextDigest is the earliest instant a link's digest is due (zero: none). It reads memory only, so the shared deadline
// loop may call it as often as it likes.
func (n *Notifier) NextDigest(context.Context) (time.Time, error) {
	set := n.o.Policy.Current()
	now := n.o.Now()
	var next time.Time
	for _, l := range n.o.Links.Snapshot() {
		if t, ok := n.nextDigestFor(&l, l.Prefs.Effective(set.NotificationDefaults), now); ok && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	return next, nil
}

// DigestDue enqueues the digest of every link whose time has come. It makes no network call and never returns an
// error (Telegram's failures are the sender's, and an error here would back off the shared deadline loop); a link
// with nothing to report gets no message. It returns the digests enqueued.
func (n *Notifier) DigestDue(context.Context) (int, error) {
	set := n.o.Policy.Current()
	now := n.o.Now()
	snd := n.o.Sender()
	links := n.o.Links.Snapshot()
	live := make(map[string]bool, len(links))
	sent := 0
	for i := range links {
		l := &links[i]
		live[l.ID] = true
		p := l.Prefs.Effective(set.NotificationDefaults)
		due, ok := n.nextDigestFor(l, p, now)
		if !ok || due.After(now) {
			continue
		}
		n.digest.mu.Lock()
		n.digest.last[l.ID] = now
		n.digest.mu.Unlock()
		items, more := n.digest.take(l.ID)
		if snd == nil || len(items) == 0 {
			continue // the bot is off, or nothing to report
		}
		if n.enqueueDigest(snd, l, items, more) {
			sent++
		}
	}
	n.digest.forgetExcept(live)
	return sent, nil
}

func (n *Notifier) enqueueDigest(snd *sender.Sender, l *Link, items []digestItem, more int) bool {
	linkID, chat := l.ID, l.ChatID
	err := snd.Enqueue(sender.Message{ChatID: chat, Priority: sender.Digest, Key: "digest",
		Send: func(ctx context.Context, c *botapi.Client) error {
			// Like a card, the digest is checked and rendered when it is sent: an item the link may no longer see, in a space
			// that was hidden meanwhile or that the link muted, is left out.
			now := n.o.Now()
			cur := n.currentLink(linkID, chat)
			if cur == nil {
				return nil
			}
			var kept []digestItem
			for _, it := range items {
				if n.stillMay(linkID, chat, &it.res) {
					kept = append(kept, it)
				}
			}
			text, ents := renderDigest(kept, more)
			if text == "" {
				return nil
			}
			_, err := c.SendMessage(ctx, botapi.SendMessageParams{ChatID: chat, Text: text, Entities: ents, DisableNotification: n.silent(cur, now), ProtectContent: true})
			return err
		}})
	if err != nil {
		n.log.Debug("telegram notifier: digest refused", slog.String("error", err.Error()))
		return false
	}
	return true
}

// renderDigest lists the titles of what was collected (never bodies, whatever a space's content level: a digest is a
// summary), grouped by section, at most maxDigestLines lines.
func renderDigest(items []digestItem, more int) (string, []botapi.MessageEntity) {
	if len(items) == 0 {
		return "", nil
	}
	head := "Daily digest"
	var b strings.Builder
	b.WriteString(head)
	lines := 0
	for sec := 0; sec < numSections; sec++ {
		first := true
		for _, it := range items {
			if it.section != sec {
				continue
			}
			if lines >= maxDigestLines {
				more++
				continue
			}
			if first {
				b.WriteString("\n\n" + sectionTitle[sec])
				first = false
			}
			b.WriteString("\n• " + it.res.Label + ": " + clip(oneLine(it.res.Title), maxTitle))
			lines++
		}
	}
	if more > 0 {
		b.WriteString("\n…and " + strconv.Itoa(more) + " more")
	}
	b.WriteString("\n\nOpen the app for details.")
	return b.String(), []botapi.MessageEntity{{Type: "bold", Offset: 0, Length: len(head)}}
}
