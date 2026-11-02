package metricstest

import (
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
)

func TestValue(t *testing.T) {
	m := metrics.New()
	m.NodeConnect(metrics.NodeAuthFailed)
	m.NodeConnect(metrics.NodeAuthFailed)
	m.NodeConnect(metrics.NodeRevoked)
	m.SetOutboxBacklog(3, 1)
	m.StoreWriterWait(time.Second)
	if v := Value(t, m, "spaceman_node_connects_total"); v != 3 {
		t.Errorf("sum = %v", v)
	}
	if v := Value(t, m, "spaceman_node_connects_total", "result", "auth_failed"); v != 2 {
		t.Errorf("filtered = %v", v)
	}
	if v := Value(t, m, "spaceman_outbox_pending"); v != 3 {
		t.Errorf("gauge = %v", v)
	}
	if v := Value(t, m, "spaceman_store_writer_wait_seconds"); v != 1 {
		t.Errorf("histogram count = %v", v)
	}
	if v := Value(t, m, "spaceman_missing"); v != 0 {
		t.Errorf("missing = %v", v)
	}
}
