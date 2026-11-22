package backup_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/backup"
)

func TestSweepStaging(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "backups")
	must(t, os.MkdirAll(dir, 0o700))
	old := time.Now().Add(-2 * time.Hour)
	mk := func(name string, age time.Time) string {
		p := filepath.Join(dir, name)
		must(t, os.MkdirAll(filepath.Join(p, "sub"), 0o700))
		must(t, os.WriteFile(filepath.Join(p, "sub", "f"), []byte("x"), 0o600))
		must(t, os.Chtimes(p, age, age))
		return p
	}
	orphan := mk(".bkp_0123456789abcdefghijklmnop.tmp-123456", old)
	fresh := mk(".bkp_0123456789abcdefghijklmnoq.tmp-654321", time.Now())
	other := mk(".bkp_short.tmp-1", old)
	other2 := mk("notes.tmp-1", old)
	plain := mk("bkp_0123456789abcdefghijklmnop", old)

	// A symlink with a matching name must not be followed or removed.
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o600))
	link := filepath.Join(dir, ".bkp_0123456789abcdefghijklmnor.tmp-777")
	must(t, os.Symlink(outside, link))

	svc := f.service(backup.LocalTarget{Dir: dir}, 2)
	must(t, svc.Recover(f.ctx))
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan staging dir still there: %v", err)
	}
	for _, p := range []string{fresh, other, other2, plain, link, filepath.Join(outside, "keep")} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s must be left alone: %v", p, err)
		}
	}
	if n, err := svc.SweepStaging(f.ctx); err != nil || n != 0 {
		t.Fatalf("second sweep: %d %v", n, err)
	}
}
