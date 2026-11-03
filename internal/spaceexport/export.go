// Package spaceexport writes a space's records and files as one archive (SM-HUI-007, SM-ATT-006, SM-SPACE-001;
// ADR-0045): inbox items, tasks with their dependencies and blockers, human actions with their responses and comments,
// messages, and attachments with their content.
//
// The archive is a ZIP of JSON (one object per line) and the attachment files. It is NOT sealed: an export exists to
// take a space's data out of SpaceMan, so it is plaintext and must be handled like the data it holds (README.txt and
// manifest.json say so). It never contains credentials, tokens or sealed fields: no table it reads holds any, and
// binary columns are left out. It is streamed straight to the requester from one read transaction (a consistent
// snapshot), so central never keeps a plaintext copy on disk. Its size is estimated before it starts and refused over
// the space's export_max_bytes limit; the stream also stops if the archive grows past it.
package spaceexport

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Format names the archive layout.
const Format = "spaceman.space-export/v1"

// Warning is shown wherever an export is offered and written into every archive.
const Warning = "A space export is NOT encrypted: it holds the space's inbox items, tasks, human actions with their responses, " +
	"messages and attachments in plain text. Store and share it like that data. It holds no credentials, tokens or sealed secrets."

const page = 500

// Content opens stored attachment content by its SHA-256.
type Content func(ctx context.Context, sha256 string) (io.ReadSeekCloser, error)

// Service writes space exports.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	content Content
	// MaxBytes returns the space's export_max_bytes limit.
	maxBytes func(ctx context.Context, spaceID string) int64
	clock    clock.Clock
}

// New returns the service.
func New(s *store.Store, aw *audit.Writer, content Content, maxBytes func(ctx context.Context, spaceID string) int64, c clock.Clock) *Service {
	if c == nil {
		c = clock.System{}
	}
	return &Service{s: s, audit: aw, content: content, maxBytes: maxBytes, clock: c}
}

// Counts are the records in an export.
type Counts struct {
	InboxItems    int64 `json:"inbox_items"`
	Tasks         int64 `json:"tasks"`
	HumanActions  int64 `json:"human_actions"`
	HumanComments int64 `json:"human_comments"`
	Messages      int64 `json:"messages"`
	Attachments   int64 `json:"attachments"`
}

// Summary describes the export a space would produce now.
type Summary struct {
	SpaceID        string `json:"space_id"`
	Format         string `json:"format"`
	Counts         Counts `json:"counts"`
	EstimatedBytes int64  `json:"estimated_bytes" doc:"Estimated uncompressed size of the archive"`
	MaxBytes       int64  `json:"max_bytes" doc:"The space's export_max_bytes limit"`
	WithinLimit    bool   `json:"within_limit"`
	Encrypted      bool   `json:"encrypted" doc:"Always false: exports are plaintext"`
	Warning        string `json:"warning"`
}

// Summarize estimates a space's export.
func (svc *Service) Summarize(ctx context.Context, spaceID string) (*Summary, error) {
	if _, err := svc.s.R().GetSpace(ctx, spaceID); err != nil {
		return nil, apperr.New(apperr.CodeNotFound, "space not found")
	}
	var out *Summary
	err := svc.s.ReadTx(ctx, func(q *db.Queries) error {
		var err error
		out, err = svc.summarize(ctx, q, spaceID)
		return err
	})
	return out, err
}

func (svc *Service) summarize(ctx context.Context, q *db.Queries, spaceID string) (*Summary, error) {
	out := &Summary{SpaceID: spaceID, Format: Format, Warning: Warning, MaxBytes: svc.maxBytes(ctx, spaceID)}
	add := func(n *int64, f func(context.Context, string) (int64, int64, error)) error {
		c, b, err := f(ctx, spaceID)
		*n += c
		out.EstimatedBytes += b
		return err
	}
	stats := []struct {
		n *int64
		f func(context.Context, string) (int64, int64, error)
	}{
		{&out.Counts.Attachments, func(ctx context.Context, id string) (int64, int64, error) {
			r, err := q.ExportAttachmentStats(ctx, id)
			return r.N, r.Bytes, err
		}},
		{&out.Counts.InboxItems, func(ctx context.Context, id string) (int64, int64, error) {
			r, err := q.ExportInboxStats(ctx, id)
			return r.N, r.Bytes, err
		}},
		{&out.Counts.Tasks, func(ctx context.Context, id string) (int64, int64, error) {
			r, err := q.ExportTaskStats(ctx, id)
			return r.N, r.Bytes, err
		}},
		{&out.Counts.HumanActions, func(ctx context.Context, id string) (int64, int64, error) {
			r, err := q.ExportHumanStats(ctx, id)
			return r.N, r.Bytes, err
		}},
		{&out.Counts.HumanComments, func(ctx context.Context, id string) (int64, int64, error) {
			r, err := q.ExportCommentStats(ctx, id)
			return r.N, r.Bytes, err
		}},
		{&out.Counts.Messages, func(ctx context.Context, id string) (int64, int64, error) {
			r, err := q.ExportMessageStats(ctx, id)
			return r.N, r.Bytes, err
		}},
	}
	for _, s := range stats {
		if err := add(s.n, s.f); err != nil {
			return nil, err
		}
	}
	out.EstimatedBytes += 64 << 10 // manifest, README and space record
	out.WithinLimit = out.EstimatedBytes <= out.MaxBytes
	return out, nil
}

// ErrTooLarge means the export is over the space's export_max_bytes limit.
var ErrTooLarge = errors.New("the export is larger than the space's export_max_bytes limit")

// File is one file in the archive, as listed in the manifest.
type File struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Manifest is the archive's manifest.json.
type Manifest struct {
	Format     string    `json:"format"`
	SpaceID    string    `json:"space_id"`
	SpaceSlug  string    `json:"space_slug"`
	ExportedAt time.Time `json:"exported_at"`
	ExportedBy string    `json:"exported_by"`
	Encrypted  bool      `json:"encrypted"`
	Warning    string    `json:"warning"`
	Times      string    `json:"times"`
	Counts     Counts    `json:"counts"`
	Files      []File    `json:"files"`
	// Missing lists attachments whose content could not be read (removed after the snapshot was taken).
	Missing []string `json:"missing_content,omitempty"`
}

// Prepare checks the space and the size limit and audits the export; the caller then streams it with Write. It
// returns the space slug (for the file name) and the summary.
func (svc *Service) Prepare(ctx context.Context, actor audit.Actor, spaceID string) (string, *Summary, error) {
	sp, err := svc.s.R().GetSpace(ctx, spaceID)
	if err != nil {
		return "", nil, apperr.New(apperr.CodeNotFound, "space not found")
	}
	sum, err := svc.Summarize(ctx, spaceID)
	if err != nil {
		return "", nil, err
	}
	if !sum.WithinLimit {
		return "", sum, apperr.New(apperr.CodePrecondition, "the export would be about %d bytes, over the space's export limit of %d bytes (limit export_max_bytes)",
			sum.EstimatedBytes, sum.MaxBytes)
	}
	err = svc.s.W().TxNamed(ctx, "export.space", func(tx *store.Tx) error {
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "space.exported", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"space_id": spaceID, "estimated_bytes": sum.EstimatedBytes, "inbox_items": sum.Counts.InboxItems, "tasks": sum.Counts.Tasks,
				"human_actions": sum.Counts.HumanActions, "messages": sum.Counts.Messages, "attachments": sum.Counts.Attachments, "encrypted": false}})
		return err
	})
	return sp.Slug, sum, err
}

// Write streams the space's export archive to w from one read transaction. It stops with ErrTooLarge when the
// archive passes max bytes (the requester then holds an incomplete archive without a manifest).
func (svc *Service) Write(ctx context.Context, w io.Writer, actor audit.Actor, spaceID string, max int64) (*Manifest, error) {
	cw := &capped{w: w, max: max}
	zw := zip.NewWriter(cw)
	m := &Manifest{Format: Format, SpaceID: spaceID, ExportedAt: svc.clock.Now().UTC(), ExportedBy: actorRef(actor), Warning: Warning,
		Times: "created_at, updated_at and the other *_at fields are Unix microseconds (UTC)", Files: []File{}}
	err := svc.s.ReadTx(ctx, func(q *db.Queries) error {
		sp, err := q.GetSpace(ctx, spaceID)
		if err != nil {
			return apperr.New(apperr.CodeNotFound, "space not found")
		}
		m.SpaceSlug = sp.Slug
		a := &archive{zw: zw, m: m}
		if err := a.text("README.txt", readme(sp.Slug)); err != nil {
			return err
		}
		if err := a.jsonFile("space.json", rowMap(sp)); err != nil {
			return err
		}
		if err := writeRows(a, "inbox_items.jsonl", &m.Counts.InboxItems, func(after string) ([]db.InboxItem, error) {
			return q.ExportInboxItems(ctx, db.ExportInboxItemsParams{SpaceID: spaceID, AfterID: after, Lim: page})
		}, func(r db.InboxItem) string { return r.ID }); err != nil {
			return err
		}
		if err := writeRows(a, "tasks.jsonl", &m.Counts.Tasks, func(after string) ([]db.Task, error) {
			return q.ExportTasks(ctx, db.ExportTasksParams{SpaceID: spaceID, AfterID: after, Lim: page})
		}, func(r db.Task) string { return r.ID }); err != nil {
			return err
		}
		deps, err := q.ExportTaskDependencies(ctx, spaceID)
		if err != nil {
			return err
		}
		if err := writeAll(a, "task_dependencies.jsonl", deps); err != nil {
			return err
		}
		blockers, err := q.ExportTaskBlockers(ctx, spaceID)
		if err != nil {
			return err
		}
		if err := writeAll(a, "task_blockers.jsonl", blockers); err != nil {
			return err
		}
		if err := writeRows(a, "human_actions.jsonl", &m.Counts.HumanActions, func(after string) ([]db.HumanAction, error) {
			return q.ExportHumanActions(ctx, db.ExportHumanActionsParams{SpaceID: spaceID, AfterID: after, Lim: page})
		}, func(r db.HumanAction) string { return r.ID }); err != nil {
			return err
		}
		if err := writeRows(a, "human_comments.jsonl", &m.Counts.HumanComments, func(after string) ([]db.HumanComment, error) {
			return q.ExportHumanComments(ctx, db.ExportHumanCommentsParams{SpaceID: spaceID, AfterID: after, Lim: page})
		}, func(r db.HumanComment) string { return r.ID }); err != nil {
			return err
		}
		if err := writeRows(a, "messages.jsonl", &m.Counts.Messages, func(after string) ([]db.Message, error) {
			return q.ExportMessages(ctx, db.ExportMessagesParams{SpaceID: spaceID, AfterID: after, Lim: page})
		}, func(r db.Message) string { return r.ID }); err != nil {
			return err
		}
		refs, err := q.ExportAttachmentRefs(ctx, spaceID)
		if err != nil {
			return err
		}
		if err := writeAll(a, "attachment_refs.jsonl", refs); err != nil {
			return err
		}
		var atts []db.Attachment
		if err := writeRows(a, "attachments.jsonl", &m.Counts.Attachments, func(after string) ([]db.Attachment, error) {
			rows, err := q.ExportAttachments(ctx, db.ExportAttachmentsParams{SpaceID: spaceID, AfterID: after, Lim: page})
			atts = append(atts, rows...)
			return rows, err
		}, func(r db.Attachment) string { return r.ID }); err != nil {
			return err
		}
		for _, at := range atts {
			if err := svc.attachment(ctx, a, at); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if cw.over {
			return nil, ErrTooLarge
		}
		return nil, err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	f, err := zw.Create("manifest.json")
	if err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = zw.Close()
	}
	if cw.over {
		return nil, ErrTooLarge
	}
	return m, err
}

func (svc *Service) attachment(ctx context.Context, a *archive, at db.Attachment) error {
	rc, err := svc.content(ctx, at.Sha256)
	if err != nil {
		a.m.Missing = append(a.m.Missing, at.ID)
		return nil //nolint:nilerr // content removed after the snapshot: listed as missing, the export continues
	}
	defer func() { _ = rc.Close() }()
	return a.file("attachments/"+at.ID, func(w io.Writer) error {
		_, err := io.Copy(w, rc)
		return err
	})
}

type archive struct {
	zw *zip.Writer
	m  *Manifest
}

// file writes one archive entry and lists it with its size and SHA-256 in the manifest.
func (a *archive) file(name string, fill func(io.Writer) error) error {
	w, err := a.zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: a.m.ExportedAt})
	if err != nil {
		return err
	}
	h := &hashed{w: w, h: sha256.New()}
	if err := fill(h); err != nil {
		return err
	}
	a.m.Files = append(a.m.Files, File{Path: name, Bytes: h.n, SHA256: hex.EncodeToString(h.h.Sum(nil))})
	return nil
}

func (a *archive) text(name, s string) error {
	return a.file(name, func(w io.Writer) error {
		_, err := io.WriteString(w, s)
		return err
	})
}

func (a *archive) jsonFile(name string, v any) error {
	return a.file(name, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	})
}

// writeRows pages through rows (keyset by ID) and writes one JSON object per line.
func writeRows[T any](a *archive, name string, n *int64, list func(after string) ([]T, error), id func(T) string) error {
	return a.file(name, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		after := ""
		for {
			rows, err := list(after)
			if err != nil {
				return err
			}
			for _, r := range rows {
				if err := enc.Encode(rowMap(r)); err != nil {
					return err
				}
				*n++
				after = id(r)
			}
			if len(rows) < page {
				return nil
			}
		}
	})
}

func writeAll[T any](a *archive, name string, rows []T) error {
	return a.file(name, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		for _, r := range rows {
			if err := enc.Encode(rowMap(r)); err != nil {
				return err
			}
		}
		return nil
	})
}

// rowMap turns a database row into a JSON object with the columns' names: NULLs become null, *_json columns are
// embedded as JSON under the name without the suffix, and binary columns (sealed or hashed values) are left out.
func rowMap(row any) map[string]any {
	out := map[string]any{}
	v := reflect.ValueOf(row)
	t := v.Type()
	for i := range t.NumField() {
		f, fv := t.Field(i), v.Field(i)
		if !f.IsExported() {
			continue
		}
		name := snake(f.Name)
		switch x := fv.Interface().(type) {
		case []byte:
			continue
		case sql.NullString:
			if x.Valid {
				out[name] = x.String
			} else {
				out[name] = nil
			}
		case sql.NullInt64:
			if x.Valid {
				out[name] = x.Int64
			} else {
				out[name] = nil
			}
		case string:
			if base, ok := strings.CutSuffix(name, "_json"); ok && json.Valid([]byte(x)) && x != "" {
				out[base] = json.RawMessage(x)
				continue
			}
			out[name] = x
		default:
			out[name] = x
		}
	}
	return out
}

// snake converts a generated Go field name back to its column name: SpaceID → space_id, TrustedJson → trusted_json.
func snake(s string) string {
	r := []rune(s)
	var b strings.Builder
	for i, c := range r {
		if unicode.IsUpper(c) && i > 0 {
			prev := r[i-1]
			nextLower := i+1 < len(r) && unicode.IsLower(r[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(c))
	}
	return b.String()
}

func readme(slug string) string {
	return fmt.Sprintf(`SpaceMan space export (%s) of space %q

%s

Layout
  manifest.json            format, counts, and every file with its size and SHA-256
  space.json               the space
  inbox_items.jsonl        inbox items (one JSON object per line)
  tasks.jsonl              queue tasks; task_dependencies.jsonl and task_blockers.jsonl relate them
  human_actions.jsonl      human actions with their requests and responses; human_comments.jsonl their comments
  messages.jsonl           messages between agents, control and operators
  attachments.jsonl        attachment metadata; attachment_refs.jsonl says which item uses which
  attachments/<id>         attachment content

Times are Unix microseconds (UTC). Bodies and attachments are untrusted content as SpaceMan received it.
`, Format, slug, Warning)
}

func actorRef(a audit.Actor) string {
	if a.ID == "" {
		return a.Type
	}
	return a.Type + ":" + a.ID
}

// capped stops writing once max bytes have been written.
type capped struct {
	w    io.Writer
	max  int64
	n    int64
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.n+int64(len(p)) > c.max {
		c.over = true
		return 0, ErrTooLarge
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

type hashed struct {
	w io.Writer
	h hash.Hash
	n int64
}

func (h *hashed) Write(p []byte) (int, error) {
	n, err := h.w.Write(p)
	h.h.Write(p[:n])
	h.n += int64(n)
	return n, err
}
