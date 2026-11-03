package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/memory"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// SettingRestorePending is the settings key Restore writes into the restored database. The first start of central
// finds it and applies the post-restore rules (ApplyPendingRestore).
const SettingRestorePending = "backup.restore_pending"

// RestoreOptions configure Restore.
type RestoreOptions struct {
	// DataDir is the data directory to restore into.
	DataDir string
	// ArtifactsDir is where attachments go (default <data>/artifacts).
	ArtifactsDir string
	// KEK opens the sealed objects. It must be the KEK the backup was made with.
	KEK keyring.KeyWrapper
	// Force moves existing data aside (to <data>/backups/pre-restore-<time>/, and <artifacts>.pre-restore-<time> for an
	// artifacts folder outside the data directory) instead of refusing a non-empty target. Nothing is deleted.
	Force bool
	// Git restores role memory from its bundles; nil refuses a backup that holds memory.
	Git *memory.Git
	// AllowNetworkFS skips the network-filesystem refusal (development only).
	AllowNetworkFS bool
	Clock          clock.Clock
}

// RestoreResult is the outcome of Restore.
type RestoreResult struct {
	Manifest *Manifest `json:"manifest"`
	DataDir  string    `json:"data_dir"`
	Files    int       `json:"files"`
	Bytes    int64     `json:"bytes"`
	// MovedAside lists where existing data was moved with Force.
	MovedAside []string `json:"moved_aside,omitempty"`
	// ConfigCopy is where the backup's configuration was written for comparison ("" when the backup has none). It is
	// never applied: the running configuration stays the operator's.
	ConfigCopy string   `json:"config_copy,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

// PendingRestore is what Restore records for the first start.
type PendingRestore struct {
	BackupID        string    `json:"backup_id"`
	BackupCreatedAt time.Time `json:"backup_created_at"`
	RestoredAt      time.Time `json:"restored_at"`
	SchemaVersion   int64     `json:"schema_version"`
}

// Restore verifies the backup in backupDir with the KEK and installs it into o.DataDir. It takes the data-directory
// lock, so central must be stopped, and refuses a target that already holds data unless o.Force is set. Every object is
// decrypted into a staging directory inside the data directory and checked against the manifest while it is written;
// memory bundles become bare repositories again; the database is installed last. The restored database carries a
// pending-restore marker: the first start pauses every space and renews execution authority (ApplyPendingRestore).
func Restore(ctx context.Context, backupDir string, o RestoreOptions) (*RestoreResult, error) {
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.KEK == nil {
		return nil, fmt.Errorf("%w: restoring needs the KEK the backup was sealed under", ErrKEKMismatch)
	}
	dataDir, err := filepath.Abs(o.DataDir)
	if err != nil {
		return nil, err
	}
	artifacts := o.ArtifactsDir
	if artifacts == "" {
		artifacts = filepath.Join(dataDir, dirArtifacts)
	}
	if artifacts, err = filepath.Abs(artifacts); err != nil {
		return nil, err
	}
	if err := store.PrepareDataDir(dataDir); err != nil {
		return nil, err
	}
	if !o.AllowNetworkFS {
		if err := store.CheckLocalFilesystem(dataDir); err != nil {
			return nil, err
		}
	}
	lock, err := store.AcquireLock(filepath.Join(dataDir, store.LockFile))
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()

	rep, err := Verify(ctx, backupDir, VerifyOptions{KEK: o.KEK, TempDir: dataDir})
	if err != nil {
		return nil, err
	}
	m := rep.Manifest
	hasMemory := false
	for _, f := range m.Files {
		hasMemory = hasMemory || f.Kind == KindMemory
	}
	if hasMemory && o.Git == nil {
		return nil, errors.New("the backup holds role memory, and restoring it needs git: install git and run the restore again")
	}
	res := &RestoreResult{Manifest: m, DataDir: dataDir, Warnings: rep.Warnings}
	stamp := o.Clock.Now().UTC().Format("20060102T150405Z")
	if err := prepareTarget(dataDir, artifacts, stamp, o.Force, res); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(dataDir, ".restore-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	// Attachments stage next to their final folder, so installing them is a rename on the same filesystem.
	artStaging := staging
	if !strings.HasPrefix(artifacts, dataDir+string(filepath.Separator)) {
		if err := os.MkdirAll(filepath.Dir(artifacts), 0o700); err != nil {
			return nil, err
		}
		if artStaging, err = os.MkdirTemp(filepath.Dir(artifacts), ".restore-artifacts-"); err != nil {
			return nil, err
		}
		defer func() { _ = os.RemoveAll(artStaging) }()
	}
	for _, f := range m.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		src := filepath.Join(backupDir, filepath.FromSlash(f.Object()))
		dst := filepath.Join(staging, filepath.FromSlash(f.Path))
		if f.Kind == KindAttachment {
			dst = filepath.Join(artStaging, filepath.FromSlash(f.Path))
		}
		if err := openFile(src, dst, o.KEK, SealedKindFile(f.Path), m.ID, sizeSum{f.Size, f.SHA256}); err != nil {
			return nil, fmt.Errorf("restore %s: %w", f.Path, err)
		}
		if f.Kind == KindMemory {
			if err := unbundle(ctx, o.Git, dst, strings.TrimSuffix(dst, ".bundle")+".git"); err != nil {
				return nil, fmt.Errorf("restore memory %s: %w", f.Path, err)
			}
			if err := os.Remove(dst); err != nil {
				return nil, err
			}
		}
		res.Files++
		res.Bytes += f.Size
	}
	if err := markPending(ctx, filepath.Join(staging, DBFile), PendingRestore{BackupID: m.ID, BackupCreatedAt: m.CreatedAt,
		RestoredAt: o.Clock.Now().UTC(), SchemaVersion: m.SchemaVersion}); err != nil {
		return nil, err
	}
	for _, d := range []string{staging, artStaging} {
		if err := fsyncTree(d); err != nil {
			return nil, err
		}
	}
	if err := install(staging, artStaging, dataDir, artifacts, m.ID, res); err != nil {
		return nil, err
	}
	return res, nil
}

// unbundle turns a bundle into a bare repository holding all of its refs.
func unbundle(ctx context.Context, git *memory.Git, bundle, repo string) error {
	if err := git.InitBare(ctx, repo); err != nil {
		return err
	}
	if _, err := git.Run(ctx, repo, nil, "bundle", "verify", "--quiet", bundle); err != nil {
		return err
	}
	_, err := git.Run(ctx, repo, nil, "fetch", "--quiet", "--no-tags", "--update-head-ok", bundle, "+refs/*:refs/*")
	return err
}

// The data directory entries a restore replaces. Anything else (the lock, a development KEK, backups/) is left alone.
var restoredEntries = []string{store.DBFile, store.DBFile + "-wal", store.DBFile + "-shm", dirRecordings, dirTranscripts, dirMemory, dirArtifacts,
	store.QuarantineDir}

// prepareTarget checks that the target holds no data, or moves existing data aside when force is set.
func prepareTarget(dataDir, artifacts, stamp string, force bool, res *RestoreResult) error {
	var found []string
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		switch name := e.Name(); {
		case name == store.LockFile, name == keyring.DevKEKFile, name == store.BackupsDir, strings.HasPrefix(name, ".restore-"):
		case e.IsDir() && emptyDir(filepath.Join(dataDir, name)): // the folders store.PrepareDataDir creates
		default:
			found = append(found, name)
		}
	}
	outside := !strings.HasPrefix(artifacts, dataDir+string(filepath.Separator))
	if outside {
		if _, err := os.Stat(artifacts); err == nil && !emptyDir(artifacts) {
			found = append(found, artifacts)
		}
	}
	if len(found) == 0 {
		return nil
	}
	if !force {
		return fmt.Errorf("%w: %s contains %s (use --force to move existing data aside)", ErrNotEmpty, dataDir, strings.Join(found, ", "))
	}
	aside := filepath.Join(dataDir, store.BackupsDir, "pre-restore-"+stamp)
	if err := os.MkdirAll(aside, 0o700); err != nil {
		return err
	}
	for _, name := range restoredEntries {
		err := os.Rename(filepath.Join(dataDir, name), filepath.Join(aside, name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("move %s aside: %w", name, err)
		}
	}
	res.MovedAside = append(res.MovedAside, aside)
	if outside {
		to := artifacts + ".pre-restore-" + stamp
		if err := os.Rename(artifacts, to); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("move %s aside: %w", artifacts, err)
		} else if err == nil {
			res.MovedAside = append(res.MovedAside, to)
		}
	}
	for _, p := range res.MovedAside {
		res.Warnings = append(res.Warnings, "existing data moved to "+p)
	}
	return store.FsyncDir(dataDir)
}

// markPending records the pending-restore marker in the staged database (no keyring is needed for a settings row).
func markPending(ctx context.Context, dbPath string, p PendingRestore) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(DELETE)")
	q.Add("_pragma", "trusted_schema(0)")
	d, err := sql.Open("sqlite", "file:"+dbPath+"?"+q.Encode())
	if err != nil {
		return err
	}
	defer d.Close()
	d.SetMaxOpenConns(1)
	if _, err := d.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at, updated_by) VALUES (?, ?, ?, 'restore')
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		SettingRestorePending, string(b), p.RestoredAt.UnixMicro()); err != nil {
		return fmt.Errorf("record the pending restore: %w", err)
	}
	if err := d.Close(); err != nil {
		return err
	}
	f, err := os.Open(dbPath) //nolint:gosec // our staging file
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// install renames the staged trees and, last, the database into place, so an interrupted restore never leaves a
// database without its files.
func install(staging, artStaging, dataDir, artifacts, backupID string, res *RestoreResult) error {
	for _, name := range []string{dirRecordings, dirTranscripts, dirMemory, dirArtifacts} {
		src := filepath.Join(staging, name)
		dst := filepath.Join(dataDir, name)
		if name == dirArtifacts {
			src, dst = filepath.Join(artStaging, name), artifacts
		}
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("replace empty %s: %w", dst, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("install %s: %w", dst, err)
		}
	}
	if cfg := filepath.Join(staging, ConfigFile); fileExists(cfg) {
		dir := filepath.Join(dataDir, store.BackupsDir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		res.ConfigCopy = filepath.Join(dir, "restored-"+backupID+"-config.yaml")
		_ = os.Remove(res.ConfigCopy)
		if err := os.Rename(cfg, res.ConfigCopy); err != nil {
			return err
		}
	}
	if err := os.Rename(filepath.Join(staging, DBFile), filepath.Join(dataDir, store.DBFile)); err != nil {
		return fmt.Errorf("install database: %w", err)
	}
	return store.FsyncDir(dataDir)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
