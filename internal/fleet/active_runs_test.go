package fleet

import (
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
)

// A run that ended on its own keeps desired = running, but it is not active: it neither blocks archiving a space nor
// counts as one of its active runs (ListActiveRunsBySpace uses the live predicate, fleet.Live). A live run still blocks.
func TestEndedRunDoesNotBlockArchive(t *testing.T) {
	e := newStaleEnv(t)
	e.exec(`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`)
	e.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'paused', 'a', 0, 't', 0)`)
	e.run("run_done", "spc_a", "nod_1", "exited", e.us(-time.Hour))
	e.run("run_live", "spc_a", "nod_1", "running", e.us(-time.Hour))

	sp, err := e.svc.GetSpace(e.ctx, "spc_a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetSpaceState(e.ctx, audit.System, "spc_a", sp.Version, "archived", ""); err == nil {
		t.Fatal("a space with a live run was archived")
	} else if want := "stop the space's 1 active run(s)"; !strings.Contains(err.Error(), want) {
		t.Fatalf("archive refused with %q, want %q", err, want)
	}
	if active, err := e.s.R().ListActiveRunsByNode(e.ctx, "nod_1"); err != nil || len(active) != 1 || active[0].ID != "run_live" {
		t.Fatalf("active runs on the node: %+v %v", active, err)
	}

	e.exec(`UPDATE runs SET observed = 'lost' WHERE id = 'run_live'`)
	if _, err := e.svc.SetSpaceState(e.ctx, audit.System, "spc_a", sp.Version, "archived", ""); err != nil {
		t.Fatalf("a space whose runs all ended was not archived: %v", err)
	}
}
