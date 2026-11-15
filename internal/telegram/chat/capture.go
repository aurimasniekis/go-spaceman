package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sort"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// Inbox capture (ADR-0083, plan §2.2, TG-8a-2): a message a linked user sends that is not a command becomes an inbox
// item in the link's current space (or its only visible one). Text and captions become the body; a photo or a file
// becomes an attachment; a forward puts the origin's name in the body ("Forwarded from …"), never in the item's source
// label, which the API sets from the principal; the parts of an album (media_group_id) become one item. Everything runs
// as the link's Telegram principal through the in-process API, so the web's authorization, size caps, content sniffing,
// quotas and audit apply, and a user who may not create inbox items there gets the same refusal as in the web.
//
// Every capture is answered with one ack message (the item was added, or why it was not). A webhook update is
// acknowledged with 200 before it is handled, so Telegram never redelivers it: an update in flight (an album still
// collecting parts, a download) when central restarts is lost, and the user, who got no ack, knows to send it again.
// Retries within a run are safe: the item is created with an Idempotency-Key naming the first message.
const (
	TextTooBig    = "That file is too large: Telegram lets a bot read files up to 20 MB. Nothing was added."
	TextSlow      = "Too many items just now. Wait a moment and send it again."
	TextNotKind   = "I can only add text, photos and files to the inbox. Nothing was added."
	TextAlbumBusy = "Too many albums at once. Wait a moment and send it again."
	TextNoFile    = "I couldn't fetch that file from Telegram. Nothing was added."

	maxAlbumFiles   = 10 // Telegram's own limit for a media group
	maxOpenAlbums   = 3  // per link
	maxDownloads    = 3  // files moving from Telegram into the API at once, across all links
	albumMaxWait    = 10 * time.Second
	albumFlushLimit = 2 * time.Minute // the whole capture, downloads and uploads included
	maxSendFiles    = 3               // attachments sent back per item read
	maxSendBytes    = botapi.MaxDownloadBytes
	maxPhotoBytes   = 10 << 20 // sendPhoto's upload limit
	titleRunes      = 80
	nameRunes       = 80
)

// piece is what one Telegram message contributes to a capture.
type piece struct {
	id     int
	text   string
	origin string // "Forwarded from …", cleaned
	files  []fileRef
}

type fileRef struct {
	id   string
	name string
	size int64 // as Telegram says (0 when it doesn't)
}

type albumKey struct{ linkID, group string }

// album is a media group collecting its parts. State is bounded: at most maxOpenAlbums per link, maxAlbumFiles parts
// each, and every album is flushed at the latest albumMaxWait after its first part.
type album struct {
	link    updates.Link
	pieces  []piece
	dropped int // parts beyond maxAlbumFiles
	skipped int // parts capture does not carry (a video, a sticker)
	first   time.Time
	timer   *time.Timer
}

func (h *Handler) capture(ctx context.Context, m *botapi.Message, l updates.Link) {
	p, ok := extract(m)
	if m.MediaGroupID == "" {
		if !ok {
			h.send(ctx, l.ChatID, reply{text: TextNotKind})
			return
		}
		h.commit(ctx, l, []piece{p}, 0, 0)
		return
	}
	// An unsupported part of an album is counted on the album and mentioned in its one ack, not answered alone.
	h.addToAlbum(ctx, l, m.MediaGroupID, p, !ok)
}

// extract reads the parts of a message that capture uses; ok is false for a message with nothing to add.
func extract(m *botapi.Message) (piece, bool) {
	if len(m.Video)+len(m.VideoNote)+len(m.Voice)+len(m.Audio)+len(m.Animation)+len(m.Sticker) > 0 {
		return piece{}, false // not carried: say so rather than keep its caption alone
	}
	p := piece{id: m.MessageID, text: strings.TrimSpace(m.Text), origin: originLine(m.ForwardOrigin)}
	if p.text == "" {
		p.text = strings.TrimSpace(m.Caption)
	}
	if n := len(m.Photo); n > 0 {
		best := m.Photo[0]
		for _, s := range m.Photo[1:] {
			if s.Width*s.Height > best.Width*best.Height {
				best = s
			}
		}
		p.files = append(p.files, fileRef{id: best.FileID, name: fmt.Sprintf("photo-%d.jpg", m.MessageID), size: best.FileSize})
	}
	if d := m.Document; d != nil {
		name := d.FileName
		if clean(name, nameRunes) == "" {
			name = fmt.Sprintf("file-%d", m.MessageID)
		}
		p.files = append(p.files, fileRef{id: d.FileID, name: name, size: d.FileSize})
	}
	return p, p.text != "" || len(p.files) > 0
}

// originLine names where a forward came from, from the message origin's variants (user, hidden_user, chat, channel).
// The names are the sender's own text: control characters and markup characters are dropped, and the length is cut.
func originLine(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var o struct {
		Type       string `json:"type"`
		Date       int64  `json:"date"`
		SenderUser *struct {
			FirstName string `json:"first_name"`
			LastName  string `json:"last_name"`
			Username  string `json:"username"`
		} `json:"sender_user"`
		SenderUserName  string       `json:"sender_user_name"`
		SenderChat      *botapi.Chat `json:"sender_chat"`
		Chat            *botapi.Chat `json:"chat"`
		AuthorSignature string       `json:"author_signature"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return ""
	}
	var name string
	switch o.Type {
	case "user":
		if u := o.SenderUser; u != nil {
			name = strings.TrimSpace(u.FirstName + " " + u.LastName)
			if name == "" {
				name = u.Username
			}
		}
	case "hidden_user":
		name = o.SenderUserName
	case "chat":
		if c := o.SenderChat; c != nil {
			name = firstNonEmpty(c.Title, c.Username)
		}
	case "channel":
		if c := o.Chat; c != nil {
			name = firstNonEmpty(c.Title, c.Username)
		}
	}
	name = markupFree(clean(name, nameRunes))
	if name == "" {
		name = "an unknown source"
	}
	if sig := markupFree(clean(o.AuthorSignature, nameRunes)); sig != "" && o.Type == "channel" {
		name += " (" + sig + ")"
	}
	if o.Date > 0 {
		name += " (" + time.Unix(o.Date, 0).UTC().Format("2006-01-02") + ")"
	}
	return "Forwarded from " + name
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// markupFree drops the characters Markdown reads as links, HTML or code, so a name can't turn into markup.
func markupFree(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if strings.ContainsRune("[]()<>`*_~|\\", r) {
			return -1
		}
		return r
	}, s))
}

// --- albums ---

func (h *Handler) addToAlbum(ctx context.Context, l updates.Link, group string, p piece, skip bool) {
	k := albumKey{l.ID, group}
	now := h.o.Clock.Now()
	h.mu.Lock()
	for rk, t := range h.refused { // a refusal is remembered for about as long as the album could still be arriving
		if now.Sub(t) > albumMaxWait {
			delete(h.refused, rk)
		}
	}
	if _, no := h.refused[k]; no {
		h.mu.Unlock() // the user was told once; the rest of the parts are dropped, even if a slot has freed since
		return
	}
	a := h.albums[k]
	if a == nil {
		open := 0
		for ak, x := range h.albums {
			if now.Sub(x.first) > albumMaxWait+albumFlushLimit { // a stuck album is forgotten, never kept
				delete(h.albums, ak)
			} else if ak.linkID == l.ID {
				open++
			}
		}
		if open >= maxOpenAlbums {
			h.refused[k] = now
			h.mu.Unlock()
			h.send(ctx, l.ChatID, reply{text: TextAlbumBusy})
			return
		}
		a = &album{link: l, first: now}
		h.albums[k] = a
	}
	if skip {
		a.skipped++
	} else if len(a.pieces) >= maxAlbumFiles {
		a.dropped++
	} else {
		a.pieces = append(a.pieces, p)
	}
	wait := h.o.AlbumQuiet
	if left := albumMaxWait - now.Sub(a.first); left < wait {
		wait = max(left, 0)
	}
	if a.timer != nil {
		a.timer.Stop()
	}
	detached := context.WithoutCancel(ctx) // the update that made the last part is long answered when this fires
	a.timer = time.AfterFunc(wait, func() { h.flushAlbum(detached, k, a) })
	h.mu.Unlock()
}

// flushAlbum turns a collected album into one item.
func (h *Handler) flushAlbum(parent context.Context, k albumKey, a *album) {
	h.mu.Lock()
	if h.albums[k] != a {
		h.mu.Unlock()
		return
	}
	delete(h.albums, k)
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, albumFlushLimit)
	defer cancel()
	sort.SliceStable(a.pieces, func(i, j int) bool { return a.pieces[i].id < a.pieces[j].id })
	if len(a.pieces) == 0 { // nothing in it can be carried
		h.send(ctx, a.link.ChatID, reply{text: TextNotKind})
		return
	}
	h.commit(ctx, a.link, a.pieces, a.dropped, a.skipped)
}

// --- commit ---

// commit makes one item from pieces: the space, the limits, the files, then the item, then the ack.
func (h *Handler) commit(ctx context.Context, l updates.Link, ps []piece, dropped, skipped int) {
	defer func() {
		if r := recover(); r != nil {
			h.o.Log.Error("telegram: capture panicked", slog.Any("panic", r))
			h.send(ctx, l.ChatID, reply{text: TextFailed})
		}
	}()
	c, ok := h.caller(ctx, l)
	if !ok {
		return
	}
	s, msg := h.target(ctx, c, l, "")
	if msg != "" {
		h.send(ctx, l.ChatID, reply{text: msg})
		return
	}
	var files []fileRef
	var texts, origins []string
	for _, p := range ps {
		files = append(files, p.files...)
		if p.text != "" {
			texts = append(texts, p.text)
		}
		if p.origin != "" && !contains(origins, p.origin) {
			origins = append(origins, p.origin)
		}
	}
	for _, f := range files {
		if f.size > botapi.MaxDownloadBytes {
			h.send(ctx, l.ChatID, reply{text: TextTooBig})
			return
		}
	}
	if h.o.Allow != nil && !h.o.Allow(l.ID, updates.KindInboxItem) {
		h.send(ctx, l.ChatID, reply{text: TextSlow})
		return
	}
	var ids []string
	for _, f := range files {
		if h.o.Allow != nil && !h.o.Allow(l.ID, updates.KindDownload) {
			h.send(ctx, l.ChatID, reply{text: TextSlow})
			return
		}
		id, text := h.fetchAndUpload(ctx, c, s, f)
		if text != "" {
			h.send(ctx, l.ChatID, reply{text: text})
			return
		}
		ids = append(ids, id)
	}
	text := strings.Join(texts, "\n\n")
	var b strings.Builder
	for _, o := range origins {
		b.WriteString("> " + o + "\n")
	}
	if len(origins) > 0 {
		b.WriteString("\n")
	}
	switch {
	case text != "":
		b.WriteString(text)
	case len(files) > 0:
		fmt.Fprintf(&b, "(no text; %d attachment(s) sent from Telegram)", len(files))
	}
	in := map[string]any{"body": b.String(), "title": captureTitle(text, files), "correlation_id": fmt.Sprintf("telegram:%d:%d", l.ChatID, ps[0].id)}
	if len(ids) > 0 {
		in["attachment_ids"] = ids
	}
	if !h.visible(s.ID) { // the space may have been hidden from Telegram while the files moved
		h.send(ctx, l.ChatID, reply{text: TextUnavailable})
		return
	}
	r := c.do(ctx, http.MethodPost, "/spaces/"+s.ID+"/inbox", in, map[string]string{"Idempotency-Key": fmt.Sprintf("tg-%s-%d", l.ID, ps[0].id)})
	if !r.ok() {
		h.send(ctx, l.ChatID, reply{text: createFail(r)})
		return
	}
	var it inboxItem
	if json.Unmarshal(r.body, &it) != nil || !itemIDRe.MatchString(it.ID) {
		h.send(ctx, l.ChatID, reply{text: TextFailed})
		return
	}
	h.setCurrent(ctx, l.ID, s.ID)
	h.send(ctx, l.ChatID, h.ack(s, it, len(ids), dropped, skipped))
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// captureTitle is the item's title: the first line of the text, else a note of the file.
func captureTitle(text string, files []fileRef) string {
	if t := clean(strings.SplitN(text, "\n", 2)[0], titleRunes); t != "" {
		return t
	}
	if len(files) == 1 {
		if n := clean(files[0].name, nameRunes); n != "" {
			return "File: " + n
		}
	}
	if len(files) > 1 {
		return fmt.Sprintf("%d files from Telegram", len(files))
	}
	return "Message from Telegram"
}

// ack is the answer to a capture. The title is the item's content, so it is shown only where the space's content level
// is full (as bodies are); the button opens the item's card.
func (h *Handler) ack(s spaceRef, it inboxItem, files, dropped, skipped int) reply {
	var b strings.Builder
	fmt.Fprintf(&b, "Added to the inbox of %s", s.title())
	if h.o.Space(s.ID).ContentLevel == settings.ContentFull {
		fmt.Fprintf(&b, ": %s", clean(it.Title, titleRunes))
	}
	b.WriteString(".")
	if files > 0 {
		fmt.Fprintf(&b, "\n%d attachment(s).", files)
	}
	if dropped > 0 {
		fmt.Fprintf(&b, "\n%d more file(s) were left out: an album holds at most %d.", dropped, maxAlbumFiles)
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "\n%d part(s) were left out: I can only add text, photos and files.", skipped)
	}
	return reply{text: b.String(), markup: markup([]botapi.InlineKeyboardButton{button("Open", readData(s.ID, it.ID))})}
}

// refusal is the text of a write the API refused: its own reason where there is one (size, quota, validation), else
// the generic text.
func refusal(r result) string {
	switch r.status {
	case http.StatusForbidden, http.StatusUnauthorized:
		return TextNoAccess
	case http.StatusNotFound:
		return TextUnavailable
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusRequestEntityTooLarge, http.StatusPreconditionFailed, http.StatusConflict:
		if d := r.problemDetail(); d != "" {
			return fitUnits("Not accepted: "+d, callbackAnswerUnits)
		}
		return TextFailed
	}
	return TextFailed
}

func createFail(r result) string { return refusal(r) }

// fetchAndUpload moves one file from Telegram into the space's attachments through the API's upload (size cap, content
// sniffing, quota). It returns the attachment ID, or the text to answer with.
func (h *Handler) fetchAndUpload(ctx context.Context, c caller, s spaceRef, f fileRef) (string, string) {
	b := h.bot()
	if b == nil {
		return "", TextFailed
	}
	select { // a global cap on files in memory and in flight, whatever the number of links and albums
	case h.dl <- struct{}{}:
		defer func() { <-h.dl }()
	case <-ctx.Done():
		return "", TextFailed
	}
	gctx, cancel := context.WithTimeout(ctx, apiTimeout)
	info, err := b.GetFile(gctx, f.id)
	cancel()
	if err != nil || info.FilePath == "" {
		return "", TextNoFile
	}
	if info.FileSize > botapi.MaxDownloadBytes {
		return "", TextTooBig
	}
	var buf bytes.Buffer
	dctx, cancel := context.WithTimeout(ctx, 2*apiTimeout)
	_, err = b.DownloadFile(dctx, info.FilePath, &buf)
	cancel()
	if err != nil {
		var be *botapi.Error
		if errors.As(err, &be) && strings.Contains(be.Description, "20 MB") {
			return "", TextTooBig
		}
		return "", TextNoFile
	}
	// The multipart body is streamed from the downloaded bytes: one copy of the file in memory, not two.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		hdr := make(textproto.MIMEHeader)
		hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, clean(f.name, 200)))
		hdr.Set("Content-Type", "application/octet-stream") // the API sniffs the type; the sender's claim is not used
		w, err := mw.CreatePart(hdr)
		if err == nil {
			_, err = w.Write(buf.Bytes())
		}
		if err == nil {
			err = mw.Close()
		}
		_ = pw.CloseWithError(err) // nil closes with EOF
	}()
	r := c.doRaw(ctx, http.MethodPost, "/spaces/"+s.ID+"/attachments", mw.FormDataContentType(), pr)
	_ = pr.Close() // unblocks the writer if the API stopped reading
	if !r.ok() {
		return "", refusal(r)
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(r.body, &out) != nil || out.ID == "" {
		return "", TextFailed
	}
	return out.ID, ""
}

// --- attachments to Telegram ---

// sendAttachments sends an item's first attachments back as files, with protect_content. It runs only where the body
// was shown (content level full). Each attachment's metadata is read through the API as the link's principal first;
// that read is the authorization for opening the stored bytes.
func (h *Handler) sendAttachments(ctx context.Context, c caller, l updates.Link, s spaceRef, it inboxItem) {
	b := h.bot()
	if b == nil || h.o.Content == nil || len(it.Attachments) == 0 {
		return
	}
	if h.o.Space(s.ID).ContentLevel != settings.ContentFull {
		return
	}
	sent := 0
	for _, a := range it.Attachments {
		if sent == maxSendFiles {
			h.send(ctx, l.ChatID, reply{text: fmt.Sprintf("… %d more attachment(s), open the item in the app.", len(it.Attachments)-sent)})
			return
		}
		if h.o.Allow != nil && !h.o.Allow(l.ID, updates.KindDownload) {
			h.send(ctx, l.ChatID, reply{text: TextSlow})
			return
		}
		sent++
		var meta attachmentRef
		if r := c.get(ctx, "/spaces/"+s.ID+"/attachments/"+a.ID, &meta); !r.ok() || meta.ID != a.ID {
			continue
		}
		name := clean(meta.Name, nameRunes)
		if meta.Size > maxSendBytes {
			h.send(ctx, l.ChatID, reply{text: fmt.Sprintf("%s is too large to send here. Open the item in the app.", orDash(name))})
			continue
		}
		rc, err := h.o.Content(ctx, s.ID, meta.ID)
		if err != nil {
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, 3*apiTimeout)
		f := &botapi.InputFile{Name: name, Reader: io.LimitReader(rc, maxSendBytes+1)}
		if meta.Inline && meta.Size <= maxPhotoBytes && (meta.MIME == "image/jpeg" || meta.MIME == "image/png") {
			_, err = b.SendPhoto(sctx, botapi.SendPhotoParams{ChatID: l.ChatID, MessageThreadID: threadOf(sctx), Photo: f, Caption: name, ProtectContent: true})
		} else {
			_, err = b.SendDocument(sctx, botapi.SendDocumentParams{ChatID: l.ChatID, MessageThreadID: threadOf(sctx), Document: f, Caption: name, ProtectContent: true})
		}
		cancel()
		_ = rc.Close()
		if err != nil {
			h.o.Log.Debug("telegram: an attachment was not sent", slog.String("error", err.Error()))
		}
	}
}
