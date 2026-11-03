package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/s3client"
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
		ak, err := readSecretFile(s.AccessKeyFile)
		if err != nil {
			return nil, fmt.Errorf("backups.s3.access_key_file: %w", err)
		}
		sk, err := readSecretFile(s.SecretKeyFile)
		if err != nil {
			return nil, fmt.Errorf("backups.s3.secret_key_file: %w", err)
		}
		var ca string
		if s.CAFile != "" {
			pem, err := os.ReadFile(s.CAFile)
			if err != nil {
				return nil, fmt.Errorf("backups.s3.ca_file: %w", err)
			}
			ca = string(pem)
		}
		cl, err := s3client.New(s3client.Config{Endpoint: s.Endpoint, Region: s.Region, Bucket: s.Bucket, PathStyle: s.UsePathStyle, AccessKeyID: ak,
			SecretAccessKey: sk, CABundlePEM: ca, AllowPrivateNetwork: s.AllowPrivateNetwork, AllowInsecureHTTP: s.InsecureAllowed && !c.IsProduction()})
		if err != nil {
			return nil, fmt.Errorf("backups.s3: %w", err)
		}
		return S3Target{Objects: cl, Bucket: s.Bucket, Prefix: s.Prefix, Staging: filepath.Join(c.DataDir, store.BackupsDir, ".staging")}, nil
	}
	return nil, fmt.Errorf("unknown backups.target %q", b.Target)
}

func readSecretFile(p string) (string, error) {
	b, err := readSmall(p, 4096)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("%s is empty", p)
	}
	return v, nil
}
