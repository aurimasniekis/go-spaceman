package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// row is one seeded row: where it is and what a rename of a to z in spc_a must leave in it (want; nil: unchanged).
type row struct {
	table string
	key   map[string]any
	want  map[string]any
}

// seeder inserts rows filling every required column the test does not care about: the first value a CHECK allows,
// otherwise a placeholder text or 0.
type seeder struct {
	t    *testing.T
	ctx  context.Context
	tx   *store.Tx
	cols map[string][]colInfo
	n    int
}

type colInfo struct {
	name, typ, enum string
	notNull, dflt   bool
	pk              int
}

func (s *seeder) columns(table string) []colInfo {
	if c, ok := s.cols[table]; ok {
		return c
	}
	var ddl string
	if err := s.tx.SQL.QueryRowContext(s.ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&ddl); err != nil {
		s.t.Fatalf("table %s: %v", table, err)
	}
	rs, err := s.tx.SQL.QueryContext(s.ctx, `SELECT name, type, "notnull", dflt_value IS NOT NULL, pk FROM pragma_table_info(?)`, table)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rs.Close()
	var out []colInfo
	for rs.Next() {
		var c colInfo
		var nn, d int
		if err := rs.Scan(&c.name, &c.typ, &nn, &d, &c.pk); err != nil {
			s.t.Fatal(err)
		}
		c.notNull, c.dflt = nn == 1, d == 1
		if m := regexp.MustCompile(`CHECK \(` + c.name + ` IN \('([^']*)'`).FindStringSubmatch(ddl); m != nil {
			c.enum = m[1]
		}
		out = append(out, c)
	}
	if err := rs.Err(); err != nil {
		s.t.Fatal(err)
	}
	s.cols[table] = out
	return out
}

func (s *seeder) ins(table string, vals map[string]any) map[string]any {
	s.t.Helper()
	s.n++
	var names, marks []string
	var args []any
	key := map[string]any{}
	for _, c := range s.columns(table) {
		v, ok := vals[c.name]
		if !ok {
			if !c.notNull || c.dflt {
				continue
			}
			switch {
			case c.enum != "":
				v = c.enum
			case c.typ == "INTEGER":
				v = int64(s.n)
			case c.typ == "BLOB":
				v = []byte{1}
			default:
				v = fmt.Sprintf("%s_%d", c.name, s.n)
			}
		}
		names, marks, args = append(names, c.name), append(marks, "?"), append(args, v)
		if c.pk > 0 {
			key[c.name] = v
		}
	}
	q := fmt.Sprintf("INSERT INTO %q (%s) VALUES (%s)", table, strings.Join(names, ", "), strings.Join(marks, ", "))
	if _, err := s.tx.SQL.ExecContext(s.ctx, q, args...); err != nil {
		s.t.Fatalf("%s: %v (%v)", q, err, args)
	}
	return key
}

func where(key map[string]any) (string, []any) {
	var parts []string
	var args []any
	names := make([]string, 0, len(key))
	for k := range key {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		parts, args = append(parts, k+" = ?"), append(args, key[k])
	}
	return strings.Join(parts, " AND "), args
}

// snapshot reads a seeded row as column → value.
func snapshot(t *testing.T, ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, r row) map[string]any {
	t.Helper()
	w, args := where(r.key)
	rs, err := q.QueryContext(ctx, fmt.Sprintf("SELECT * FROM %q WHERE %s", r.table, w), args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	cols, _ := rs.Columns()
	if !rs.Next() {
		if err := rs.Err(); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rs.Scan(ptrs...); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for i, c := range cols {
		if b, ok := vals[i].([]byte); ok {
			vals[i] = string(b)
		}
		out[c] = vals[i]
	}
	return out
}

// renameFx is the catalog fixture with two spaces, each with roles a and other (spc_a also has c), every kind of row
// that names a role seeded for each, and the expected effect of renaming a to z in spc_a.
type renameFx struct {
	*fx
	rows []row
}

func newRenameFx(t *testing.T) *renameFx {
	t.Helper()
	f := &renameFx{fx: newFx(t)}
	// One transaction per role keeps each under the write-lock guard with the race detector on (every row type of a
	// role is seeded, and the seeder reads each table's columns).
	s := &seeder{t: t, ctx: f.ctx, cols: map[string][]colInfo{}}
	seed := func(fn func()) {
		must(t, f.s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
			s.tx = tx
			fn()
			return nil
		}))
	}
	seed(func() {
		s.ins("spaces", map[string]any{"id": "spc_b", "slug": "bbb", "name": "B", "node_id": "nod_1", "state": "active", "workdir": "b"})
		s.ins("role_templates", map[string]any{"id": "rtp_1", "slug": "worker", "kind": "worker"})
		s.ins("mcp_servers", map[string]any{"id": "mcp_1", "slug": "srv"})
		s.ins("accounts", map[string]any{"id": "acc_1", "name": "acct"})
	})
	for _, sp := range []string{"spc_a", "spc_b"} {
		for _, r := range []string{"a", "other"} {
			seed(func() { f.seedRole(s, sp, r, sp == "spc_a" && r == "a") })
		}
	}
	seed(func() { s.ins("space_roles", map[string]any{"space_id": "spc_a", "name": "c", "template_id": "rtp_1"}) })
	must(t, f.s.W().TxNamed(f.ctx, "seed.audit", func(tx *store.Tx) error {
		id, err := f.svc.audit.Append(f.ctx, tx, audit.Event{Actor: op, Action: "space.role_saved", Target: audit.Target{Type: "space", ID: "spc_a"},
			Details: map[string]any{"role": "a"}})
		f.rows = append(f.rows, row{table: "audit_events", key: map[string]any{"id": id}})
		return err
	}))
	return f
}

// exec runs one statement in a write transaction.
func (f *fx) exec(q string, args ...any) {
	f.t.Helper()
	must(f.t, f.s.W().TxNamed(f.ctx, "test.exec", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, q, args...)
		return err
	}))
}

func dbRole(space, name string) db.GetSpaceRoleParams {
	return db.GetSpaceRoleParams{SpaceID: space, Name: name}
}

// seedRole seeds every row type of ADR-0093's list for one role of one space. live: the rows the rename rewrites get
// their expected new values.
func (f *renameFx) seedRole(s *seeder, sp, r string, live bool) {
	tag := sp + "_" + r
	add := func(table string, vals map[string]any, want map[string]any) {
		k := s.ins(table, vals)
		if !live {
			want = nil
		}
		f.rows = append(f.rows, row{table: table, key: k, want: want})
	}
	z := "z"
	list := func(l ...string) string { b, _ := json.Marshal(l); return string(b) }
	s.ins("space_roles", map[string]any{"space_id": sp, "name": r, "template_id": "rtp_1"})

	// Live configuration.
	add("mcp_grants", map[string]any{"id": "mcg_" + tag, "server_id": "mcp_1", "space_id": sp, "role": r}, map[string]any{"role": z})
	add("kb_repos", map[string]any{"id": "kbr_" + tag, "space_id": sp, "slug": tag, "space_role": r, "allowed_roles_json": list("c", r, "*", "z")},
		map[string]any{"space_role": z, "allowed_roles_json": list("c", "z", "*"), "version": int64(3)})
	add("dream_configs", map[string]any{"id": "drm_" + tag, "space_id": sp, "slug": tag, "space_role": r, "memory_roles_json": list(r),
		"transcript_roles_json": list("*", r)},
		map[string]any{"space_role": z, "memory_roles_json": list("z"), "transcript_roles_json": list("*", "z"), "version": int64(3)})
	add("space_schedules", map[string]any{"id": "sch_" + tag, "space_id": sp, "name": tag, "assignee_role": r},
		map[string]any{"assignee_role": z, "version": int64(2)})
	action := map[string]string{"a": "question", "other": "todo"}[r]
	add("human_policies", map[string]any{"id": "hpl_" + tag, "space_id": sp, "action_type": action, "successor_role": r},
		map[string]any{"successor_role": z, "version": int64(2), "updated_by": op.ID})
	add("space_human_type_permissions", map[string]any{"space_id": sp, "action_type": "t_" + tag, "agents": "roles", "agent_roles_json": list(r, "c")},
		map[string]any{"agent_roles_json": list("z", "c"), "version": int64(2), "updated_by": op.ID})
	f.rows = append(f.rows, row{table: "space_role_accounts", key: s.ins("space_role_accounts", map[string]any{"space_id": sp, "role": r, "account_id": "acc_1"})})
	if live { // the key holds the role: the renamed row is found under the new name
		f.rows[len(f.rows)-1] = row{table: "space_role_accounts", key: map[string]any{"space_id": sp, "role": z, "account_id": "acc_1"}, want: map[string]any{"role": z}}
	}
	add("memory_collections", map[string]any{"id": "mem_" + tag, "space_id": sp, "role": r}, map[string]any{"role": z})

	// Open work, live until it closes.
	for _, st := range []string{"backlog", "ready", "running", "blocked", "review"} {
		add("tasks", map[string]any{"id": "tsk_" + tag + "_" + st, "space_id": sp, "status": st, "assignee_role": r},
			map[string]any{"assignee_role": z, "version": int64(2)})
	}
	for _, st := range []string{"done", "failed", "cancelled"} {
		add("tasks", map[string]any{"id": "tsk_" + tag + "_" + st, "space_id": sp, "status": st, "assignee_role": r}, nil)
	}
	add("messages", map[string]any{"id": "msg_" + tag + "_stored", "space_id": sp, "to_kind": "role", "to_id": r, "state": "stored", "deliver_at": int64(1) << 60},
		map[string]any{"to_id": z, "version": int64(2)})
	add("messages", map[string]any{"id": "msg_" + tag + "_available", "space_id": sp, "to_kind": "role", "to_id": r, "state": "available"},
		map[string]any{"to_id": z, "version": int64(2)})
	for _, st := range []string{"acknowledged", "resolved"} {
		add("messages", map[string]any{"id": "msg_" + tag + "_" + st, "space_id": sp, "to_kind": "role", "to_id": r, "state": st}, nil)
	}
	// A message from the role keeps its label.
	add("messages", map[string]any{"id": "msg_" + tag + "_from", "space_id": sp, "from_kind": "run", "from_label": r, "to_kind": "control", "state": "stored"}, nil)
	for _, st := range []string{"queued", "dispatched"} {
		add("kb_requests", map[string]any{"id": "kbq_" + tag + "_" + st, "kb_id": "kbr_" + tag, "space_id": sp, "requester_role": r, "state": st},
			map[string]any{"requester_role": z})
	}
	// A request from another space carries a sentinel requester role, never a role of this space.
	add("kb_requests", map[string]any{"id": "kbq_" + tag + "_xspace", "kb_id": "kbr_" + tag, "space_id": sp, "requester_role": r, "state": "queued",
		"xspace_request_id": "xsr_" + tag}, nil)
	for _, st := range []string{"answered", "failed", "cancelled", "timed_out"} {
		add("kb_requests", map[string]any{"id": "kbq_" + tag + "_" + st, "kb_id": "kbr_" + tag, "space_id": sp, "requester_role": r, "state": st}, nil)
	}
	add("human_actions", map[string]any{"id": "hum_" + tag, "space_id": sp, "requester_kind": "agent", "requester_role": r}, nil)
	add("human_notifications", map[string]any{"id": "hnt_" + tag + "_pending", "action_id": "hum_" + tag, "space_id": sp, "target_role": r, "state": "pending"},
		map[string]any{"target_role": z})
	for _, st := range []string{"delivered", "acknowledged"} {
		add("human_notifications", map[string]any{"id": "hnt_" + tag + "_" + st, "action_id": "hum_" + tag, "space_id": sp, "target_role": r, "state": st}, nil)
	}

	// History.
	launch := fmt.Sprintf(`{"role":%q}`, r)
	add("runs", map[string]any{"id": "run_" + tag, "space_id": sp, "node_id": "nod_1", "kind": "worker", "role": r, "desired": "running", "observed": "exited", "launch_json": launch,
		"manifest_json": fmt.Sprintf(`{"role":%q}`, r)}, nil)
	add("run_usage", map[string]any{"run_id": "run_" + tag, "space_id": sp, "role": r}, nil)
	add("usage_buckets", map[string]any{"space_id": sp, "account_id": "acc_1", "role": r}, nil)
	add("limit_waits", map[string]any{"id": "lwt_" + tag, "space_id": sp, "run_id": "run_" + tag, "role": r, "launch_json": launch, "state": "resumed"}, nil)
	add("account_switches", map[string]any{"id": "asw_" + tag, "space_id": sp, "role": r, "state": "succeeded"}, nil)
	add("node_commands", map[string]any{"id": "cmd_" + tag, "node_id": "nod_1", "space_id": sp, "kind": "run.launch", "payload_json": launch, "state": "accepted"}, nil)
	add("approvals", map[string]any{"id": "apv_" + tag, "space_id": sp, "requester_role": r}, nil)
	add("xspace_requests", map[string]any{"id": "xsr_" + tag, "source_space_id": sp, "dest_space_id": sp, "requester_role": r}, nil)
	add("kb_sessions", map[string]any{"id": "kbs_" + tag, "kb_id": "kbr_" + tag, "space_id": sp, "requester_role": r}, nil)
	add("inbox_items", map[string]any{"id": "inb_" + tag, "space_id": sp, "dedup_key": "role:" + r + ":x", "source_label": r}, nil)
	add("event_outbox", map[string]any{"id": "evt_" + tag, "payload_json": fmt.Sprintf(`{"space_id":%q,"role":%q}`, sp, r)}, nil)
}

func (f *renameFx) snapshots(q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) []map[string]any {
	out := make([]map[string]any, len(f.rows))
	for i, r := range f.rows {
		k := r
		if r.want != nil && r.table == "space_role_accounts" {
			continue // read after the rename under its new key
		}
		out[i] = snapshot(f.t, f.ctx, q, k)
	}
	return out
}

// TestRenameSpaceRoleRewritesLiveRows renames a to z in spc_a: every live reference in spc_a follows, and history,
// closed work, the other role and the other space keep what they had.
func TestRenameSpaceRoleRewritesLiveRows(t *testing.T) {
	f := newRenameFx(t)
	before := f.snapshots(f.s.ReadDB())
	revision := f.role("spc_a", "a").Revision

	role, rn, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "a", "z", revision)
	must(t, err)
	if role.Name != "z" || role.Revision != revision+1 || role.TemplateID != "rtp_1" {
		t.Errorf("renamed role %+v", role)
	}
	if rn.From != "a" || rn.To != "z" || rn.NodeState != RenameNodePending || rn.NodeID != "nod_1" || rn.CentralMovedAt != nil || rn.RenamedBy != op.ID {
		t.Errorf("rename %+v", rn)
	}
	if _, err := f.svc.s.R().GetSpaceRole(f.ctx, dbRole("spc_a", "a")); err == nil {
		t.Error("role a still exists in spc_a")
	}
	if f.role("spc_b", "a").Name != "a" || f.role("spc_a", "other").Revision != 1 {
		t.Error("another space's role or another role changed")
	}

	after := f.snapshots(f.s.ReadDB())
	for i, r := range f.rows {
		if r.want != nil && r.table == "space_role_accounts" {
			if snapshot(t, f.ctx, f.s.ReadDB(), r) == nil {
				t.Errorf("space_role_accounts: no pool entry for z")
			}
			if old := snapshot(t, f.ctx, f.s.ReadDB(), row{table: r.table, key: map[string]any{"space_id": "spc_a", "role": "a", "account_id": "acc_1"}}); old != nil {
				t.Errorf("space_role_accounts: the pool entry for a is still there")
			}
			continue
		}
		want := map[string]any{}
		for k, v := range before[i] {
			want[k] = v
		}
		for k, v := range r.want {
			want[k] = v
		}
		got := after[i]
		if _, ok := got["updated_at"]; ok && r.want != nil {
			want["updated_at"] = got["updated_at"] // a rewritten row also gets a new updated_at
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s %v:\n got %v\nwant %v", r.table, r.key, got, want)
		}
	}

	// The audit event and the change feed.
	var details string
	must(t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT details_json FROM audit_events WHERE action = 'space.role_renamed'`).Scan(&details))
	var d struct {
		From, To  string
		RenameID  string `json:"rename_id"`
		Rewritten map[string]int64
	}
	must(t, json.Unmarshal([]byte(details), &d))
	wantCounts := map[string]int64{"mcp_grants": 1, "kb_repos.space_role": 1, "kb_repos.allowed_roles_json": 1, "dream_configs.space_role": 1,
		"dream_configs.roles_json": 1, "space_schedules": 1, "human_policies": 1, "space_human_type_permissions": 1, "space_role_accounts": 1,
		"memory_collections": 1, "tasks": 5, "messages": 2, "kb_requests": 2, "human_notifications": 1}
	if d.From != "a" || d.To != "z" || d.RenameID != rn.ID || !reflect.DeepEqual(d.Rewritten, wantCounts) {
		t.Errorf("audit details %s", details)
	}
	changes := map[string]int{}
	rs, err := f.s.ReadDB().QueryContext(f.ctx, `SELECT resource_type, resource_id FROM changes WHERE space_id = 'spc_a' AND action = 'updated'`)
	must(t, err)
	defer rs.Close()
	for rs.Next() {
		var typ, id string
		must(t, rs.Scan(&typ, &id))
		changes[typ+":"+id]++
	}
	must(t, rs.Err())
	for _, c := range []string{"space:spc_a", "mcp_server:mcp_1", "kb:kbr_spc_a_a", "dream:drm_spc_a_a", "schedule:sch_spc_a_a", "human_policy:hpl_spc_a_a",
		"memory:mem_spc_a_a", "task:tsk_spc_a_a_ready", "message:msg_spc_a_a_stored"} {
		if changes[c] == 0 {
			t.Errorf("no change record for %s (have %v)", c, changes)
		}
	}
	for c := range changes {
		if strings.Contains(c, "spc_a_other") || strings.Contains(c, "tsk_spc_a_a_done") || strings.Contains(c, "msg_spc_a_a_acknowledged") {
			t.Errorf("change record for an untouched row: %s", c)
		}
	}

	renames, err := f.svc.ListRoleRenames(f.ctx, "spc_a")
	must(t, err)
	if len(renames) != 1 || renames[0].ID != rn.ID {
		t.Errorf("renames %+v", renames)
	}
}

// TestRenameSpaceRoleBodyIsRerunnable runs the transaction body, rolls it back (as a BUSY retry does) and runs it
// again: the committed result is the one the first attempt saw.
func TestRenameSpaceRoleBodyIsRerunnable(t *testing.T) {
	f := newRenameFx(t)
	rollback := errors.New("rollback")
	run := func(commit bool) []map[string]any {
		var out []map[string]any
		err := f.s.W().TxNamed(f.ctx, "catalog.space_role_rename", func(tx *store.Tx) error {
			if err := f.svc.renameTx(f.ctx, tx, op, "rrn_fixed", "spc_a", "a", "z", 0); err != nil {
				return err
			}
			out = f.renameState(tx.SQL, true)
			if !commit {
				return rollback
			}
			return nil
		})
		if commit {
			must(t, err)
		} else if !errors.Is(err, rollback) {
			t.Fatal(err)
		}
		return out
	}
	before := f.renameState(f.s.ReadDB(), false)
	first := run(false)
	if f.role("spc_a", "a").Name != "a" {
		t.Fatal("the rolled back attempt left the rename")
	}
	if after := f.renameState(f.s.ReadDB(), false); !reflect.DeepEqual(before, after) {
		t.Fatal("the rolled back attempt left changes")
	}
	second := run(true)
	if !reflect.DeepEqual(first, second) {
		t.Errorf("the second attempt differs:\n%v\n%v", first, second)
	}
	if committed := f.renameState(f.s.ReadDB(), true); !reflect.DeepEqual(committed, second) {
		t.Errorf("committed state differs from the attempt:\n%v\n%v", committed, second)
	}
}

// renameState is the seeded rows plus the role and rename rows, as one comparable value. changedOnly keeps just the rows
// a rename of a rewrites (want set), so the dump is cheap enough to run inside the write transaction; the rest are
// compared outside it.
func (f *renameFx) renameState(q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, changedOnly bool) []map[string]any {
	out := []map[string]any{}
	for _, r := range f.rows {
		if changedOnly && r.want == nil {
			continue
		}
		out = append(out, snapshot(f.t, f.ctx, q, r))
	}
	for _, n := range []string{"a", "z"} {
		out = append(out, snapshot(f.t, f.ctx, q, row{table: "space_roles", key: map[string]any{"space_id": "spc_a", "name": n}}))
	}
	return append(out, snapshot(f.t, f.ctx, q, row{table: "space_role_renames", key: map[string]any{"id": "rrn_fixed"}}))
}

func (f *fx) role(space, name string) *SpaceRole {
	f.t.Helper()
	r, err := f.svc.s.R().GetSpaceRole(f.ctx, dbRole(space, name))
	must(f.t, err)
	return spaceRoleOut(r)
}

// TestRenameSpaceRoleBlockers: each kind of live work refuses the rename with role_busy, names itself and changes
// nothing.
func TestRenameSpaceRoleBlockers(t *testing.T) {
	cases := []struct {
		name, want string
		seed       func(s *seeder)
	}{
		{"pending run", "run run_live (pending)", func(s *seeder) {
			s.ins("runs", map[string]any{"id": "run_live", "space_id": "spc_a", "node_id": "nod_1", "kind": "worker", "role": "a", "desired": "running", "observed": "pending"})
		}},
		{"running run", "run run_live (running)", func(s *seeder) {
			s.ins("runs", map[string]any{"id": "run_live", "space_id": "spc_a", "node_id": "nod_1", "kind": "worker", "role": "a", "desired": "running", "observed": "running"})
		}},
		{"stopping run", "run run_live (stopping)", func(s *seeder) {
			s.ins("runs", map[string]any{"id": "run_live", "space_id": "spc_a", "node_id": "nod_1", "kind": "worker", "role": "a", "desired": "stopped", "observed": "stopping"})
		}},
		{"unknown run", "run run_live (unknown)", func(s *seeder) {
			s.ins("runs", map[string]any{"id": "run_live", "space_id": "spc_a", "node_id": "nod_1", "kind": "worker", "role": "a", "desired": "running", "observed": "unknown"})
		}},
		{"limit wait", "limit wait lwt_open (waiting)", func(s *seeder) {
			s.ins("runs", map[string]any{"id": "run_park", "space_id": "spc_a", "node_id": "nod_1", "kind": "worker", "role": "a", "observed": "exited"})
			s.ins("limit_waits", map[string]any{"id": "lwt_open", "space_id": "spc_a", "run_id": "run_park", "role": "a", "state": "waiting"})
		}},
		{"resuming limit wait", "limit wait lwt_open (resuming)", func(s *seeder) {
			s.ins("runs", map[string]any{"id": "run_park", "space_id": "spc_a", "node_id": "nod_1", "kind": "worker", "role": "a", "observed": "exited"})
			s.ins("limit_waits", map[string]any{"id": "lwt_open", "space_id": "spc_a", "run_id": "run_park", "role": "a", "state": "resuming"})
		}},
		{"account switch", "account switch asw_open (parked)", func(s *seeder) {
			s.ins("account_switches", map[string]any{"id": "asw_open", "space_id": "spc_a", "role": "a", "state": "parked"})
		}},
		{"pending account switch", "account switch asw_open (pending)", func(s *seeder) {
			s.ins("account_switches", map[string]any{"id": "asw_open", "space_id": "spc_a", "role": "a", "state": "pending"})
		}},
		{"resumed account switch", "account switch asw_open (resumed)", func(s *seeder) {
			s.ins("account_switches", map[string]any{"id": "asw_open", "space_id": "spc_a", "role": "a", "state": "resumed"})
		}},
		{"queued launch", "queued launch cmd_launch (sent)", func(s *seeder) {
			s.ins("node_commands", map[string]any{"id": "cmd_launch", "node_id": "nod_1", "space_id": "spc_a", "kind": "run.launch", "payload_json": `{"role":"a"}`,
				"state": "sent"})
		}},
		{"pending queued launch", "queued launch cmd_launch (pending)", func(s *seeder) {
			s.ins("node_commands", map[string]any{"id": "cmd_launch", "node_id": "nod_1", "space_id": "spc_a", "kind": "run.launch", "payload_json": `{"role":"a"}`,
				"state": "pending"})
		}},
		{"dream run", "dream run drr_open (reported)", func(s *seeder) {
			s.ins("dream_configs", map[string]any{"id": "drm_x", "space_id": "spc_a", "slug": "x", "space_role": "other"})
			s.ins("dream_runs", map[string]any{"id": "drr_open", "config_id": "drm_x", "space_id": "spc_a", "state": "reported"})
		}},
		{"dream commit", "dream commit cmd_dream (pending)", func(s *seeder) {
			s.ins("node_commands", map[string]any{"id": "cmd_dream", "node_id": "nod_1", "space_id": "spc_a", "kind": "dream.commit", "state": "pending"})
		}},
		{"relocation", "relocation rlc_open (draining)", func(s *seeder) {
			s.ins("relocations", map[string]any{"id": "rlc_open", "space_id": "spc_a", "source_node_id": "nod_1", "dest_node_id": "nod_1", "state": "draining"})
		}},
		{"relocating space", "relocation (the space is moving", func(s *seeder) {
			if _, err := s.tx.SQL.ExecContext(s.ctx, `UPDATE spaces SET desired_node_id = 'nod_1' WHERE id = 'spc_a'`); err != nil {
				s.t.Fatal(err)
			}
		}},
		{"pending rename into the name", "rename rrn_prev (q to a) waiting for node nod_1", func(s *seeder) {
			s.ins("space_role_renames", map[string]any{"id": "rrn_prev", "space_id": "spc_a", "from_name": "q", "to_name": "a", "node_id": "nod_1", "node_state": "pending"})
		}},
		{"pending rename away from the target", "rename rrn_prev (z to y) waiting", func(s *seeder) {
			s.ins("space_role_renames", map[string]any{"id": "rrn_prev", "space_id": "spc_a", "from_name": "z", "to_name": "y", "node_id": "nod_1", "node_state": "pending"})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newRenameFx(t)
			must(t, f.s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
				c.seed(&seeder{t: t, ctx: f.ctx, tx: tx, cols: map[string][]colInfo{}, n: 1000})
				return nil
			}))
			before := f.renameStateNoID()
			_, _, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "a", "z", 0)
			code(t, err, apperr.CodePrecondition)
			var rr *RenameRefused
			if !errors.As(err, &rr) || rr.Reason != RenameRoleBusy {
				t.Fatalf("cause %v, want role_busy", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name %q", err, c.want)
			}
			if after := f.renameStateNoID(); !reflect.DeepEqual(before, after) {
				t.Error("a refused rename changed rows")
			}
		})
	}
	// Live work elsewhere does not block: a run of another role, an ended run of the role, and the role's run in
	// another space.
	f := newRenameFx(t)
	must(t, f.s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		s := &seeder{t: t, ctx: f.ctx, tx: tx, cols: map[string][]colInfo{}, n: 1000}
		s.ins("runs", map[string]any{"id": "run_o", "space_id": "spc_a", "node_id": "nod_1", "kind": "worker", "role": "other", "observed": "running"})
		s.ins("runs", map[string]any{"id": "run_b", "space_id": "spc_b", "node_id": "nod_1", "kind": "worker", "role": "a", "observed": "running"})
		s.ins("runs", map[string]any{"id": "run_e", "space_id": "spc_a", "node_id": "nod_1", "kind": "worker", "role": "a", "desired": "running", "observed": "failed"})
		return nil
	}))
	_, _, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "a", "z", 0)
	must(t, err)
}

func (f *renameFx) renameStateNoID() []map[string]any {
	out := []map[string]any{}
	for _, r := range f.rows {
		out = append(out, snapshot(f.t, f.ctx, f.s.ReadDB(), r))
	}
	for _, n := range []string{"a", "z"} {
		out = append(out, snapshot(f.t, f.ctx, f.s.ReadDB(), row{table: "space_roles", key: map[string]any{"space_id": "spc_a", "name": n}}))
	}
	var n int
	must(f.t, f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM space_role_renames WHERE to_name = 'z'`).Scan(&n))
	return append(out, map[string]any{"renames": n})
}

func TestRenameSpaceRoleRefusals(t *testing.T) {
	f := newRenameFx(t)
	_, _, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "a", "a", 0)
	code(t, err, apperr.CodeInvalid)
	_, _, err = f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "a", "Bad Name", 0)
	code(t, err, apperr.CodeInvalid)
	_, _, err = f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "a", "cross-space", 0)
	code(t, err, apperr.CodeInvalid)
	_, _, err = f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "nope", "z", 0)
	code(t, err, apperr.CodeNotFound)
	_, _, err = f.svc.RenameSpaceRole(f.ctx, op, "spc_x", "a", "z", 0)
	code(t, err, apperr.CodeNotFound)
	_, _, err = f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "a", "z", 7)
	code(t, err, apperr.CodePrecondition)

	taken := func(to, want string, seed func(s *seeder)) {
		t.Helper()
		if seed != nil {
			must(t, f.s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
				seed(&seeder{t: t, ctx: f.ctx, tx: tx, cols: map[string][]colInfo{}, n: 2000})
				return nil
			}))
		}
		_, _, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "a", to, 0)
		code(t, err, apperr.CodeAlreadyExists)
		var rr *RenameRefused
		if !errors.As(err, &rr) || rr.Reason != RenameNameTaken || !strings.Contains(err.Error(), want) {
			t.Errorf("rename to %s: %v, want name_taken naming %q", to, err, want)
		}
	}
	taken("other", "a role other exists", nil)
	taken("y", "memory collection mem_left is named y", func(s *seeder) {
		s.ins("memory_collections", map[string]any{"id": "mem_left", "space_id": "spc_a", "role": "y"})
	})
	taken("w", "an MCP grant of server mcp_1 to w exists", func(s *seeder) {
		s.ins("mcp_grants", map[string]any{"id": "mcg_left", "server_id": "mcp_1", "space_id": "spc_a", "role": "w"})
	})
	taken("v", "account acc_1 is in the pool of v", func(s *seeder) {
		s.ins("space_role_accounts", map[string]any{"space_id": "spc_a", "role": "v", "account_id": "acc_1"})
	})
	if f.role("spc_a", "a").Revision != 1 {
		t.Error("a refused rename changed the role")
	}
}

func TestRenameInList(t *testing.T) {
	for _, c := range []struct {
		in, want string
		changed  bool
	}{
		{`["a"]`, `["z"]`, true},
		{`["*","a","c"]`, `["*","z","c"]`, true},
		{`["c","a","*","z"]`, `["c","z","*"]`, true},
		{`["z","a"]`, `["z"]`, true},
		{`["*"]`, `["*"]`, false},
		{`["ab","c"]`, `["ab","c"]`, false},
		{`[]`, `[]`, false},
		{`not json`, `not json`, false},
	} {
		got, changed := renameInList(c.in, "a", "z")
		if got != c.want || changed != c.changed {
			t.Errorf("renameInList(%s) = %s, %v; want %s, %v", c.in, got, changed, c.want, c.changed)
		}
	}
}

// TestCurrentRoleName follows renames for names no role has, and never redirects a name a role has.
func TestCurrentRoleName(t *testing.T) {
	f := newRenameFx(t)
	q := f.s.R()
	resolve := func(name, want string) {
		t.Helper()
		got, err := CurrentRoleName(f.ctx, q, "spc_a", name)
		must(t, err)
		if got != want {
			t.Errorf("CurrentRoleName(%s) = %s, want %s", name, got, want)
		}
	}
	former := func(name string, want ...string) {
		t.Helper()
		got, err := FormerRoleNames(f.ctx, q, "spc_a", name)
		must(t, err)
		slices.Sort(got)
		slices.Sort(want)
		if want == nil {
			want = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("FormerRoleNames(%s) = %v, want %v", name, got, want)
		}
	}
	resolve("a", "a")
	resolve("gone", "gone")
	former("a")

	mark := func(id string) { // the node reported the move: the next rename of these names may go ahead
		f.exec(`UPDATE space_role_renames SET node_state = 'done' WHERE id = ?`, id)
	}
	_, r1, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "a", "b", 0)
	must(t, err)
	mark(r1.ID)
	_, r2, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "b", "c2", 0)
	must(t, err)
	mark(r2.ID)
	resolve("a", "c2")
	resolve("b", "c2")
	resolve("c2", "c2")
	former("c2", "a", "b")
	if got, _ := CurrentRoleName(f.ctx, q, "spc_b", "a"); got != "a" {
		t.Errorf("another space resolved a to %s", got)
	}

	// A new role a is a role again: it is never redirected, and it is no former name of c2.
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "a", TemplateID: "rtp_1"})
	must(t, err)
	resolve("a", "a")
	resolve("b", "c2")
	former("c2", "b")
	// Renamed away again, a leads to its newest name.
	_, r3, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "a", "d", 0)
	must(t, err)
	mark(r3.ID)
	resolve("a", "d")
	former("d", "a")
	former("c2", "b")

	// A name reused in the middle of a chain does not capture it: e1 -> e2 -> e3, then a new role e2. e1 still leads
	// to e3, the new e2 is itself, and only e1 is a former name of e3.
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "e1", TemplateID: "rtp_1"})
	must(t, err)
	_, r4, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "e1", "e2", 0)
	must(t, err)
	mark(r4.ID)
	_, r5, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "e2", "e3", 0)
	must(t, err)
	mark(r5.ID)
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "e2", TemplateID: "rtp_1"})
	must(t, err)
	resolve("e1", "e3")
	resolve("e2", "e2")
	former("e3", "e1")
	former("e2")

	// The chain only follows renames after the one it came from: n -> x, then m -> n into the freed name. m leads to
	// the new n, not on to x (the older n -> x rename was of the previous n).
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "n", TemplateID: "rtp_1"})
	must(t, err)
	_, r6, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "n", "x", 0)
	must(t, err)
	mark(r6.ID)
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "m", TemplateID: "rtp_1"})
	must(t, err)
	_, r7, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "m", "n", 0)
	must(t, err)
	mark(r7.ID)
	resolve("m", "n")
	resolve("n", "n")

	// A cycle in the table (not reachable through renames) resolves to the name unchanged.
	f.exec(`INSERT INTO space_role_renames (id, space_id, from_name, to_name, renamed_at, renamed_by, node_state)
		VALUES ('rrn_c1', 'spc_a', 'p', 'q', 1, 't', 'done'), ('rrn_c2', 'spc_a', 'q', 'p', 2, 't', 'done')`)
	resolve("p", "p")
}

// TestRenameSpaceRoleDeletedTarget: a rename chain whose end was deleted resolves to the name unchanged.
func TestRenameSpaceRoleDeletedTarget(t *testing.T) {
	f := newRenameFx(t)
	_, _, err := f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "a", "z", 0)
	must(t, err)
	must(t, f.svc.DeleteSpaceRole(f.ctx, op, "spc_a", "z"))
	got, err := f.svc.CurrentRoleName(f.ctx, "spc_a", "a")
	must(t, err)
	if got != "a" {
		t.Errorf("resolved to %s, want a", got)
	}
	// The not-found error names where a renamed role went.
	_, err = f.svc.SaveSpaceRole(f.ctx, op, "spc_a", SpaceRole{Name: "y", TemplateID: "rtp_1"})
	must(t, err)
	f.exec(`UPDATE space_role_renames SET node_state = 'done'`)
	_, _, err = f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "y", "x", 0)
	must(t, err)
	_, _, err = f.svc.RenameSpaceRole(f.ctx, op, "spc_a", "y", "w", 0)
	code(t, err, apperr.CodeNotFound)
	if !strings.Contains(err.Error(), "renamed to x") {
		t.Errorf("error %q does not say where the role went", err)
	}
}
