package app

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/memory"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/store"
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
