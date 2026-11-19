package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// 0066 adds ix_messages_from (ADR-0099): the query for the messages a run sent searches by it, not by a scan of the
// space's messages.
func TestMessagesFromIndexUsed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := Open(ctx, Options{DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.rdb.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT count(*) FROM messages m WHERE m.space_id = 'spc_a' AND m.from_kind = 'run' AND m.from_id = 'run_1'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if p := strings.Join(plan, "; "); !strings.Contains(p, "ix_messages_from") {
		t.Fatalf("plan %q does not use ix_messages_from", p)
	}
}
