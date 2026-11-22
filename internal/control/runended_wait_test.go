package control

import (
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
)

// A parked worker's end is announced by its park message, so no run-ended notice is sent while its wait is waiting or
// resuming. A wait given up at park (its task had finished) announced nothing, so the run's end is told to control.
func TestRunEndedNoticeDependsOnLimitWaitState(t *testing.T) {
	for _, tc := range []struct {
		state, reason string
		told          bool
	}{{"waiting", "", false}, {"resuming", "", false}, {"cancelled", "task_moved", true}} {
		t.Run(tc.state, func(t *testing.T) {
			e := newPeopleEnv(t)
			e.sv.msgs = messages.New(e.s, e.aw, e.ch)
			e.sv.q = queue.New(e.s, e.aw, e.ch)
			e.sv.Async = false
			inst := ids.New(ids.Instance)
			e.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, session_id, turn, desired, observed, launch_json,
				created_at, created_by, updated_at, observed_at) VALUES ('run_w', 'spc_a', 'nod_1', 1, 'worker', 'implementor', 'claude-code', ?, 'ses_w', 3,
				'running', 'exited', '{"prompt":"p"}', 0, 't', 0, 0)`, inst)
			e.exec(`INSERT INTO limit_waits (id, space_id, run_id, task_id, run_kind, role, node_id, instance_id, harness, launch_json, resume_at, state, last_error,
				created_at, updated_at) VALUES ('lwt_1', 'spc_a', 'run_w', 'tsk_x', 'worker', 'implementor', 'nod_1', ?, 'claude-code', '{}', 1, ?, ?, 0, 0)`,
				inst, tc.state, tc.reason)
			e.sv.OnRunEnded("run_w")
			got := e.controlBodies("spc_a")
			if tc.told && (len(got) != 1 || !strings.Contains(got[0], "run_w")) {
				t.Fatalf("control was not told the run ended: %q", got)
			}
			if !tc.told && len(got) != 0 {
				t.Fatalf("control was told: %q", got)
			}
		})
	}
}
