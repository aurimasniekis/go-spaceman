package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// SM-OBS-006: the status tolerates missing sources (the OpenAPI spec registers it with none) and reports an
// unreadable schema version as unavailable instead of failing; uptime follows the clock.
func TestSystemStatusSources(t *testing.T) {
	st := storetest.Open(t)
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	d := &Deps{Store: st, Clock: clock.NewFake(now)}
	ctx := context.Background()

	out, err := d.systemStatus(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.UptimeSeconds != 0 || out.BootID != "" || out.Storage.Checks == nil || out.Build.Version == "" || out.Protocol.Major == 0 {
		t.Fatalf("no sources %+v", out)
	}

	out, err = d.systemStatus(ctx, &SystemSources{StartedAt: now.Add(-90 * time.Second),
		Schema:      func(context.Context) (int64, int64, error) { return 0, 0, errors.New("boom") },
		Nodes:       func(context.Context) (int, int, error) { return 3, 2, nil },
		StoreSizes:  func() (int64, int64) { return 10, 20 },
		LiveStreams: func() int64 { return 4 }})
	if err != nil {
		t.Fatal(err)
	}
	if out.UptimeSeconds != 90 || out.SchemaVersion.Error != "unavailable" || out.Counts.NodesActive != 3 || out.Counts.NodesConnected != 2 ||
		out.Storage.DBBytes != 10 || out.Storage.WALBytes != 20 || out.Counts.LiveStreams != 4 {
		t.Fatalf("with sources %+v", out)
	}

	if _, err := d.systemStatus(ctx, &SystemSources{Nodes: func(context.Context) (int, int, error) { return 0, 0, errors.New("down") }}); err == nil {
		t.Fatal("a nodes error should fail the status")
	}
}
