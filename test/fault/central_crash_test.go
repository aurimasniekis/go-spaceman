//go:build fault

package fault

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/app/apptest"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
)

const (
	childEnv     = "SM_FAULT_CHILD"
	childDataEnv = "SM_FAULT_DATA"
)

func childConfig(dir string) *config.Config {
	cfg := config.Default()
	cfg.Mode = config.ModeDevelopment
	cfg.DataDir = filepath.Join(dir, "central")
	cfg.KEK.File = filepath.Join(dir, "kek")
	cfg.PublicURL = "http://127.0.0.1:1"
	return cfg
}

// centralChild opens central and writes as fast as it can from several goroutines (node creations: a row, its
// enrollment token, a change and an audit event per transaction), printing each committed node ID, until it is killed.
func centralChild() int {
	cfg := childConfig(os.Getenv(childDataEnv))
	c, err := app.OpenCentral(context.Background(), cfg, apptest.Log(), app.Options{Environ: []string{}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open central:", err)
		return 2
	}
	var mu sync.Mutex
	out := bufio.NewWriter(os.Stdout)
	fmt.Fprintln(out, "ready")
	_ = out.Flush()
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				en, err := c.Fleet.CreateNode(context.Background(), apptest.Admin, fleet.CreateNodeInput{Name: fmt.Sprintf("n-%d-%d", w, i)})
				if err != nil {
					fmt.Fprintln(os.Stderr, "write:", err)
					continue
				}
				mu.Lock()
				fmt.Fprintln(out, "ok", en.Node.ID)
				_ = out.Flush()
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return 0
}

// Fault: central is killed (SIGKILL, no shutdown, no WAL checkpoint) in the middle of a stream of write
// transactions. The next start opens the same data directory cleanly (lock released, quick check, migrations), every
// write the killed process reported as committed is there, the audit chain verifies end to end, and the reopened
// central accepts new writes that extend the chain. Repeated three times on the same data directory.
func TestFaultCentralKilledMidWrite(t *testing.T) {
	dir := t.TempDir()
	cfg := childConfig(dir)
	k, err := keyring.GenerateKEK()
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.WriteKEKFile(cfg.KEK.File, k); err != nil {
		t.Fatal(err)
	}
	var committed []string
	for round := range 3 {
		cmd := exec.Command(os.Args[0], "-test.run=^$") //nolint:gosec // the test binary itself
		cmd.Env = append(os.Environ(), childEnv+"=central", childDataEnv+"="+dir)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(stdout)
		n := 0
		deadline := time.AfterFunc(60*time.Second, func() { _ = cmd.Process.Kill() })
		for sc.Scan() {
			line := sc.Text()
			if id, ok := strings.CutPrefix(line, "ok "); ok {
				committed = append(committed, id)
				n++
			}
			if n == 40+round*25 {
				// Kill while the writers keep going: some transactions are in flight.
				_ = cmd.Process.Signal(syscall.SIGKILL)
				break
			}
		}
		deadline.Stop()
		_ = cmd.Wait()
		if n < 40 {
			t.Fatalf("round %d: child committed only %d writes before exiting: %s", round, n, stderr.String())
		}

		c, err := app.OpenCentral(context.Background(), cfg, apptest.Log(), app.Options{Environ: []string{}})
		if err != nil {
			t.Fatalf("round %d: reopen after kill: %v", round, err)
		}
		ctx := context.Background()
		for _, id := range committed {
			if _, err := c.Fleet.GetNode(ctx, id); err != nil {
				_ = c.Close()
				t.Fatalf("round %d: committed node %s lost after the kill: %v", round, id, err)
			}
		}
		res, err := audit.Verify(ctx, c.Store.R())
		if err != nil || !res.OK || res.Events < int64(len(committed)) {
			_ = c.Close()
			t.Fatalf("round %d: audit chain after kill: %+v %v (committed %d)", round, res, err, len(committed))
		}
		if _, err := c.Fleet.CreateNode(ctx, apptest.Admin, fleet.CreateNodeInput{Name: fmt.Sprintf("after-%d", round)}); err != nil {
			t.Fatalf("round %d: write after restart: %v", round, err)
		}
		if res2, _ := audit.Verify(ctx, c.Store.R()); !res2.OK || res2.Events != res.Events+1 {
			t.Fatalf("round %d: chain after a new write %+v", round, res2)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		t.Logf("round %d: %d committed writes survived, %d audit events verified", round, len(committed), res.Events)
	}
}
