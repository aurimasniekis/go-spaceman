package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// 0029 adds space repositories and registers every folder a knowledge base names as a listed repository (role kb,
// manual), once per space and folder, with a suffix when the derived name is taken; the knowledge bases are unchanged.
func TestSpaceRepositoriesMigrationKeepsKBs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(ctx, Options{DataDir: dir, Migrations: embeddedUpTo(t, "0028")})
	if err != nil {
		t.Fatal(err)
	}
	kb := func(id, space, slug, repo, branch string) string {
		return `INSERT INTO kb_repos (id, space_id, slug, name, repo, base_branch, space_role, created_at, created_by, updated_at, version)
			VALUES ('` + id + `', '` + space + `', '` + slug + `', 'N', '` + repo + `', '` + branch + `', 'kb', 5, 't', 6, 4)`
	}
	for _, q := range []string{
		`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bbb', 'B', 'nod_1', 'active', 'b', 0, 't', 0)`,
		kb("kbr_aaaaaaaaaaaaaaaaaaaaaaaaa1", "spc_a", "docs", "kb/Docs", "trunk"),
		kb("kbr_aaaaaaaaaaaaaaaaaaaaaaaaa2", "spc_a", "docs2", "kb/Docs", "main"),
		kb("kbr_aaaaaaaaaaaaaaaaaaaaaaaaa3", "spc_a", "flat", "kb-docs", "main"),
		kb("kbr_aaaaaaaaaaaaaaaaaaaaaaaaa4", "spc_b", "docs", "kb/Docs", "main"),
	} {
		if _, err := s.wdb.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = s.Close()

	s, err = Open(ctx, Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, space_id, name, path, role, clone_policy, url, default_branch FROM space_repositories ORDER BY space_id, path`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, space, name, path, role, policy, url, branch string
		if err := rows.Scan(&id, &space, &name, &path, &role, &policy, &url, &branch); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(id, "rep_") || len(id) != 30 || role != "kb" || policy != "manual" || url != "" {
			t.Fatalf("repository %s %s %s %s %s", id, role, policy, url, branch)
		}
		got = append(got, space+" "+name+" "+path+" "+branch)
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	// spc_a: "kb-docs" (from kb-docs) and "kb/Docs" → "kb-docs" collide; one of them gets a suffix. spc_b has its own.
	if len(got) != 3 || got[2] != "spc_b kb-docs kb/Docs main" {
		t.Fatalf("repositories %q", got)
	}
	names := map[string]bool{}
	for _, g := range got[:2] {
		names[strings.Fields(g)[1]] = true
	}
	if !names["kb-docs"] || len(names) != 2 {
		t.Fatalf("names %q", got)
	}
	var repo string
	var version int64
	if err := s.rdb.QueryRowContext(ctx, `SELECT repo, version FROM kb_repos WHERE id = 'kbr_aaaaaaaaaaaaaaaaaaaaaaaaa1'`).Scan(&repo, &version); err != nil ||
		repo != "kb/Docs" || version != 4 {
		t.Fatalf("knowledge base after migration: %q %d %v", repo, version, err)
	}
	if _, err := s.wdb.ExecContext(ctx, `INSERT INTO space_repositories (id, space_id, name, path, url, clone_policy, created_at, created_by, updated_at, updated_by)
		VALUES ('rep_x', 'spc_a', 'x', 'x', '', 'auto', 0, 't', 0, 't')`); err == nil {
		t.Fatal("an auto repository without a URL was accepted")
	}
}
