// Package attachments stores files that people and agents attach to inbox items, tasks, human actions, comments and
// messages (SM-ATT-001…006, ADR-0013). Content is content-addressed in a blob store; each upload is a row in its space,
// counted against the space's quota and referenced by the items that use it. Unreferenced uploads are removed after a
// grace period, and content is removed once no row points to it.
package attachments

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// URIScheme prefixes attachment references in Markdown bodies: spaceman://attachment/<id>.
const URIScheme = "spaceman://attachment/"

// UnreferencedGrace is how long an upload may stay unreferenced before garbage collection removes it.
const UnreferencedGrace = 24 * time.Hour

// Attachment is one uploaded file.
type Attachment struct {
	ID         string    `json:"id"`
	SpaceID    string    `json:"space_id"`
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	MIME       string    `json:"mime" doc:"Media type sniffed from the content (never the uploader's claim)"`
	SHA256     string    `json:"sha256"`
	UploadedBy string    `json:"uploaded_by"`
	CreatedAt  time.Time `json:"created_at"`
	// Inline reports whether browsers may display it inline (raster images only); everything else is downloaded.
	Inline bool `json:"inline"`
}

// Options configure the service.
type Options struct {
	MaxBytes   int64 // per upload
	QuotaBytes int64 // per space
	// QuotaFor returns a space's effective quota inside the upload transaction (the limits model, ADR-0036); nil or a
	// non-positive result uses QuotaBytes.
	QuotaFor func(ctx context.Context, q *db.Queries, spaceID string) int64
	Clock    clock.Clock
}

// Service manages attachments.
type Service struct {
	s     *store.Store
	blobs Blobs
	audit *audit.Writer
	o     Options
}

// New returns the service.
func New(s *store.Store, blobs Blobs, aw *audit.Writer, o Options) *Service {
	if o.MaxBytes <= 0 {
		o.MaxBytes = 25 << 20
	}
	if o.QuotaBytes <= 0 {
		o.QuotaBytes = 1 << 30
	}
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	return &Service{s: s, blobs: blobs, audit: aw, o: o}
}

// MaxBytes is the per-upload limit.
func (svc *Service) MaxBytes() int64 { return svc.o.MaxBytes }

var errNotFound = apperr.New(apperr.CodeNotFound, "attachment not found")

// inlineTypes are the only types served inline (ADR-0013): raster images. SVG and HTML are always downloads.
var inlineTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// InlineAllowed reports whether a sniffed media type may be displayed inline.
func InlineAllowed(mime string) bool { return inlineTypes[mime] }

// CleanName makes an uploaded file name safe to store and to put in a Content-Disposition header.
func CleanName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '"' || r == '/' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	if r := []rune(name); len(r) > 200 {
		name = string(r[:200])
	}
	return name
}

// Upload stores one file in a space. The content is written before the transaction (no I/O inside it); a rejected
// upload leaves unreferenced content that garbage collection removes.
func (svc *Service) Upload(ctx context.Context, actor audit.Actor, spaceID, name string, r io.Reader) (*Attachment, error) {
	sum, size, head, err := svc.blobs.Write(ctx, r, svc.o.MaxBytes)
	if errors.Is(err, ErrTooLarge) {
		return nil, apperr.New(apperr.CodeInvalid, "attachments are limited to %d bytes", svc.o.MaxBytes)
	} else if err != nil {
		return nil, err
	}
	mime := http.DetectContentType(head)
	if i := strings.IndexByte(mime, ';'); i > 0 && !strings.HasPrefix(mime, "text/") {
		mime = mime[:i]
	}
	a := &Attachment{ID: ids.New(ids.Attachment), SpaceID: spaceID, Name: CleanName(name), Size: size, MIME: mime, SHA256: sum,
		UploadedBy: actor.ID, Inline: InlineAllowed(mime)}
	err = svc.s.W().TxNamed(ctx, "attachments.upload", func(tx *store.Tx) error {
		sp, err := tx.Q.GetSpace(ctx, spaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return apperr.New(apperr.CodeNotFound, "space not found")
		} else if err != nil {
			return err
		}
		if sp.State == "archived" {
			return apperr.New(apperr.CodePrecondition, "the space is archived")
		}
		used, err := tx.Q.SpaceAttachmentBytes(ctx, spaceID)
		if err != nil {
			return err
		}
		quota := svc.o.QuotaBytes
		if svc.o.QuotaFor != nil {
			if n := svc.o.QuotaFor(ctx, tx.Q, spaceID); n > 0 {
				quota = n
			}
		}
		if used+size > quota {
			return apperr.New(apperr.CodePrecondition, "the space's attachment quota (%d bytes) is full", quota)
		}
		a.CreatedAt = tx.Now
		if err := tx.Q.InsertAttachment(ctx, db.InsertAttachmentParams{ID: a.ID, SpaceID: spaceID, Sha256: sum, Size: size, Mime: mime,
			Name: a.Name, UploadedBy: actor.ID, CreatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if actor.Type == audit.ActorSystem {
			return nil // retained event payloads: the delivery history records them, the audit log stays about people
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "attachment.uploaded", Target: audit.Target{Type: "attachment", ID: a.ID},
			Details: map[string]any{"space_id": spaceID, "name": a.Name, "size": size, "mime": mime}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

func fromRow(r db.Attachment) *Attachment {
	return &Attachment{ID: r.ID, SpaceID: r.SpaceID, Name: r.Name, Size: r.Size, MIME: r.Mime, SHA256: r.Sha256, UploadedBy: r.UploadedBy,
		CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), Inline: InlineAllowed(r.Mime)}
}

// Get returns an attachment of a space (an attachment of another space is not found).
func (svc *Service) Get(ctx context.Context, spaceID, id string) (*Attachment, error) {
	r, err := svc.s.R().GetAttachment(ctx, id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && r.SpaceID != spaceID) {
		return nil, errNotFound
	} else if err != nil {
		return nil, err
	}
	return fromRow(r), nil
}

// Open returns an attachment and its content.
func (svc *Service) Open(ctx context.Context, spaceID, id string) (*Attachment, io.ReadSeekCloser, error) {
	a, err := svc.Get(ctx, spaceID, id)
	if err != nil {
		return nil, nil, err
	}
	rc, err := svc.blobs.Open(ctx, a.SHA256)
	if err != nil {
		return nil, nil, err
	}
	return a, rc, nil
}

// Reference records, inside the caller's transaction, that an item uses attachments. Every attachment must belong to the
// item's space.
func Reference(ctx context.Context, tx *store.Tx, spaceID, ownerKind, ownerID string, attachmentIDs []string) error {
	for _, id := range attachmentIDs {
		r, err := tx.Q.GetAttachment(ctx, id)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && r.SpaceID != spaceID) {
			return apperr.New(apperr.CodeInvalid, "attachment %q is not an attachment of this space", id)
		} else if err != nil {
			return err
		}
		if err := tx.Q.InsertAttachmentRef(ctx, db.InsertAttachmentRefParams{AttachmentID: id, OwnerKind: ownerKind, OwnerID: ownerID,
			CreatedAt: tx.NowMicros()}); err != nil {
			return err
		}
	}
	return nil
}

// ListByOwner returns the attachments an item references.
func (svc *Service) ListByOwner(ctx context.Context, ownerKind, ownerID string) ([]*Attachment, error) {
	rows, err := svc.s.R().ListAttachmentsByOwner(ctx, db.ListAttachmentsByOwnerParams{OwnerKind: ownerKind, OwnerID: ownerID})
	if err != nil {
		return nil, err
	}
	out := make([]*Attachment, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromRow(r))
	}
	return out, nil
}

// GC removes uploads that nothing referenced within UnreferencedGrace, then content no upload points to. Content
// touched within the last hour is kept (a concurrent upload of the same bytes).
func (svc *Service) GC(ctx context.Context) (removedRows int64, removedBlobs int, err error) {
	now := svc.o.Clock.Now()
	err = svc.s.W().TxNamed(ctx, "attachments.gc", func(tx *store.Tx) error {
		removedRows, err = tx.Q.DeleteUnreferencedAttachments(ctx, now.Add(-UnreferencedGrace).UnixMicro())
		return err
	})
	if err != nil {
		return 0, 0, err
	}
	sums, err := svc.blobs.List(ctx)
	if err != nil {
		return removedRows, 0, err
	}
	for _, sum := range sums {
		n, err := svc.s.R().CountAttachmentsBySHA(ctx, sum)
		if err != nil {
			return removedRows, removedBlobs, err
		}
		if n > 0 {
			continue
		}
		if err := svc.blobs.Remove(ctx, sum, time.Now().Add(-time.Hour)); err != nil {
			return removedRows, removedBlobs, err
		}
		removedBlobs++
	}
	return removedRows, removedBlobs, nil
}
