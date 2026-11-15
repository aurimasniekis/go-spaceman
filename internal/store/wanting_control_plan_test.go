package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// namedQuery returns the text of a sqlc query from queries/<file>, with its sqlc.arg(x) placeholders made positional.
func namedQuery(t *testing.T, file, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("queries", file))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "-- name: "+name+" ")
	if i < 0 {
		t.Fatalf("query %s not in %s", name, file)
	}
	s = s[i:]
	s = s[strings.Index(s, "\n")+1 : strings.Index(s, ";")]
	for {
		j := strings.Index(s, "sqlc.arg(")
		if j < 0 {
			return s
		}
		k := strings.Index(s[j:], ")")
		s = s[:j] + "?" + s[j+k+1:]
	}
}

// ListSpacesWantingControl runs on every notifier sweep: each of its lookups (messages to control, waking human
// notifications, the latest control run whose instance a held response waits for, the authoritative control run) uses
// an index, and none scans its table or sorts (spaces, a small table, is scanned).
func TestListSpacesWantingControlPlan(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, Options{DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	q := namedQuery(t, "messages.sql", "ListSpacesWantingControl")
	plan := queryPlan(ctx, t, s, strings.Replace(q, "?", "5", 1))
	t.Logf("plan:\n%s", plan)
	for _, line := range strings.Split(plan, "\n") {
		if l := strings.TrimSpace(line); (strings.HasPrefix(l, "SCAN ") && l != "SCAN s") || strings.Contains(l, "TEMP B-TREE") {
			t.Errorf("unindexed step in the plan: %q\n%s", line, plan)
		}
	}
	for _, ix := range []string{"ix_human_notifications_pending", "ix_runs_space"} {
		if !strings.Contains(plan, ix) {
			t.Errorf("plan does not use %s:\n%s", ix, plan)
		}
	}
}
