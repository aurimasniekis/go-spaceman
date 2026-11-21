package app

import (
	"context"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
)

// BackupMemoryWait bounds how long a backup waits for the nodes to checkpoint and sync role memory (SM-MEM-005,
// ADR-0045). Tests shorten it.
var BackupMemoryWait = time.Minute

// backupMemoryTimeout is the backup service's own bound on the whole memory-flush stage: the flush bounds itself to
// BackupMemoryWait plus a grace of at most 10 seconds, and this backstop leaves room above that.
func backupMemoryTimeout() time.Duration { return BackupMemoryWait + 30*time.Second }

// prepareBackupMemory asks every connected node to checkpoint and sync its spaces' role memory before a backup and
// reports which spaces' memory is possibly stale in it. It runs outside any transaction (the backup goroutine). Each
// node's answer, or the end of the wait for it, is reported to the backup's progress.
func (c *Central) prepareBackupMemory(ctx context.Context) *backup.MemoryFlush {
	start := c.Clock.Now()
	f := &backup.MemoryFlush{Attempted: true, StartedAt: &start, Flushed: []string{}, Stale: []backup.StaleMemory{}}
	res, err := c.Fleet.FlushMemory(ctx, BackupMemoryWait, func() { backup.ReportProgress(ctx) })
	f.Seconds = c.Clock.Now().Sub(start).Seconds()
	if err != nil {
		c.Log.Warn("flush role memory before a backup", obs.Err(err))
		f.Note = "the spaces could not be listed: " + err.Error()
	}
	for _, r := range res {
		if r.Flushed {
			f.Flushed = append(f.Flushed, r.SpaceID)
			continue
		}
		f.Stale = append(f.Stale, backup.StaleMemory{SpaceID: r.SpaceID, SpaceSlug: r.SpaceSlug, NodeID: r.NodeID, NodeName: r.NodeName,
			Reason: r.Reason, Detail: r.Detail})
	}
	return f
}
