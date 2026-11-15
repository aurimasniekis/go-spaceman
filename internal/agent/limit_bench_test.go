package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// BenchmarkHelloRuns measures building a hello's run list from a journal of 2500 runs, a third of them with a usage
// limit (open or cleared): the journal read and each run's normalized report, as session does before dialing.
func BenchmarkHelloRuns(b *testing.B) {
	ctx := context.Background()
	j, err := OpenJournal(ctx, filepath.Join(b.TempDir(), "journal"), nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = j.Close() })
	now := time.Now().UTC()
	since := now.Add(-time.Hour)
	reset := now.Add(4 * time.Hour)
	for i := range 2500 {
		id := fmt.Sprintf("run_bench%05d", i)
		if err := j.insertRun(ctx, runRow{RunID: id, SpaceID: "spc_b", Generation: 1, Kind: "worker", Role: "w", Harness: "claude-code",
			InstanceID: "ins_b", TerminalName: RunName(id)}); err != nil {
			b.Fatal(err)
		}
		switch i % 3 {
		case 1:
			err = j.setRunLimit(ctx, id, runLimit{Kind: proto.LimitFiveHour, ResetsAt: &reset, Since: &since, Offset: 4096})
		case 2:
			err = j.setRunLimit(ctx, id, runLimit{Since: &since, Offset: 8192})
		}
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for range b.N {
		runs, err := j.runs(ctx, false)
		if err != nil || len(runs) != 2500 {
			b.Fatal(len(runs), err)
		}
		out := make([]proto.RunReport, 0, len(runs))
		for _, r := range runs {
			out = append(out, r.report(now))
		}
		benchReports = out
	}
}

// benchReports keeps the benchmark's result alive, so the compiler can't drop the work.
var benchReports []proto.RunReport
