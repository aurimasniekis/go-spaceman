package artifacts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
)

// MigrateOptions configure Migrate.
type MigrateOptions struct {
	// TmpDir holds the spool file of the object being copied (inside the data directory).
	TmpDir string
	// RemoveSource deletes each artifact from the source once it is in the destination with the same size.
	RemoveSource bool
	// DryRun only counts what would be copied.
	DryRun bool
	// Progress, when set, is called after each artifact (copied or already present).
	Progress func(key string, copied bool)
}

// MigrateReport is the outcome of Migrate.
type MigrateReport struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Copied  int    `json:"copied"`
	Bytes   int64  `json:"bytes" doc:"Bytes copied"`
	Present int    `json:"present" doc:"Artifacts already in the destination with the same size"`
	Removed int    `json:"removed" doc:"Artifacts removed from the source"`
	DryRun  bool   `json:"dry_run"`
}

// Migrate copies every artifact of src that dst lacks (or holds with another size) into dst: attachments (their
// content address checked), recording segments and transcripts. It is idempotent, so an interrupted run is simply
// repeated. It must run while central is stopped (the command takes the data-directory lock), so nothing writes
// either store meanwhile.
func Migrate(ctx context.Context, src, dst Store, o MigrateOptions) (*MigrateReport, error) {
	rep := &MigrateReport{From: src.Kind() + " (" + src.Location() + ")", To: dst.Kind() + " (" + dst.Location() + ")", DryRun: o.DryRun}
	if src.Kind() == dst.Kind() && src.Location() == dst.Location() {
		return nil, errors.New("the source and the destination are the same store")
	}
	for _, ns := range Namespaces {
		objs, err := src.List(ctx, ns+"/")
		if err != nil {
			return rep, fmt.Errorf("list %s: %w", ns, err)
		}
		for _, info := range objs {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			have, err := dst.Stat(ctx, info.Key)
			copied := false
			switch {
			case err == nil && have.Size == info.Size:
				rep.Present++
			case err != nil && !errors.Is(err, fs.ErrNotExist):
				return rep, fmt.Errorf("check %s: %w", info.Key, err)
			case o.DryRun:
				copied = true
			default:
				if _, err := Copy(ctx, src, dst, info.Key, o.TmpDir); err != nil {
					return rep, err
				}
				copied = true
			}
			if copied {
				rep.Copied++
				rep.Bytes += info.Size
			}
			if o.Progress != nil {
				o.Progress(info.Key, copied)
			}
			if o.RemoveSource && !o.DryRun {
				if err := src.Delete(ctx, info.Key); err != nil {
					return rep, fmt.Errorf("remove %s from the source: %w", info.Key, err)
				}
				rep.Removed++
			}
		}
	}
	return rep, nil
}
