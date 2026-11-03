package attachments

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var actor = audit.Actor{Type: audit.ActorUser, ID: "usr_t"}

// A 1×1 PNG.
var png = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

type fx struct {
	ctx  context.Context
	s    *store.Store
	svc  *Service
	clk  *clock.Fake
	blob string
}

func newFx(t *testing.T, o Options) *fx { t.Helper(); return newFxWith(t, o, nil) }

// newFxWith uses the blobs mk returns for the fixture's directory (nil: the local store).
func newFxWith(t *testing.T, o Options, mk func(dir string) Blobs) *fx {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))
	s := storetest.Open(t, storetest.WithClock(clk))
	kr := keyringtest.Load(t, s)
	dir := t.TempDir()
	o.Clock = clk
	var blobs Blobs = NewLocal(dir)
	if mk != nil {
		blobs = mk(dir)
	}
	f := &fx{ctx: context.Background(), s: s, svc: New(s, blobs, audit.NewWriter(kr), o), clk: clk, blob: dir}
	err := s.W().TxNamed(f.ctx, "test.seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bbb', 'B', 'nod_1', 'active', 'b', 0, 't', 0)`,
		} {
			if _, err := tx.SQL.ExecContext(f.ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func code(err error) string {
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		return ""
	}
	return string(ae.Code)
}

// SM-ATT-002/004: content is sniffed (the name never decides the type), names are cleaned, identical content is
// stored once, only raster images are inline, and an attachment is invisible from another space.
func TestUploadSniffDedupAndScope(t *testing.T) {
	f := newFx(t, Options{})
	img, err := f.svc.Upload(f.ctx, actor, "spc_a", "../../evil\x00.svg", bytes.NewReader(png))
	if err != nil {
		t.Fatal(err)
	}
	if img.MIME != "image/png" || !img.Inline || img.Name != "evil.svg" || img.Size != int64(len(png)) {
		t.Fatalf("image %+v", img)
	}
	svg, err := f.svc.Upload(f.ctx, actor, "spc_a", "pic.png", strings.NewReader(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
	if err != nil {
		t.Fatal(err)
	}
	if svg.Inline || strings.HasPrefix(svg.MIME, "image/") {
		t.Fatalf("SVG must never be inline: %+v", svg)
	}
	again, err := f.svc.Upload(f.ctx, actor, "spc_a", "copy.png", bytes.NewReader(png))
	if err != nil {
		t.Fatal(err)
	}
	if again.ID == img.ID || again.SHA256 != img.SHA256 {
		t.Fatalf("dedup %+v vs %+v", again, img)
	}
	if sums, _ := f.svc.blobs.List(f.ctx); len(sums) != 2 {
		t.Fatalf("stored contents %v, want 2", sums)
	}
	if _, err := f.svc.Get(f.ctx, "spc_b", img.ID); code(err) != "not_found" {
		t.Fatalf("cross-space read: %v", err)
	}
	a, rc, err := f.svc.Open(f.ctx, "spc_a", img.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(b, png) || a.ID != img.ID {
		t.Fatal("content mismatch")
	}
}

// SM-ATT-002: per-upload size limit and per-space quota.
func TestLimits(t *testing.T) {
	f := newFx(t, Options{MaxBytes: 100, QuotaBytes: 150})
	if _, err := f.svc.Upload(f.ctx, actor, "spc_a", "big", bytes.NewReader(make([]byte, 101))); code(err) != "validation_failed" {
		t.Fatalf("oversize upload: %v", err)
	}
	if _, err := f.svc.Upload(f.ctx, actor, "spc_a", "one", bytes.NewReader(bytes.Repeat([]byte("a"), 100))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Upload(f.ctx, actor, "spc_a", "two", bytes.NewReader(bytes.Repeat([]byte("b"), 60))); code(err) != "precondition_failed" {
		t.Fatalf("quota: %v", err)
	}
	if _, err := f.svc.Upload(f.ctx, actor, "spc_b", "other space", bytes.NewReader(bytes.Repeat([]byte("b"), 60))); err != nil {
		t.Fatalf("quota is per space: %v", err)
	}
	if _, err := f.svc.Upload(f.ctx, actor, "spc_missing", "x", strings.NewReader("x")); code(err) != "not_found" {
		t.Fatalf("missing space: %v", err)
	}
}

// SM-LIMIT-001: a space's effective storage quota (the limits model) replaces the installation quota; lowering it keeps
// what is stored and refuses new uploads.
func TestQuotaFromLimits(t *testing.T) {
	quota := int64(300)
	f := newFx(t, Options{MaxBytes: 1000, QuotaBytes: 150, QuotaFor: func(_ context.Context, _ *db.Queries, spaceID string) int64 {
		if spaceID == "spc_a" {
			return quota
		}
		return 0 // no override: the installation quota
	}})
	if _, err := f.svc.Upload(f.ctx, actor, "spc_a", "one", bytes.NewReader(bytes.Repeat([]byte("a"), 250))); err != nil {
		t.Fatalf("within the space quota: %v", err)
	}
	if _, err := f.svc.Upload(f.ctx, actor, "spc_b", "big", bytes.NewReader(bytes.Repeat([]byte("b"), 200))); code(err) != "precondition_failed" {
		t.Fatalf("installation quota: %v", err)
	}
	quota = 100
	if _, err := f.svc.Upload(f.ctx, actor, "spc_a", "two", strings.NewReader("x")); code(err) != "precondition_failed" {
		t.Fatalf("lowered quota: %v", err)
	}
}

// SM-ATT-001/006: references bind attachments to items of the same space; unreferenced uploads and their content are
// collected after the grace period, referenced ones are kept.
func TestReferencesAndGC(t *testing.T) {
	f := newFx(t, Options{})
	kept, _ := f.svc.Upload(f.ctx, actor, "spc_a", "kept.txt", strings.NewReader("kept"))
	dropped, _ := f.svc.Upload(f.ctx, actor, "spc_a", "dropped.txt", strings.NewReader("dropped"))
	other, _ := f.svc.Upload(f.ctx, actor, "spc_b", "other.txt", strings.NewReader("other"))
	err := f.s.W().TxNamed(f.ctx, "test.ref", func(tx *store.Tx) error {
		return Reference(f.ctx, tx, "spc_a", "inbox_item", "inb_1", []string{kept.ID, other.ID})
	})
	if code(err) != "validation_failed" {
		t.Fatalf("referencing another space's attachment: %v", err)
	}
	if err := f.s.W().TxNamed(f.ctx, "test.ref", func(tx *store.Tx) error {
		return Reference(f.ctx, tx, "spc_a", "inbox_item", "inb_1", []string{kept.ID})
	}); err != nil {
		t.Fatal(err)
	}
	if l, _ := f.svc.ListByOwner(f.ctx, "inbox_item", "inb_1"); len(l) != 1 || l[0].ID != kept.ID {
		t.Fatalf("owner list %+v", l)
	}
	// List views read the attachments of several owners in one query (SM-ATT-001: tasks, messages).
	if err := f.s.W().TxNamed(f.ctx, "test.ref", func(tx *store.Tx) error {
		return Reference(f.ctx, tx, "spc_a", "task", "tsk_2", []string{kept.ID, dropped.ID})
	}); err != nil {
		t.Fatal(err)
	}
	by, err := ByOwners(f.ctx, f.s.R(), "task", []string{"tsk_1", "tsk_2"})
	if err != nil || len(by["tsk_2"]) != 2 || len(by["tsk_1"]) != 0 || by["tsk_2"][0].SpaceID != "spc_a" {
		t.Fatalf("by owners %+v (%v)", by, err)
	}
	if empty, err := ByOwners(f.ctx, f.s.R(), "task", nil); err != nil || len(empty) != 0 {
		t.Fatalf("no owners %+v (%v)", empty, err)
	}
	if err := f.s.W().TxNamed(f.ctx, "test.unref", func(tx *store.Tx) error {
		_, err := tx.Q.DeleteAttachmentRefsByOwner(f.ctx, db.DeleteAttachmentRefsByOwnerParams{OwnerKind: "task", OwnerID: "tsk_2"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Within the grace period nothing goes.
	if rows, _, err := f.svc.GC(f.ctx); err != nil || rows != 0 {
		t.Fatalf("early gc removed %d (%v)", rows, err)
	}
	f.clk.Advance(UnreferencedGrace + time.Minute)
	// Content written in the last hour is protected from a concurrent upload race; age it.
	old := time.Now().Add(-2 * time.Hour)
	_ = filepath.WalkDir(f.blob, func(p string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			_ = os.Chtimes(p, old, old)
		}
		return nil
	})
	rows, blobs, err := f.svc.GC(f.ctx)
	if err != nil || rows != 2 || blobs != 2 {
		t.Fatalf("gc removed rows=%d blobs=%d (%v), want 2 and 2 (dropped, other)", rows, blobs, err)
	}
	if _, err := f.svc.Get(f.ctx, "spc_a", dropped.ID); code(err) != "not_found" {
		t.Fatal("unreferenced upload survived")
	}
	if _, rc, err := f.svc.Open(f.ctx, "spc_a", kept.ID); err != nil {
		t.Fatalf("referenced attachment lost: %v", err)
	} else {
		_ = rc.Close()
	}
}

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{"a/b/c.txt": "c.txt", `C:\x\y.doc`: "y.doc", "": "file", "..": "file", "  \"q\".md ": "q.md", "tab\there": "tabhere"} {
		if got := CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Retained event payloads (system uploads) are not audit events; people's uploads are.
func TestSystemUploadsAreNotAudited(t *testing.T) {
	f := newFx(t, Options{})
	if _, err := f.svc.Upload(f.ctx, audit.Actor{Type: audit.ActorSystem, ID: "event-ingest"}, "spc_a", "event.json", strings.NewReader(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Upload(f.ctx, actor, "spc_a", "note.txt", strings.NewReader("hi")); err != nil {
		t.Fatal(err)
	}
	rows, err := f.s.R().ListAuditPage(f.ctx, db.ListAuditPageParams{Action: sql.NullString{String: "attachment.uploaded", Valid: true}, Lim: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit rows %d (%v), want 1", len(rows), err)
	}
}
