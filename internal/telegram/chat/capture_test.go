package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// msg delivers a message with the given content to the handler (the way the dispatcher would for a linked user).
func (e *env) msg(edit func(*botapi.Message)) {
	e.upd++
	m := &botapi.Message{MessageID: int(e.upd), From: &botapi.User{ID: chatID}, Chat: botapi.Chat{ID: chatID, Type: botapi.ChatPrivate}}
	edit(m)
	e.h.Handle(context.Background(), botapi.Update{UpdateID: e.upd, Message: m}, e.l)
}

func (e *env) pickAlpha() { e.say("/space alpha") }

func (e *env) texts() []string {
	var out []string
	for _, m := range e.srv.Messages(chatID) {
		out = append(out, m.Text)
	}
	return out
}

func (e *env) waitFor(what string, ok func() bool) {
	e.t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for %s", what)
}

func (e *env) createdN() int {
	e.api.mu.Lock()
	defer e.api.mu.Unlock()
	return len(e.api.created)
}

func photo(e *env, content []byte) []botapi.PhotoSize {
	id := e.srv.AddFile("p.jpg", content, int64(len(content)))
	return []botapi.PhotoSize{{FileID: id, Width: 90, Height: 90}, {FileID: id, Width: 800, Height: 600, FileSize: int64(len(content))}}
}

// SM-TG-007 (TG-8a-2): a plain text message becomes an inbox item in the current space, as the link's principal, and is
// acked with the item's title and a button to open it. The item is created through the API only (no source label is
// sent), with an idempotency key naming the message.
func TestCaptureText(t *testing.T) {
	e := newEnv(t)
	e.pickAlpha()
	e.msg(func(m *botapi.Message) { m.Text = "Buy milk\nand eggs" })
	if e.createdN() != 1 {
		t.Fatalf("created %d", e.createdN())
	}
	c := e.api.created[0]
	if c["title"] != "Buy milk" || c["body"] != "Buy milk\nand eggs" {
		t.Fatalf("created %v", c)
	}
	if _, ok := c["source_label"]; ok {
		t.Fatal("capture must not set a source label")
	}
	if k := e.api.createH[0].Get("Idempotency-Key"); len(k) < 8 {
		t.Fatalf("idempotency key %q", k)
	}
	ack := e.last()
	if !strings.Contains(ack.Text, "Added to the inbox of Space aa: Buy milk") || find(ack, "Open") == "" {
		t.Fatalf("ack %q", ack.Text)
	}
	if !e.api.touched("/spaces/" + spA + "/inbox") {
		t.Fatal("not captured into the chosen space")
	}
}

// The target is the link's current space; with several visible and none chosen nothing is created.
func TestCaptureNeedsASpace(t *testing.T) {
	e := newEnv(t)
	e.msg(func(m *botapi.Message) { m.Text = "hello" })
	if e.createdN() != 0 || e.last().Text != TextPickSpace {
		t.Fatalf("created %d, last %q", e.createdN(), e.last().Text)
	}
}

// A user who may not create inbox items in the space gets the API's refusal as one text, and nothing is created.
func TestCaptureForbidden(t *testing.T) {
	e := newEnv(t)
	e.h.setCurrent(e.l.ID, spViewer)
	e.msg(func(m *botapi.Message) { m.Text = "hello" })
	if e.createdN() != 0 || e.last().Text != TextNoAccess {
		t.Fatalf("created %d, last %q", e.createdN(), e.last().Text)
	}
	// A space hidden from Telegram can't be the target even if it was chosen before.
	e.h.setCurrent(e.l.ID, spB)
	e.msg(func(m *botapi.Message) { m.Text = "hello" })
	if e.createdN() != 0 || e.api.touched("/spaces/"+spB+"/inbox") {
		t.Fatal("captured into a space Telegram can't see")
	}
}

// A failed create is acked with the API's reason, and an unsupported message is told so: every capture gets an ack.
func TestCaptureAckOnFailure(t *testing.T) {
	e := newEnv(t)
	e.pickAlpha()
	e.api.createSt = 400
	e.msg(func(m *botapi.Message) { m.Text = "hello" })
	if got := e.last().Text; got != "Not accepted: title too long" {
		t.Fatalf("ack %q", got)
	}
	n := len(e.srv.Messages(chatID))
	e.msg(func(m *botapi.Message) {}) // a sticker, say: nothing to add
	if len(e.srv.Messages(chatID)) != n+1 || e.last().Text != TextNotKind {
		t.Fatalf("last %q", e.last().Text)
	}
}

// Captured text is untrusted: it goes into the body as given, and the ack for a summary space doesn't repeat it.
func TestCaptureSummarySpaceAckHasNoTitle(t *testing.T) {
	e := newEnv(t)
	e.say("/space summary")
	e.msg(func(m *botapi.Message) { m.Text = "SECRET TITLE" })
	if e.createdN() != 1 || strings.Contains(e.last().Text, "SECRET") {
		t.Fatalf("created %d, ack %q", e.createdN(), e.last().Text)
	}
}

// A photo becomes an attachment (the largest size, downloaded server-side and uploaded through the API); its caption is
// the body.
func TestCapturePhotoAndDocument(t *testing.T) {
	e := newEnv(t)
	e.pickAlpha()
	png := []byte("\x89PNG\r\n\x1a\n-pixels-")
	e.msg(func(m *botapi.Message) { m.Photo = photo(e, png); m.Caption = "the receipt" })
	if e.createdN() != 1 {
		t.Fatalf("created %d; texts %q", e.createdN(), e.texts())
	}
	c := e.api.created[0]
	ids, _ := c["attachment_ids"].([]any)
	if len(ids) != 1 || c["body"] != "the receipt" {
		t.Fatalf("created %v", c)
	}
	a := e.api.atts[ids[0].(string)]
	if string(a.content) != string(png) || !strings.HasPrefix(a.meta.Name, "photo-") {
		t.Fatalf("upload %q %q", a.meta.Name, a.content)
	}
	id := e.srv.AddFile("report.pdf", []byte("%PDF-1.4 x"), 10)
	e.msg(func(m *botapi.Message) {
		m.Document = &botapi.Document{FileID: id, FileName: "report.pdf", MimeType: "text/html", FileSize: 10}
	})
	if e.createdN() != 2 {
		t.Fatalf("created %d", e.createdN())
	}
	c = e.api.created[1]
	if c["title"] != "File: report.pdf" || !strings.Contains(fmt.Sprint(c["body"]), "1 attachment") {
		t.Fatalf("created %v", c)
	}
	if !strings.Contains(e.last().Text, "1 attachment(s)") {
		t.Fatalf("ack %q", e.last().Text)
	}
}

// A file over 20 MB is refused cleanly before any download, and nothing is created.
func TestCaptureTooBig(t *testing.T) {
	e := newEnv(t)
	e.pickAlpha()
	id := e.srv.AddFile("big.bin", nil, 21<<20)
	e.msg(func(m *botapi.Message) {
		m.Document = &botapi.Document{FileID: id, FileName: "big.bin", FileSize: 21 << 20}
	})
	if e.createdN() != 0 || e.last().Text != TextTooBig {
		t.Fatalf("created %d, last %q", e.createdN(), e.last().Text)
	}
	if len(e.srv.Calls("getFile")) != 0 {
		t.Fatal("a file known to be too big was fetched")
	}
	// Telegram not saying how big it is: getFile refuses it, and so does the download.
	id = e.srv.AddFile("big2.bin", nil, 21<<20)
	e.msg(func(m *botapi.Message) { m.Document = &botapi.Document{FileID: id, FileName: "big2.bin"} })
	if e.createdN() != 0 || e.last().Text != TextNoFile {
		t.Fatalf("created %d, last %q", e.createdN(), e.last().Text)
	}
}

// The API's own upload refusal (size cap, quota) is the ack, and no item is created.
func TestCaptureUploadRefused(t *testing.T) {
	e := newEnv(t)
	e.pickAlpha()
	e.api.uploadSt = 400
	id := e.srv.AddFile("a.txt", []byte("hello"), 5)
	e.msg(func(m *botapi.Message) { m.Document = &botapi.Document{FileID: id, FileName: "a.txt"} })
	if e.createdN() != 0 || !strings.HasPrefix(e.last().Text, "Not accepted: attachments are limited") {
		t.Fatalf("created %d, last %q", e.createdN(), e.last().Text)
	}
}

// A forward puts the origin's name in the body as a quoted line, never in a source label; names are cleaned of
// markup and control characters.
func TestCaptureForwardPlacement(t *testing.T) {
	e := newEnv(t)
	e.pickAlpha()
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	e.msg(func(m *botapi.Message) {
		m.Text = "look at this"
		m.ForwardOrigin = raw(map[string]any{"type": "user", "sender_user": map[string]any{"first_name": "Eve [click](http://x)", "last_name": "Mal\x00lory"}})
	})
	e.msg(func(m *botapi.Message) {
		m.Text = "channel post"
		m.ForwardOrigin = raw(map[string]any{"type": "channel", "chat": map[string]any{"title": "News"}, "author_signature": "Ed"})
	})
	e.msg(func(m *botapi.Message) {
		m.Text = "hidden"
		m.ForwardOrigin = raw(map[string]any{"type": "hidden_user", "sender_user_name": "Ghost"})
	})
	if e.createdN() != 3 {
		t.Fatalf("created %d", e.createdN())
	}
	want := []string{"> Forwarded from Eve clickhttp://x Mal lory\n\nlook at this", "> Forwarded from News (Ed)\n\nchannel post", "> Forwarded from Ghost\n\nhidden"}
	for i, w := range want {
		c := e.api.created[i]
		if c["body"] != w {
			t.Errorf("body %d = %q, want %q", i, c["body"], w)
		}
		if strings.Contains(fmt.Sprint(c["title"]), "Forwarded") {
			t.Errorf("the origin leaked into the title: %v", c["title"])
		}
		if _, ok := c["source_label"]; ok {
			t.Error("source label set")
		}
	}
	// The item's source label is the API's (the principal's name), not the forward's origin.
	if got := e.api.items[fmt.Sprintf("inb_%026d", 1)].SourceLabel; got != "Alice Operator" {
		t.Fatalf("source label %q", got)
	}
}

// An album (several messages with one media_group_id) becomes one item with all its files, one caption, and one ack;
// parts beyond Telegram's limit of ten are counted, not kept.
func TestCaptureAlbumIsOneItem(t *testing.T) {
	e := newEnv(t)
	e.pickAlpha()
	for i := 0; i < 3; i++ {
		e.msg(func(m *botapi.Message) {
			m.MediaGroupID = "g1"
			m.Photo = photo(e, []byte(fmt.Sprintf("\x89PNG\r\n\x1a\nimg%d", i)))
			if i == 1 {
				m.Caption = "trip"
			}
		})
	}
	if e.createdN() != 0 {
		t.Fatal("an album was created before it was complete")
	}
	e.waitFor("the album", func() bool { return e.createdN() == 1 })
	e.waitFor("the ack", func() bool { return strings.Contains(strings.Join(e.texts(), "|"), "3 attachment(s)") })
	time.Sleep(100 * time.Millisecond)
	if e.createdN() != 1 {
		t.Fatalf("created %d items for one album", e.createdN())
	}
	c := e.api.created[0]
	if ids, _ := c["attachment_ids"].([]any); len(ids) != 3 || c["body"] != "trip" {
		t.Fatalf("created %v", c)
	}
	e.h.mu.Lock()
	open := len(e.h.albums)
	e.h.mu.Unlock()
	if open != 0 {
		t.Fatalf("%d albums still held", open)
	}
}

func TestAlbumBoundsAndLimit(t *testing.T) {
	e := newEnv(t)
	e.pickAlpha()
	for i := 0; i < 12; i++ {
		e.msg(func(m *botapi.Message) { m.MediaGroupID = "big"; m.Text = fmt.Sprintf("part %d", i) })
	}
	e.waitFor("the album", func() bool { return strings.Contains(strings.Join(e.texts(), "|"), "2 more file(s)") })
	if e.createdN() != 1 {
		t.Fatalf("created %d", e.createdN())
	}
	// At most three albums are open per link.
	for _, g := range []string{"a", "b", "c", "d"} {
		e.msg(func(m *botapi.Message) { m.MediaGroupID = g; m.Text = g })
	}
	if got := e.last().Text; got != TextAlbumBusy {
		t.Fatalf("last %q", got)
	}
	e.waitFor("the three albums", func() bool { return e.createdN() == 4 })
}

// Capture and file moves cost the link's inbound limits; over them the user is told, and nothing is created.
func TestCaptureLimits(t *testing.T) {
	e := newEnv(t)
	e.pickAlpha()
	e.allow = func(_ string, k updates.Kind) bool { return k != updates.KindInboxItem }
	e.msg(func(m *botapi.Message) { m.Text = "hello" })
	if e.createdN() != 0 || e.last().Text != TextSlow {
		t.Fatalf("created %d, last %q", e.createdN(), e.last().Text)
	}
	e.allow = func(_ string, k updates.Kind) bool { return k != updates.KindDownload }
	id := e.srv.AddFile("a.txt", []byte("hello"), 5)
	e.msg(func(m *botapi.Message) { m.Document = &botapi.Document{FileID: id, FileName: "a.txt"} })
	if e.createdN() != 0 || e.last().Text != TextSlow || len(e.srv.Calls("getFile")) != 0 {
		t.Fatalf("created %d, last %q", e.createdN(), e.last().Text)
	}
}

// A reply that is not to a reject prompt is captured; one that is stays the reason.
func TestCaptureReplyIsCaptured(t *testing.T) {
	e := newEnv(t)
	e.pickAlpha()
	e.msg(func(m *botapi.Message) { m.Text = "reply text"; m.ReplyToMessage = &botapi.Message{MessageID: 99} })
	if e.createdN() != 1 {
		t.Fatalf("created %d", e.createdN())
	}
}

// Without a principal (a revoked link) nothing is fetched or created.
func TestCaptureNoPrincipal(t *testing.T) {
	e := newEnv(t)
	e.pickAlpha()
	e.principalErr = fmt.Errorf("revoked")
	n := len(e.api.seen())
	e.msg(func(m *botapi.Message) { m.Text = "hello" })
	if len(e.api.seen()) != n || e.last().Text != TextNoAccess {
		t.Fatal("capture reached the API without a principal")
	}
}

// Attachments go back to Telegram when an item is read in a space with content level full: the metadata is read as the
// principal first, the file is sent with protect_content, an inline image as a photo, anything else as a document.
// Summary spaces send nothing.
func TestAttachmentRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.pickAlpha()
	png := []byte("\x89PNG\r\n\x1a\n-pixels-")
	e.msg(func(m *botapi.Message) { m.Photo = photo(e, png); m.Caption = "pic" })
	id := e.srv.AddFile("n.txt", []byte("some notes"), 10)
	e.msg(func(m *botapi.Message) {
		m.Document = &botapi.Document{FileID: id, FileName: "n.txt"}
		m.Caption = "notes"
	})
	e.api.atts[fmt.Sprintf("att_%026d", 1)].meta.Inline = true
	e.api.atts[fmt.Sprintf("att_%026d", 1)].meta.MIME = "image/png"

	// Read both items from the list.
	e.say("/inbox alpha")
	list := e.last()
	for _, title := range []string{"pic", "notes"} {
		e.tapData(find(list, title), list)
	}
	e.mu.Lock()
	files := append([]sentFile(nil), e.files...)
	e.mu.Unlock()
	if len(files) != 2 {
		t.Fatalf("sent %d files", len(files))
	}
	for _, f := range files {
		if !f.protect {
			t.Errorf("%s of %s was sent without protect_content", f.method, f.name)
		}
	}
	byName := map[string]sentFile{}
	for _, f := range files {
		byName[f.method] = f
	}
	if string(byName["sendPhoto"].data) != string(png) || string(byName["sendDocument"].data) != "some notes" {
		t.Fatalf("round trip: %v", files)
	}

	// Content level summary: the card says an attachment exists, no bytes go out.
	sum := e.addItem(spS, iid('9'), "S", "new", "b")
	sum.Attachments = append(sum.Attachments, e.api.atts[fmt.Sprintf("att_%026d", 2)].meta)
	e.say("/inbox summary")
	e.tapData(find(e.last(), "S"), e.last())
	e.mu.Lock()
	n := len(e.files)
	e.mu.Unlock()
	if n != 2 {
		t.Fatalf("a summary space sent files: %d", n)
	}
}

// An attachment the principal can't read through the API (metadata 404) is not sent, and the limit on downloads and
// the size cap hold.
func TestAttachmentOutChecksAccess(t *testing.T) {
	e := newEnv(t)
	e.pickAlpha()
	e.api.atts["att_"+strings.Repeat("0", 26)] = &storedAtt{meta: attachmentRef{ID: "att_" + strings.Repeat("0", 26), Name: "x.txt", Size: 3, MIME: "text/plain"}, content: []byte("abc")}
	it := e.addItem(spA, iid('8'), "With file", "new", "b")
	it.Attachments = []attachmentRef{{ID: "att_" + strings.Repeat("0", 26), Name: "x.txt"}, {ID: "att_" + strings.Repeat("1", 26), Name: "ghost.txt"}}
	e.say("/inbox alpha")
	e.tapData(find(e.last(), "With file"), e.last())
	e.mu.Lock()
	files := append([]sentFile(nil), e.files...)
	e.mu.Unlock()
	if len(files) != 1 || files[0].name != "x.txt" {
		t.Fatalf("sent %v", files)
	}
	// Too many downloads: nothing more is sent and the user is told.
	e.allow = func(_ string, k updates.Kind) bool { return k != updates.KindDownload }
	e.tapData(find(e.last(), "With file"), e.last())
	e.mu.Lock()
	n := len(e.files)
	e.mu.Unlock()
	if n != 1 {
		t.Fatalf("sent %d files over the limit", n)
	}
}
