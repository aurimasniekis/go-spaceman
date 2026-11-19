package app

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/memory"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// openArtifacts builds the configured artifact store (SM-STORE-002, ADR-0048): directories in the data directory, or
// the artifacts.s3 bucket. Its credentials are read from their files and never logged.
func openArtifacts(cfg *config.Config, st *store.Store, attachmentsDir string) (artifacts.Store, error) {
	if cfg.Artifacts.Backend == "s3" {
		return artifacts.S3FromConfig(cfg)
	}
	return artifacts.NewLocal(map[string]string{artifacts.NSAttachments: attachmentsDir, artifacts.NSRecordings: st.Path(store.RecordingsDir),
		artifacts.NSTranscripts: st.Path(artifacts.NSTranscripts)}), nil
}

// backupSources are the backup's payload trees; with a remote artifact store its objects are copied in too.
func backupSources(dataDir, attachmentsDir string, git *memory.Git, st artifacts.Store) backup.Sources {
	src := backup.DefaultSources(dataDir, attachmentsDir, git)
	if st != nil && st.Kind() != "local" {
		src.Store = st
	}
	return src
}

// uploadSpooledArtifacts runs once after the start with a remote artifact store: recordings of ended runs and
// complete transcripts left in the local spool (an upload that failed, or a stop before it finished) are uploaded.
// Local attachment content means artifacts were not migrated (after a restore or a backend switch): it is reported,
// since only `spaceman artifacts migrate` moves it.
func (c *Central) uploadSpooledArtifacts(ctx context.Context) {
	if c.Artifacts == nil || c.Artifacts.Kind() == "local" {
		return
	}
	log := obs.Component(c.Log, "artifacts")
	ended := func(ctx context.Context, runID string) bool {
		r, err := c.Store.R().GetRun(ctx, runID)
		if errors.Is(err, sql.ErrNoRows) {
			return true // no such run (deleted, or not in a restored database): archive what is left
		}
		if err == nil && r.RecordingDeletedAt.Valid {
			// A recording deleted earlier is not uploaded again: a folder an older release recreated is removed, with
			// whatever it uploaded to the store.
			if derr := c.Fleet.Recordings().Delete(ctx, runID); derr != nil && ctx.Err() == nil {
				log.Warn("leftover recording of a deleted run not removed; retried at the next start", obs.Err(derr))
			}
			return false
		}
		return err == nil && r.EndedAt.Valid
	}
	if n, err := c.Fleet.Recordings().ArchiveSpooled(ctx, ended); err != nil && ctx.Err() == nil {
		log.Warn("spooled recordings not archived to the artifact store; retried at the next start", slog.Int("archived", n), obs.Err(err))
	} else if n > 0 {
		log.Info("spooled recordings archived to the artifact store", slog.Int("archived", n))
	}
	if n, err := c.Usage.UploadSpooled(ctx); err != nil && ctx.Err() == nil {
		log.Warn("spooled transcripts not uploaded to the artifact store; retried at the next start", slog.Int("uploaded", n), obs.Err(err))
	} else if n > 0 {
		log.Info("spooled transcripts uploaded to the artifact store", slog.Int("uploaded", n))
	}
	dir := c.Cfg.Artifacts.LocalDir
	if dir == "" {
		dir = c.Store.Path("artifacts")
	}
	local := artifacts.NewLocal(map[string]string{artifacts.NSAttachments: dir})
	if l, err := local.List(ctx, artifacts.NSAttachments+"/"); err == nil && len(l) > 0 {
		log.Warn("attachment content is on the local disk while artifacts.backend is s3; it is not served until it is migrated: "+
			"stop central and run `spaceman artifacts migrate --to s3`", slog.Int("attachments", len(l)), slog.String("dir", filepath.Clean(dir)))
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("check the local attachment folder", obs.Err(err))
	}
}

// Bounds of one recording reconcile pass: at most reconcileMaxRuns runs examined, a pause after every removal, and a
// run absent from the database is only removed once its recording has been untouched for reconcileMinAge.
const (
	reconcileMaxRuns = 200
	reconcilePace    = 100 * time.Millisecond
	reconcileMinAge  = 24 * time.Hour
)

// runFate maps a run lookup to what the sweep may do: only a clean not-found is "absent"; any other error is returned
// (the run is skipped) and a run whose recording was not deleted is kept.
func runFate(r db.Run, err error) (fleet.RunFate, error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fleet.RunAbsent, nil
	case err != nil:
		return fleet.RunKeep, err
	case r.RecordingDeletedAt.Valid:
		return fleet.RunRecordingDeleted, nil
	}
	return fleet.RunKeep, nil
}

// reconcileRecordings removes stored recordings (local folders, spool folders, S3 objects) of runs whose recording
// was deleted or that no longer exist: once at start, then every artifacts.recording_reconcile_interval. A database
// error is never read as "absent": the run is skipped.
func (c *Central) reconcileRecordings(ctx context.Context) {
	interval := c.Cfg.Artifacts.RecordingReconcileInterval.D()
	if interval <= 0 || c.Fleet == nil {
		return
	}
	log := obs.Component(c.Log, "artifacts")
	fate := func(ctx context.Context, runID string) (fleet.RunFate, error) {
		r, err := c.Store.R().GetRun(ctx, runID)
		return runFate(r, err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		res, err := c.Fleet.Recordings().Reconcile(ctx, fate, fleet.ReconcileOptions{MaxRuns: reconcileMaxRuns, MinAge: reconcileMinAge, Absent: c.Cfg.Artifacts.RecordingReconcileAbsent, Pace: reconcilePace})
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn("recording reconcile failed; retried at the next pass", obs.Err(err))
		case res.Removed > 0:
			log.Info("leftover recordings of deleted runs removed", slog.Int("removed", res.Removed), slog.Int("skipped", res.Skipped), slog.Bool("truncated", res.Truncated))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
