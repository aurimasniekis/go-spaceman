package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// isRoleNameColumn matches the columns that may name a space role (ADR-0093 T-L1): role, any *_role (assignee_role,
// requester_role, successor_role, target_role, space_role, and a dummy foo_role) and any *_roles_json. The match is
// deliberately wide: a column that matches but holds something else is classified roleNameNotASpaceRole. The match sees
// only column names, so a role name stored under another column is invisible to it: messages.to_id (with to_kind='role'),
// node_commands.payload_json, limit_waits.launch_json, runs.manifest_json and inbox_items.dedup_key hold role names
// without matching, and are reviewed by hand against the rename instead of by this guard.
func isRoleNameColumn(column string) bool {
	return column == "role" || strings.HasSuffix(column, "_role") || strings.HasSuffix(column, "_roles_json")
}

// roleNameColumnsIn lists "table.column" for every role-name column of every table the migrations left behind.
func roleNameColumnsIn(ctx context.Context, d *sql.DB) ([]string, error) {
	rs, err := d.QueryContext(ctx, `SELECT m.name, p.name FROM sqlite_master m, pragma_table_info(m.name) p
		WHERE m.type = 'table' AND m.name NOT LIKE 'sqlite_%' ORDER BY m.name, p.cid`)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var table, column string
		if err := rs.Scan(&table, &column); err != nil {
			return nil, err
		}
		if isRoleNameColumn(column) {
			out = append(out, table+"."+column)
		}
	}
	return out, rs.Err()
}

// roleNameProblems returns what is wrong with the classification of cols: a role-name column it does not list, a listed
// entry no column has, a class that is not one of the three, or an entry without a reason. Nothing is wrong when empty.
func roleNameProblems(cols []string, classes map[string]roleNameEntry) []string {
	var out []string
	present := map[string]bool{}
	for _, c := range cols {
		present[c] = true
		if _, ok := classes[c]; !ok {
			out = append(out, fmt.Sprintf("column %s names a space role but is not classified: add it to roleNameColumns in internal/catalog/rename.go as %s, %s or %s, with a reason (ADR-0093 §1.2)",
				c, roleNameLive, roleNameHistory, roleNameNotASpaceRole))
		}
	}
	keys := make([]string, 0, len(classes))
	for k := range classes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		e := classes[k]
		switch {
		case !present[k]:
			out = append(out, fmt.Sprintf("roleNameColumns lists %s, which no table has: remove it from internal/catalog/rename.go", k))
		case e.class != roleNameLive && e.class != roleNameHistory && e.class != roleNameNotASpaceRole:
			out = append(out, fmt.Sprintf("%s has class %q, want %s, %s or %s", k, e.class, roleNameLive, roleNameHistory, roleNameNotASpaceRole))
		case strings.TrimSpace(e.reason) == "":
			out = append(out, fmt.Sprintf("%s has no reason", k))
		}
	}
	return out
}

// TestRoleNameColumnsAreClassified is the schema guard: every column that names a space role, in the schema after the
// migrations, is classified live, history or not-a-space-role next to RenameSpaceRole. A new column fails here until it
// is classified, so a rename cannot miss it silently.
func TestRoleNameColumnsAreClassified(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	cols, err := roleNameColumnsIn(f.ctx, f.s.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) == 0 {
		t.Fatal("found no role-name column: the schema query is broken")
	}
	for _, p := range roleNameProblems(cols, roleNameColumns) {
		t.Error(p)
	}
}

// TestRoleNameGuardFailsOnAnUnclassifiedColumn adds a column named foo_role to a migrated database and checks the guard
// reports it with a message that points at the classification. It is the red case of the guard.
func TestRoleNameGuardFailsOnAnUnclassifiedColumn(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.exec(`ALTER TABLE tasks ADD COLUMN foo_role TEXT NOT NULL DEFAULT ''`)
	cols, err := roleNameColumnsIn(f.ctx, f.s.ReadDB())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range roleNameProblems(cols, roleNameColumns) {
		if strings.Contains(p, "tasks.foo_role") {
			got = append(got, p)
		}
	}
	if len(got) != 1 || !strings.Contains(got[0], "roleNameColumns") {
		t.Fatalf("an unclassified foo_role column: want one problem naming tasks.foo_role and roleNameColumns, got %q", got)
	}
}

// TestRoleNameProblems pins the checker itself on a small table of cases, without a database.
func TestRoleNameProblems(t *testing.T) {
	t.Parallel()
	ok := roleNameEntry{roleNameLive, "reason"}
	for _, tc := range []struct {
		name    string
		cols    []string
		classes map[string]roleNameEntry
		want    int
	}{
		{"classified", []string{"a.role"}, map[string]roleNameEntry{"a.role": ok}, 0},
		{"unclassified", []string{"a.role", "b.foo_role"}, map[string]roleNameEntry{"a.role": ok}, 1},
		{"stale entry", []string{"a.role"}, map[string]roleNameEntry{"a.role": ok, "gone.role": ok}, 1},
		{"unknown class", []string{"a.role"}, map[string]roleNameEntry{"a.role": {"maybe", "reason"}}, 1},
		{"no reason", []string{"a.role"}, map[string]roleNameEntry{"a.role": {roleNameLive, " "}}, 1},
	} {
		if got := roleNameProblems(tc.cols, tc.classes); len(got) != tc.want {
			t.Errorf("%s: %d problems %q, want %d", tc.name, len(got), got, tc.want)
		}
	}
	for _, c := range []string{"role", "assignee_role", "requester_role", "successor_role", "target_role", "space_role", "foo_role", "allowed_roles_json"} {
		if !isRoleNameColumn(c) {
			t.Errorf("%s is not matched as a role-name column", c)
		}
	}
	for _, c := range []string{"roles", "name", "to_id", "roles_json", "rolex"} {
		if isRoleNameColumn(c) {
			t.Errorf("%s is matched as a role-name column", c)
		}
	}
}
