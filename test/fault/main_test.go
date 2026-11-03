//go:build fault

// Package fault is SpaceMan's fault-injection suite (`make test-fault`): crashes, a full disk while recording,
// interrupted recordings and uploads, disconnects beyond the lease, network partitions during a command, corrupted
// node state and database contention. It runs central and a node agent in process (internal/app/apptest, in-memory
// runner); central's crash test kills a real child process. All evidence here is simulated (docs/TEST_STRATEGY.md).
package fault

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// The test binary doubles as the central process that TestFaultCentralKilledMidWrite kills.
	if os.Getenv(childEnv) == "central" {
		os.Exit(centralChild())
	}
	os.Exit(m.Run())
}
