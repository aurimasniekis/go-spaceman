package backup

import (
	"fmt"
	"path/filepath"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// TargetFromConfig builds the configured target. Local backups default to <data_dir>/backups; S3 backups are staged in
// <data_dir>/backups/.staging. S3 keys are read from their files here and never logged.
func TargetFromConfig(c *config.Config) (Target, error) {
	b := c.Backups
	switch b.Target {
	case "", "local":
		dir := b.LocalDir
		if dir == "" {
			dir = filepath.Join(c.DataDir, store.BackupsDir)
		}
		return LocalTarget{Dir: dir}, nil
	case "s3":
		s := b.S3
		if err := ValidatePrefix(s.Prefix); err != nil {
			return nil, fmt.Errorf("backups.s3.prefix: %w", err)
		}
		cl, err := artifacts.NewS3Client("backups.s3", s, c.IsProduction())
		if err != nil {
			return nil, err
		}
		return S3Target{Objects: cl, Bucket: s.Bucket, Prefix: s.Prefix, Staging: filepath.Join(c.DataDir, store.BackupsDir, ".staging")}, nil
	}
	return nil, fmt.Errorf("unknown backups.target %q", b.Target)
}
