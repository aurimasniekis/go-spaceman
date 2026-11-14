package agent

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// The journal migrates through store.Open, so a journal migration lower than the journal's version that was never
// applied (0006 missing under 0007) refuses the open with the file named instead of being skipped silently.
func TestJournalMissingLowerMigrationRefused(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "journal")
	j, err := OpenJournal(ctx, dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = j.Close()

	d, err := sql.Open("sqlite", filepath.Join(dir, store.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, "DELETE FROM goose_db_version WHERE version_id = 6"); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()

	_, err = OpenJournal(ctx, dir, nil, nil)
	if !errors.Is(err, store.ErrMissingMigrations) || !strings.Contains(err.Error(), "0006 (0006_harness_version.sql)") {
		t.Fatalf("want ErrMissingMigrations naming 0006, got %v", err)
	}
}
