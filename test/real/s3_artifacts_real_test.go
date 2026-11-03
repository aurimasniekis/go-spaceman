//go:build real

package real

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
)

// s3ArtifactsConfig writes a central configuration whose artifacts live in the MinIO bucket under prefix (backups
// stay local).
func s3ArtifactsConfig(t *testing.T, dir, name, dataDir, prefix string, m *s3Minio) (cfg, url string) {
	t.Helper()
	port, mport := freePort(t), freePort(t)
	url = fmt.Sprintf("http://127.0.0.1:%d", port)
	cfg = filepath.Join(dir, name+".yaml")
	must(t, os.WriteFile(cfg, []byte(fmt.Sprintf(`mode: development
data_dir: %s
public_url: %s
kek: {file: %s/kek}
http: {listen: "127.0.0.1:%d", management_listen: "127.0.0.1:%d"}
log: {level: debug, format: text}
artifacts:
  backend: s3
  s3:
    endpoint: %s
    region: us-east-1
    bucket: %s
    prefix: %s
    access_key_file: %s/s3-access-key
    secret_key_file: %s/s3-secret-key
    use_path_style: true
    allow_private_network: true
    ca_file: %s/s3-ca.pem
`, dataDir, url, dir, port, mport, m.endpoint, s3Bucket, prefix, dir, dir, dir)), 0o600))
	return cfg, url
}

// raw sends a request with the central's root token and returns the status and body.
func (c *s3Central) raw(t *testing.T, req *http.Request) (int, []byte) {
	t.Helper()
	req.Header.Set("Authorization", "Bearer "+c.root)
	res, err := http.DefaultClient.Do(req)
	must(t, err)
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	must(t, err)
	return res.StatusCode, b
}

// TestRealS3Artifacts (SM-STORE-002, SM-ARCH-003, ADR-0048, SM-VER-002): the S3 artifact store against a real S3
// service (MinIO in a throwaway container, TLS with a test CA, path style, generated credentials). The store itself:
// content-addressed objects, payload hashes the server checks, range reads, listing, deletes. Then a real central with
// artifacts.backend: s3: an attachment uploaded through the API lands in the bucket under its SHA-256 and downloads
// whole and as a range; an offline backup copies it in; `spaceman artifacts migrate` moves it to the local folders
// and back; a backup restored into a fresh installation is uploaded with the migrate command and served from the
// bucket again. Opt-in: SPACEMAN_REAL_S3=1 and Docker.
func TestRealS3Artifacts(t *testing.T) {
	if os.Getenv("SPACEMAN_REAL_S3") != "1" {
		t.Skip("set SPACEMAN_REAL_S3=1 (and run Docker) to run the S3 artifact store against a real MinIO container")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not available")
	}
	dir, err := os.MkdirTemp("/tmp", "sms3a-") // under /tmp: shared with Docker Desktop's VM for the certs mount
	must(t, err)
	t.Cleanup(func() {
		if os.Getenv("SM_KEEP") == "" {
			_ = os.RemoveAll(dir)
		}
	})
	bin := filepath.Join(dir, "bin")
	build(t, bin)
	m := s3Start(t, dir)
	must(t, os.WriteFile(filepath.Join(dir, "s3-access-key"), []byte(m.access), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "s3-secret-key"), []byte(m.secret), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "s3-ca.pem"), m.caPEM, 0o600))
	ctx := context.Background()

	// 0. The store against the real server.
	{
		st := artifacts.NewS3(m.client(t), s3Bucket, "lib/")
		data := make([]byte, 3<<20+17)
		_, _ = rand.Read(data)
		sum := sha256.Sum256(data)
		h := hex.EncodeToString(sum[:])
		key := artifacts.AttachmentKey(h)
		must(t, st.Put(ctx, key, bytes.NewReader(data), int64(len(data)), h))
		info, err := st.Stat(ctx, key)
		if err != nil || info.Size != int64(len(data)) || info.ModTime.IsZero() {
			t.Fatalf("stat %+v %v", info, err)
		}
		o, err := st.Open(ctx, key)
		must(t, err)
		_, err = o.Seek(2<<20, io.SeekStart)
		must(t, err)
		tail, err := io.ReadAll(o)
		must(t, err)
		if !bytes.Equal(tail, data[2<<20:]) {
			t.Fatal("range read from MinIO differs")
		}
		_, _ = o.Seek(0, io.SeekStart)
		v := artifacts.Verified(o, h)
		all, err := io.ReadAll(v)
		if err != nil || !bytes.Equal(all, data) {
			t.Fatalf("verified full read: %v", err)
		}
		_ = v.Close()
		if err := st.Put(ctx, artifacts.TranscriptKey("run_0123456789abcdefghijklmnop"), strings.NewReader("abc"), 3, h); err == nil {
			t.Fatal("MinIO accepted content whose SHA-256 does not match")
		}
		if ls, err := st.List(ctx, artifacts.NSAttachments+"/"); err != nil || len(ls) != 1 || ls[0].Key != key {
			t.Fatalf("list %+v %v", ls, err)
		}
		must(t, st.Delete(ctx, key))
		if _, err := st.Stat(ctx, key); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("after delete: %v", err)
		}
	}

	// 1. Central with artifacts in the bucket.
	c := &s3Central{}
	c.cfg, c.url = s3ArtifactsConfig(t, dir, "central", dir+"/central", "art/", m)
	run(t, bin+"/spaceman", "keys", "init-kek", "--out", dir+"/kek")
	for _, f := range strings.Fields(run(t, bin+"/spaceman", "-c", c.cfg, "admin", "bootstrap")) {
		if strings.HasPrefix(f, "spaceman_rt_") {
			c.root = f
		}
	}
	c.start(t, bin, filepath.Join(dir, "central.log"))
	t.Cleanup(c.stop)
	var node struct {
		Node struct{ ID string } `json:"node"`
	}
	if st := c.api(t, "POST", "/nodes", map[string]any{"name": "s3a-node"}, &node); st != 201 {
		t.Fatalf("create node: %d", st)
	}
	var sp struct{ ID string }
	if st := c.api(t, "POST", "/spaces", map[string]any{"slug": "s3a-space", "name": "Artifacts", "node_id": node.Node.ID}, &sp); st != 201 {
		t.Fatalf("create space: %d", st)
	}
	data := make([]byte, 1<<20)
	_, _ = rand.Read(data)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "blob.bin")
	must(t, err)
	_, _ = fw.Write(data)
	must(t, mw.Close())
	req, _ := http.NewRequest(http.MethodPost, c.url+"/api/v1/spaces/"+sp.ID+"/attachments", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	st, b := c.raw(t, req)
	var att struct{ ID, SHA256 string }
	if st != 201 || json.Unmarshal(b, &att) != nil || att.ID == "" {
		t.Fatalf("upload: %d %s", st, b)
	}
	objKey := "art/" + artifacts.AttachmentKey(att.SHA256)
	cl := m.client(t)
	if n, err := cl.Head(ctx, objKey); err != nil || n != int64(len(data)) {
		t.Fatalf("attachment object %s: %d %v", objKey, n, err)
	}
	content := c.url + "/api/v1/spaces/" + sp.ID + "/attachments/" + att.ID + "/content"
	req, _ = http.NewRequest(http.MethodGet, content, nil)
	if st, b := c.raw(t, req); st != 200 || !bytes.Equal(b, data) {
		t.Fatalf("download: %d (%d bytes)", st, len(b))
	}
	req, _ = http.NewRequest(http.MethodGet, content, nil)
	req.Header.Set("Range", "bytes=500000-500099")
	if st, b := c.raw(t, req); st != 206 || !bytes.Equal(b, data[500000:500100]) {
		t.Fatalf("range download: %d (%d bytes)", st, len(b))
	}
	if _, err := os.Stat(filepath.Join(dir, "central", "artifacts", "sha256")); !os.IsNotExist(err) {
		t.Fatalf("attachment content on the local disk: %v", err)
	}

	// 2. An offline backup copies the attachment out of the bucket.
	var manifest struct {
		ID            string `json:"id"`
		ArtifactStore struct {
			Kind     string `json:"kind"`
			Included bool   `json:"included"`
			Objects  int    `json:"objects"`
		} `json:"artifact_store"`
		Files []struct{ Path, Kind string } `json:"files"`
	}
	must(t, json.Unmarshal([]byte(s3Spaceman(t, bin, c.cfg, false, "backup", "create", "--json")), &manifest))
	found := false
	for _, f := range manifest.Files {
		found = found || f.Path == "artifacts/sha256/"+att.SHA256[:2]+"/"+att.SHA256
	}
	if manifest.ArtifactStore.Kind != "s3" || !manifest.ArtifactStore.Included || manifest.ArtifactStore.Objects != 1 || !found {
		t.Fatalf("backup manifest: %+v", manifest)
	}

	// 3. Offline migration to the local folders and back (central must be stopped: the command takes the lock).
	if out := s3Spaceman(t, bin, c.cfg, true, "artifacts", "migrate", "--to", "local"); !strings.Contains(out, "stop central") {
		t.Fatalf("migrate while central runs:\n%s", out)
	}
	c.stop()
	var rep struct{ Copied, Present, Removed int }
	must(t, json.Unmarshal([]byte(s3Spaceman(t, bin, c.cfg, false, "artifacts", "migrate", "--to", "local", "--json")), &rep))
	if rep.Copied != 1 {
		t.Fatalf("migrate to local: %+v", rep)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "central", "artifacts", "sha256", att.SHA256[:2], att.SHA256)); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("migrated attachment: %v", err)
	}
	must(t, json.Unmarshal([]byte(s3Spaceman(t, bin, c.cfg, false, "artifacts", "migrate", "--to", "s3", "--json", "--remove-source")), &rep))
	if rep.Copied != 0 || rep.Present != 1 || rep.Removed != 1 {
		t.Fatalf("migrate back: %+v", rep)
	}

	// 4. Restore the backup into a fresh installation with its own prefix, upload the restored artifacts with the
	// migrate command, and serve the attachment from the bucket.
	r := &s3Central{root: c.root}
	r.cfg, r.url = s3ArtifactsConfig(t, dir, "restored", dir+"/restored", "art2/", m)
	out := s3Spaceman(t, bin, r.cfg, false, "backup", "restore", filepath.Join(dir, "central", "backups", manifest.ID))
	if !strings.Contains(out, "artifacts migrate --to s3") {
		t.Fatalf("restore output does not name the migration:\n%s", out)
	}
	must(t, json.Unmarshal([]byte(s3Spaceman(t, bin, r.cfg, false, "artifacts", "migrate", "--to", "s3", "--json", "--remove-source")), &rep))
	if rep.Copied != 1 || rep.Removed != 1 {
		t.Fatalf("migrate the restored artifacts: %+v", rep)
	}
	if _, err := cl.Head(ctx, "art2/"+artifacts.AttachmentKey(att.SHA256)); err != nil {
		t.Fatalf("restored attachment not in the bucket: %v", err)
	}
	r.start(t, bin, filepath.Join(dir, "restored.log"))
	t.Cleanup(r.stop)
	req, _ = http.NewRequest(http.MethodGet, r.url+"/api/v1/spaces/"+sp.ID+"/attachments/"+att.ID+"/content", nil)
	if st, b := r.raw(t, req); st != 200 || !bytes.Equal(b, data) {
		t.Fatalf("restored download: %d (%d bytes)", st, len(b))
	}
	// No credential reaches the logs.
	for _, f := range []string{"central.log", "restored.log"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		must(t, err)
		if bytes.Contains(b, []byte(m.secret)) || bytes.Contains(b, []byte(m.access)) {
			t.Fatalf("%s contains an S3 credential", f)
		}
	}
	t.Logf("real S3 (%s): artifact store contract (3 MiB, range, hash refusal, list, delete); attachment %s uploaded to %s, downloaded whole "+
		"and as a range; backup %s copied it in; migrated to local and back; restored into a fresh installation, migrated to art2/ and served",
		m.endpoint, att.ID, objKey, manifest.ID)
}
