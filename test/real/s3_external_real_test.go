//go:build real

package real

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/s3client"
)

// s3External is a real S3-compatible service the operator points the test at (AWS S3, Cloudflare R2, …).
type s3External struct {
	endpoint, region, bucket, access, secret string
	pathStyle                                bool
	// caFile and allowPrivate serve a self-hosted service (MinIO on a LAN): a private CA, a private address.
	caFile       string
	allowPrivate bool
}

func s3ExternalFromEnv(t *testing.T) s3External {
	t.Helper()
	x := s3External{endpoint: os.Getenv("SPACEMAN_REAL_S3_ENDPOINT"), region: os.Getenv("SPACEMAN_REAL_S3_REGION"),
		bucket: os.Getenv("SPACEMAN_REAL_S3_BUCKET"), access: os.Getenv("SPACEMAN_REAL_S3_ACCESS_KEY_ID"),
		secret: os.Getenv("SPACEMAN_REAL_S3_SECRET_ACCESS_KEY"), pathStyle: os.Getenv("SPACEMAN_REAL_S3_PATH_STYLE") == "1",
		caFile: os.Getenv("SPACEMAN_REAL_S3_CA_FILE"), allowPrivate: os.Getenv("SPACEMAN_REAL_S3_ALLOW_PRIVATE_NETWORK") == "1"}
	if os.Getenv("SPACEMAN_REAL_S3_EXTERNAL") != "1" || x.endpoint == "" || x.bucket == "" || x.access == "" || x.secret == "" {
		t.Skip("set SPACEMAN_REAL_S3_EXTERNAL=1 with SPACEMAN_REAL_S3_ENDPOINT, _BUCKET, _ACCESS_KEY_ID and _SECRET_ACCESS_KEY " +
			"(optional _REGION, default us-east-1; _PATH_STYLE=1; _CA_FILE and _ALLOW_PRIVATE_NETWORK=1 for a self-hosted service) " +
			"to run against a real S3 bucket")
	}
	if x.region == "" {
		x.region = "us-east-1"
	}
	return x
}

func (x s3External) client(t *testing.T) *s3client.Client {
	t.Helper()
	ca := ""
	if x.caFile != "" {
		b, err := os.ReadFile(x.caFile)
		must(t, err)
		ca = string(b)
	}
	c, err := s3client.New(s3client.Config{Endpoint: x.endpoint, Region: x.region, Bucket: x.bucket, PathStyle: x.pathStyle,
		AccessKeyID: x.access, SecretAccessKey: x.secret, CABundlePEM: ca, AllowPrivateNetwork: x.allowPrivate})
	must(t, err)
	return c
}

// centralS3 is the matching backups.s3 configuration for central (the key files are written by the caller).
func (x s3External) centralS3(dir, prefix string) string {
	cfg := fmt.Sprintf(`    endpoint: %s
    region: %s
    bucket: %s
    prefix: %s
    access_key_file: %s/s3-access-key
    secret_key_file: %s/s3-secret-key
    use_path_style: %t
`, x.endpoint, x.region, x.bucket, prefix, dir, dir, x.pathStyle)
	if x.allowPrivate {
		cfg += "    allow_private_network: true\n"
	}
	if x.caFile != "" {
		cfg += fmt.Sprintf("    ca_file: %s\n", x.caFile)
	}
	return cfg
}

// TestRealS3External (SM-STORE-002, SM-BAK-*, SM-VER-002): the S3 artifact store and S3 backups against a real
// service such as AWS S3 or Cloudflare R2, with the operator's own bucket. Everything happens under a fresh random
// prefix (spaceman-real-test/<random>/), and the test deletes exactly the objects under that prefix afterwards, also
// when it fails. Opt-in: SPACEMAN_REAL_S3_EXTERNAL=1 and the bucket settings (see s3ExternalFromEnv).
func TestRealS3External(t *testing.T) {
	x := s3ExternalFromEnv(t)
	ctx := context.Background()
	prefix := "spaceman-real-test/" + s3RandHex(t, 8) + "/"
	cl := x.client(t)
	t.Cleanup(func() {
		objs, err := cl.List(context.Background(), prefix)
		if err != nil {
			t.Logf("cleanup: list %s: %v", prefix, err)
			return
		}
		for _, o := range objs {
			if err := cl.Delete(context.Background(), o.Key); err != nil {
				t.Logf("cleanup: delete %s: %v", o.Key, err)
			}
		}
		t.Logf("cleanup: removed %d objects under %s", len(objs), prefix)
	})

	// 1. The artifact store: content-addressed objects, payload hashes the service checks, range and verified reads.
	st := artifacts.NewS3(cl, x.bucket, prefix+"store/")
	data := make([]byte, 3<<20+17)
	_, _ = rand.Read(data)
	sum := sha256.Sum256(data)
	h := hex.EncodeToString(sum[:])
	key := artifacts.AttachmentKey(h)
	must(t, st.Put(ctx, key, bytes.NewReader(data), int64(len(data)), h))
	if info, err := st.Stat(ctx, key); err != nil || info.Size != int64(len(data)) {
		t.Fatalf("stat %+v %v", info, err)
	}
	o, err := st.Open(ctx, key)
	must(t, err)
	_, err = o.Seek(2<<20, io.SeekStart)
	must(t, err)
	tail, err := io.ReadAll(o)
	must(t, err)
	if !bytes.Equal(tail, data[2<<20:]) {
		t.Fatal("range read differs")
	}
	_, _ = o.Seek(0, io.SeekStart)
	v := artifacts.Verified(o, h)
	if all, err := io.ReadAll(v); err != nil || !bytes.Equal(all, data) {
		t.Fatalf("verified read: %v", err)
	}
	_ = v.Close()
	if err := st.Put(ctx, artifacts.TranscriptKey("run_0123456789abcdefghijklmnop"), strings.NewReader("abc"), 3, h); err == nil {
		t.Fatal("the service accepted content whose SHA-256 does not match")
	}
	if ls, err := st.List(ctx, artifacts.NSAttachments+"/"); err != nil || len(ls) != 1 || ls[0].Key != key {
		t.Fatalf("list %+v %v", ls, err)
	}
	must(t, st.Delete(ctx, key))
	if _, err := st.Stat(ctx, key); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("after delete: %v", err)
	}

	// 2. A real central backs up to the bucket: sealed objects and the manifest under the prefix, no plaintext secret.
	dir, err := os.MkdirTemp("/tmp", "sms3x-")
	must(t, err)
	t.Cleanup(func() {
		if os.Getenv("SM_KEEP") == "" {
			_ = os.RemoveAll(dir)
		}
	})
	bin := filepath.Join(dir, "bin")
	build(t, bin)
	must(t, os.WriteFile(filepath.Join(dir, "s3-access-key"), []byte(x.access), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "s3-secret-key"), []byte(x.secret), 0o600))
	port, mport := freePort(t), freePort(t)
	c := &s3Central{url: fmt.Sprintf("http://127.0.0.1:%d", port), cfg: filepath.Join(dir, "central.yaml")}
	must(t, os.WriteFile(c.cfg, []byte(fmt.Sprintf(`mode: development
data_dir: %s/central
public_url: %s
kek: {file: %s/kek}
http: {listen: "127.0.0.1:%d", management_listen: "127.0.0.1:%d"}
log: {level: debug, format: text}
backups:
  target: s3
  s3:
%s`, dir, c.url, dir, port, mport, x.centralS3(dir, prefix+"backups/"))), 0o600))
	run(t, bin+"/spaceman", "keys", "init-kek", "--out", dir+"/kek")
	for _, f := range strings.Fields(run(t, bin+"/spaceman", "-c", c.cfg, "admin", "bootstrap")) {
		if strings.HasPrefix(f, "spaceman_rt_") {
			c.root = f
		}
	}
	c.start(t, bin, filepath.Join(dir, "central.log"))
	t.Cleanup(c.stop)
	accountSecret := "sm-real-s3x-canary-" + s3RandHex(t, 16) // must never reach the bucket in plaintext
	var acc struct{ ID string }
	if st := c.api(t, "POST", "/accounts", map[string]any{"name": "s3x-account", "kind": "anthropic_api_key", "secret": accountSecret}, &acc); st != 201 {
		t.Fatalf("create account: %d", st)
	}
	var item struct{ ID string }
	if st := c.api(t, "POST", "/system/backups", nil, &item); st != 202 {
		t.Fatalf("start backup: %d", st)
	}
	var status struct {
		Target string `json:"target"`
		Items  []struct {
			ID, State, Error string
			Stored           bool `json:"stored"`
		} `json:"items"`
	}
	eventually(t, 120*time.Second, "backup stored in the bucket", func() bool {
		c.api(t, "GET", "/system/backups", nil, &status)
		for _, it := range status.Items {
			if it.ID == item.ID {
				if it.State == "failed" {
					t.Fatalf("backup failed: %s", it.Error)
				}
				return it.State == "succeeded" && it.Stored
			}
		}
		return false
	})
	objs, err := cl.List(ctx, prefix+"backups/")
	must(t, err)
	complete := false
	for _, o := range objs {
		if strings.HasSuffix(o.Key, item.ID+"/manifest.sha256") {
			complete = true
		}
		rc, _, err := cl.Get(ctx, o.Key)
		must(t, err)
		b, err := io.ReadAll(rc)
		rc.Close()
		must(t, err)
		if bytes.Contains(b, []byte(accountSecret)) || bytes.Contains(b, []byte(x.secret)) || bytes.Contains(b, []byte(c.root)) {
			t.Fatalf("object %s contains a plaintext secret", o.Key)
		}
	}
	if !complete || status.Target != "s3" {
		t.Fatalf("backup in the bucket: %d objects, manifest %v, target %q", len(objs), complete, status.Target)
	}
	verify := s3Spaceman(t, bin, c.cfg, false, "backup", "verify", item.ID) // read back from the bucket and checked
	t.Logf("backup %s stored and verified in s3://%s/%sbackups/ (%d objects): %s", item.ID, x.bucket, prefix, len(objs), strings.TrimSpace(verify))
}
