package dream

import (
	"context"
	"reflect"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// ADR-0093: the node filters transcripts by the role its runs had, so a dream's transcript roles (and its own excluded
// role) list every former name of a renamed role too. "*" stays, a role that never was renamed adds nothing, and a
// former name that is a role again is not added.
func TestWithFormerNames(t *testing.T) {
	ctx := context.Background()
	s := storetest.Open(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
			`INSERT INTO role_templates (id, slug, name, kind, created_at, created_by, updated_at) VALUES ('rtp_w', 'worker', 'W', 'worker', 0, 't', 0)`,
			`INSERT INTO space_roles (space_id, name, template_id, created_at, created_by, updated_at) VALUES ('spc_a', 'impl', 'rtp_w', 0, 't', 0),
			 ('spc_a', 'rev', 'rtp_w', 0, 't', 0), ('spc_a', 'old', 'rtp_w', 0, 't', 0)`,
			// coder -> dev -> impl; old -> impl, but old is a role again.
			`INSERT INTO space_role_renames (id, space_id, from_name, to_name, renamed_at, renamed_by, node_state) VALUES
			 ('rrn_1', 'spc_a', 'coder', 'dev', 1, 't', 'done'), ('rrn_2', 'spc_a', 'dev', 'impl', 2, 't', 'done'), ('rrn_3', 'spc_a', 'old', 'impl', 0, 't', 'done')`,
		} {
			if _, err := tx.SQL.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}))
	for _, c := range []struct{ in, want []string }{
		{[]string{"*"}, []string{"*"}},
		{[]string{"rev"}, []string{"rev"}},
		{[]string{"rev", "impl"}, []string{"rev", "impl", "dev", "coder"}},
		{[]string{"*", "impl", "dev"}, []string{"*", "impl", "dev", "coder"}},
		{[]string{}, []string{}},
	} {
		got, err := withFormerNames(ctx, s.R(), "spc_a", c.in)
		must(err)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("withFormerNames(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
