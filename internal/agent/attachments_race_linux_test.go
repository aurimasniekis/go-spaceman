package agent

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/aurimasniekis/go-spaceman/internal/ids"
)

// Review M1: the run swaps a real folder and a link in and out of a folder on the attachments path while the node
// saves, removes and prunes. Every step goes through handles opened from the node's spaces folder, so the link's
// target (outside the space, or another folder inside it) is never written to or emptied. The save is the real
// saveAttachment, with central's ranged reads served by a fake.
func TestAttachmentsFolderSwapRace(t *testing.T) {
	const iterations = 1500
	for _, tc := range []struct {
		name  string
		swap  func(instances, inst string) string // the folder that is swapped
		links func(t *testing.T, instances, inst, other string) []string
	}{
		{"attachments", func(instances, inst string) string { return filepath.Join(instances, inst, attachmentsDir) },
			func(t *testing.T, instances, inst, other string) []string {
				return []string{t.TempDir(), filepath.Join(instances, inst, "claude"), filepath.Join(instances, other, "claude")}
			}},
		{"instance", func(instances, inst string) string { return filepath.Join(instances, inst) },
			func(t *testing.T, instances, inst, other string) []string {
				return []string{t.TempDir(), filepath.Join(instances, other)}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, spaceID, workdir, inst, other := attachAgent(t)
			ctx := context.Background()
			instances := filepath.Join(a.o.DataDir, "spaces", workdir, "instances")
			if root, _, err := a.openAttachments(workdir, inst, true); err != nil {
				t.Fatal(err)
			} else {
				_ = root.Close()
			}
			// Every target holds a decoy: an entry named like an ended run (which prune and remove delete) and a file.
			ended := ids.New(ids.Run)
			targets := tc.links(t, instances, inst, other)
			for _, d := range targets {
				write(t, d, "decoy.txt", "keep")
				write(t, d, ended+"/decoy.txt", "keep")
				write(t, d, attachmentsDir+"/"+ended+"/decoy.txt", "keep")
			}
			swapped := tc.swap(instances, inst)
			linkName := swapped + ".link"
			stop := make(chan struct{})
			var linked atomic.Int64
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				// The real folder and a link change places atomically (renameat2 RENAME_EXCHANGE), so the name is never
				// missing and the link is in place half the time; the link's target changes between rounds.
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					_ = os.Remove(linkName)
					if os.Symlink(targets[i%len(targets)], linkName) != nil {
						continue
					}
					if unix.Renameat2(unix.AT_FDCWD, swapped, unix.AT_FDCWD, linkName, unix.RENAME_EXCHANGE) != nil {
						continue
					}
					linked.Add(1)
					for unix.Renameat2(unix.AT_FDCWD, swapped, unix.AT_FDCWD, linkName, unix.RENAME_EXCHANGE) != nil {
						// put the real folder back before the next round
					}
				}
			}()
			run := startRun(t, a, spaceID, inst)
			var saved, removed int
			for i := 0; i < iterations; i++ {
				att := ids.New(ids.Attachment)
				a.fetchChunk = fakeCentral(t, att, "../shot.png", []byte("\x89PNG saved"), nil)
				if a.saveAttachment(ctx, run, saveArgs(att)).OK {
					saved++
				}
				a.removeRunAttachments(ctx, runRow{RunID: ended, SpaceID: spaceID, InstanceID: inst})
				a.pruneAttachments(ctx, workdir, inst, run.RunID)
				removed++
			}
			close(stop)
			wg.Wait()
			if saved == 0 || removed == 0 || linked.Load() == 0 {
				t.Fatalf("the race never let a step through (saved %d, link in place %d times)", saved, linked.Load())
			}
			for _, d := range targets {
				var got []string
				_ = filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
					if err == nil && !e.IsDir() {
						rel, _ := filepath.Rel(d, p)
						got = append(got, rel)
					}
					return nil
				})
				want := []string{attachmentsDir + "/" + ended + "/decoy.txt", "decoy.txt", ended + "/decoy.txt"}
				if !slices.Equal(got, want) {
					t.Fatalf("the link target %s was touched: %v", d, got)
				}
			}
			t.Logf("%d iterations, %d saves through the race, the link in place %d times", iterations, saved, linked.Load())
		})
	}
}
