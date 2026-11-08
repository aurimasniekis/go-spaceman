package store

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 0045 adds a partial index over live runs, and the System page's runs_running count (queries/system.sql) uses it
// instead of scanning an index over every run (SM-OBS-006). The count's text is read from the query file, so a change
// to its IN list that the index no longer matches fails here.
func TestSystemCountsUsesLiveRunsIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := Open(ctx, Options{DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	src, err := os.ReadFile("queries/system.sql")
	if err != nil {
		t.Fatal(err)
	}
	q := regexp.MustCompile(`SELECT count\(\*\) FROM runs WHERE [^)]*\)`).Find(src)
	if q == nil {
		t.Fatalf("no runs count in queries/system.sql:\n%s", src)
	}
	if plan := queryPlan(ctx, t, s, string(q)); !strings.Contains(plan, "ix_runs_observed_live") {
		t.Fatalf("%s\ndoes not use the partial index:\n%s", q, plan)
	}
}
