package artifacts

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/s3client"
)

// LocalFromConfig returns the local store of a configuration: artifacts.local_dir (default <data_dir>/artifacts) for
// attachments, <data_dir>/recordings and <data_dir>/transcripts. With the S3 backend these directories still hold
// the spool: recordings being shipped and transcripts being received.
func LocalFromConfig(c *config.Config) *Local {
	att := c.Artifacts.LocalDir
	if att == "" {
		att = filepath.Join(c.DataDir, "artifacts")
	}
	return NewLocal(map[string]string{NSAttachments: att, NSRecordings: filepath.Join(c.DataDir, NSRecordings),
		NSTranscripts: filepath.Join(c.DataDir, NSTranscripts)})
}

// FromConfig returns the configured artifact store (artifacts.backend).
func FromConfig(c *config.Config) (Store, error) {
	switch c.Artifacts.Backend {
	case "", "local":
		return LocalFromConfig(c), nil
	case "s3":
		return S3FromConfig(c)
	}
	return nil, fmt.Errorf("unknown artifacts.backend %q", c.Artifacts.Backend)
}

// S3FromConfig returns the store in the artifacts.s3 bucket, whatever the configured backend (the migration command
// uses it to move artifacts into the bucket before the backend is switched).
func S3FromConfig(c *config.Config) (*S3, error) {
	s := c.Artifacts.S3
	if s.Bucket == "" || s.Endpoint == "" {
		return nil, errors.New("artifacts.s3.bucket and artifacts.s3.endpoint are not configured")
	}
	if err := ValidatePrefix(s.Prefix); err != nil {
		return nil, fmt.Errorf("artifacts.s3.prefix: %w", err)
	}
	cl, err := NewS3Client("artifacts.s3", s, c.IsProduction())
	if err != nil {
		return nil, err
	}
	return NewS3(cl, s.Bucket, s.Prefix), nil
}

// NewS3Client builds an S3 client from a configuration block (field names it in errors, e.g. "backups.s3"). The keys
// are read from their files here; they are never logged or returned in an error.
func NewS3Client(field string, s config.S3Config, production bool) (*s3client.Client, error) {
	ak, err := readSecretFile(s.AccessKeyFile)
	if err != nil {
		return nil, fmt.Errorf("%s.access_key_file: %w", field, err)
	}
	sk, err := readSecretFile(s.SecretKeyFile)
	if err != nil {
		return nil, fmt.Errorf("%s.secret_key_file: %w", field, err)
	}
	var ca string
	if s.CAFile != "" {
		pem, err := os.ReadFile(s.CAFile)
		if err != nil {
			return nil, fmt.Errorf("%s.ca_file: %w", field, err)
		}
		ca = string(pem)
	}
	cl, err := s3client.New(s3client.Config{Endpoint: s.Endpoint, Region: s.Region, Bucket: s.Bucket, PathStyle: s.UsePathStyle, AccessKeyID: ak,
		SecretAccessKey: sk, CABundlePEM: ca, AllowPrivateNetwork: s.AllowPrivateNetwork, AllowInsecureHTTP: s.InsecureAllowed && !production})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	return cl, nil
}

// readSecretFile reads a small regular file holding one secret value.
func readSecretFile(p string) (string, error) {
	if p == "" {
		return "", errors.New("not set")
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", p)
	}
	f, err := os.Open(p) //nolint:gosec // an operator-configured secret file
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", err
	}
	if len(b) > 4096 {
		return "", fmt.Errorf("%s is larger than 4096 bytes", p)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("%s is empty", p)
	}
	return v, nil
}

// ValidatePrefix accepts "" or a slash-terminated prefix of safe characters, such as spaceman/artifacts/.
func ValidatePrefix(p string) error {
	if p == "" {
		return nil
	}
	if len(p) > 256 || !strings.HasSuffix(p, "/") || strings.HasPrefix(p, "/") || strings.Contains(p, "//") {
		return errors.New("prefix must be empty or like spaceman/ (no leading slash, ends with a slash)")
	}
	for _, c := range p {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("-_./", c) {
			return fmt.Errorf("prefix contains %q; use letters, digits, - _ . /", c)
		}
	}
	for _, part := range strings.Split(strings.TrimSuffix(p, "/"), "/") {
		if part == "." || part == ".." {
			return errors.New("prefix must not contain . or .. segments")
		}
	}
	return nil
}
