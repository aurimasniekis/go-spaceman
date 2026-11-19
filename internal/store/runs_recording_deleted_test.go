package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// RunsWithRecordingDeleted names only the given runs whose recording_deleted_at is set: not live ones, not unknown ones.
func TestRunsWithRecordingDeleted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := Open(ctx, Options{DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
	} {
		if _, err := s.wdb.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for _, r := range []struct{ id, del string }{{"run_del", "5"}, {"run_live", "NULL"}, {"run_del2", "0"}} {
		q := `INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, status, launch_json, created_at, created_by, updated_at, recording_deleted_at)
		 VALUES ('` + r.id + `', 'spc_a', 'nod_1', 1, 'worker', 'implementor', 'codex', 'ins_` + r.id + `', 'running', 'running', 'idle', '{}', 0, 't', 0, ` + r.del + `)`
		if _, err := s.wdb.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	got, err := db.New(s.rdb).RunsWithRecordingDeleted(ctx, []string{"run_del", "run_live", "run_del2", "run_none"})
	slices.Sort(got)
	if err != nil || !slices.Equal(got, []string{"run_del", "run_del2"}) {
		t.Fatalf("%v %v", got, err)
	}
}
