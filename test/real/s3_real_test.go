//go:build real

package real

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/s3client"
)

// s3Image is MinIO pinned by digest. Docker Hub no longer serves minio/minio (pulls are denied since MinIO stopped
// publishing images), so this is Chainguard's MinIO build: cgr.dev/chainguard/minio:latest as pulled on 2026-11-03,
// MinIO RELEASE.2026-10-23T20-06-51Z (commit 76c06ff1d2cc), linux/arm64 and linux/amd64 index digest below.
// Override with SPACEMAN_S3_IMAGE (always pin a digest).
const s3Image = "cgr.dev/chainguard/minio@sha256:090010a3f73f6aa2dff606c61cd0de3640b22748abe19f4e48cce6dc9de7a53f"

const s3Bucket = "spaceman-real-backups"

// s3Minio is one throwaway MinIO container started by this test.
type s3Minio struct {
	id       string // the container ID this test created (the only one it removes)
	endpoint string // https://127.0.0.1:<random port>
	caPEM    []byte
	access   string
	secret   string
}

func s3RandHex(t *testing.T, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// s3Certs writes a throwaway CA and a server certificate for 127.0.0.1 in MinIO's certs-dir layout and returns the CA.
func s3Certs(t *testing.T, dir string) []byte {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "spaceman real-S3 test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	must(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	must(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	must(t, err)
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	must(t, err)
	must(t, os.MkdirAll(dir, 0o755))
	// The container runs as UID 65532 and must read both files; the key is a throwaway for this test only.
	must(t, os.WriteFile(filepath.Join(dir, "public.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "private.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o644))
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

// s3Start runs MinIO with TLS on a random loopback port. Credentials are generated here and reach the container only
// through a 0600 env file (never argv or logs).
func s3Start(t *testing.T, dir string) *s3Minio {
	t.Helper()
	image := s3Image
	if v := os.Getenv("SPACEMAN_S3_IMAGE"); v != "" {
		image = v
	}
	if !strings.Contains(image, "@sha256:") {
		t.Fatalf("SPACEMAN_S3_IMAGE must be pinned by digest, got %q", image)
	}
	m := &s3Minio{access: "sm" + s3RandHex(t, 9), secret: s3RandHex(t, 24)}
	certs := filepath.Join(dir, "minio-certs")
	m.caPEM = s3Certs(t, certs)
	envFile := filepath.Join(dir, "minio.env")
	must(t, os.WriteFile(envFile, []byte("MINIO_ROOT_USER="+m.access+"\nMINIO_ROOT_PASSWORD="+m.secret+"\n"), 0o600))
	out, err := exec.Command("docker", "run", "-d", "--env-file", envFile, "-p", "127.0.0.1::9000",
		"-v", certs+":/certs:ro", "--label", "spaceman.test=real-s3", image,
		"server", "/tmp/data", "--certs-dir", "/certs", "--address", ":9000").CombinedOutput()
	_ = os.Remove(envFile)
	if err != nil {
		t.Fatalf("docker run minio: %v\n%s", err, out)
	}
	m.id = strings.TrimSpace(string(out))
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", "--tail", "40", m.id).CombinedOutput()
			t.Logf("---- minio log ----\n%s", logs)
		}
		// Remove only the container this test created, with its anonymous volumes.
		if b, err := exec.Command("docker", "rm", "-f", "-v", m.id).CombinedOutput(); err != nil {
			t.Logf("docker rm %s: %v %s", m.id, err, b)
		}
	})
	pb, err := exec.Command("docker", "port", m.id, "9000/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	hostPort := strings.TrimSpace(strings.Split(string(pb), "\n")[0])
	_, port, err := net.SplitHostPort(hostPort)
	must(t, err)
	m.endpoint = "https://127.0.0.1:" + port

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(m.caPEM)
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	eventually(t, 60*time.Second, "minio ready", func() bool {
		res, err := hc.Get(m.endpoint + "/minio/health/ready")
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == 200
	})
	m.makeBucket(t, hc, s3Bucket)
	return m
}

// makeBucket creates a bucket with a SigV4-signed PUT (AWS Signature Version 4, path style). SpaceMan's client never
// creates buckets (operators do), so the test signs this one request itself.
func (m *s3Minio) makeBucket(t *testing.T, hc *http.Client, bucket string) {
	t.Helper()
	const region, empty = "us-east-1", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	now := time.Now().UTC()
	amz, day := now.Format("20060102T150405Z"), now.Format("20060102")
	host := strings.TrimPrefix(m.endpoint, "https://")
	canon := strings.Join([]string{"PUT", "/" + bucket, "", "host:" + host, "x-amz-content-sha256:" + empty, "x-amz-date:" + amz, "",
		"host;x-amz-content-sha256;x-amz-date", empty}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	sum := sha256.Sum256([]byte(canon))
	sts := "AWS4-HMAC-SHA256\n" + amz + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	mac := func(k []byte, s string) []byte { h := hmac.New(sha256.New, k); h.Write([]byte(s)); return h.Sum(nil) }
	k := mac(mac(mac(mac([]byte("AWS4"+m.secret), day), region), "s3"), "aws4_request")
	req, _ := http.NewRequest(http.MethodPut, m.endpoint+"/"+bucket, nil)
	req.Header.Set("x-amz-date", amz)
	req.Header.Set("x-amz-content-sha256", empty)
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=%s",
		m.access, scope, hex.EncodeToString(mac(k, sts))))
	res, err := hc.Do(req)
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("create bucket: HTTP %d %s", res.StatusCode, b)
	}
}

// client is SpaceMan's own S3 client for the test bucket (private network allowed: the endpoint is loopback).
func (m *s3Minio) client(t *testing.T) *s3client.Client {
	t.Helper()
	c, err := s3client.New(s3client.Config{Endpoint: m.endpoint, Bucket: s3Bucket, PathStyle: true, AccessKeyID: m.access,
		SecretAccessKey: m.secret, CABundlePEM: string(m.caPEM), AllowPrivateNetwork: true})
	must(t, err)
	return c
}

// s3Central is one `spaceman serve` process of this test.
type s3Central struct {
	url, cfg, root string
	cmd            *exec.Cmd
}

func s3WriteConfig(t *testing.T, dir, name, dataDir string, m *s3Minio) (cfg, url string) {
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
backups:
  target: s3
  s3:
    endpoint: %s
    region: us-east-1
    bucket: %s
    prefix: real/
    access_key_file: %s/s3-access-key
    secret_key_file: %s/s3-secret-key
    use_path_style: true
    allow_private_network: true
    ca_file: %s/s3-ca.pem
`, dataDir, url, dir, port, mport, m.endpoint, s3Bucket, dir, dir, dir)), 0o600))
	return cfg, url
}

func (c *s3Central) start(t *testing.T, bin, logFile string) {
	t.Helper()
	c.cmd = start(t, logFile, bin+"/spaceman", "-c", c.cfg, "serve")
	waitHTTP(t, c.url+"/api/v1/system/info")
}

func (c *s3Central) stop() {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Signal(syscall.SIGTERM)
		_ = c.cmd.Wait()
		c.cmd = nil
	}
}

func (c *s3Central) api(t *testing.T, method, path string, body, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.url+"/api/v1"+path, rd)
	req.Header.Set("Authorization", "Bearer "+c.root)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if out != nil {
		_ = json.Unmarshal(b, out)
	}
	if res.StatusCode >= 300 {
		t.Logf("%s %s: %d %s", method, path, res.StatusCode, b)
	}
	return res.StatusCode
}

// s3Spaceman runs the spaceman CLI and returns its output, failing on an error unless wantErr.
func s3Spaceman(t *testing.T, bin, cfg string, wantErr bool, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin+"/spaceman", append([]string{"-c", cfg}, args...)...)
	cmd.Env = cleanEnv()
	out, err := cmd.CombinedOutput()
	if (err != nil) != wantErr {
		t.Fatalf("spaceman %v: err=%v (want error %v)\n%s", args, err, wantErr, out)
	}
	return string(out)
}

// TestRealS3BackupRestore (SM-STORE-002/003/004/005/006, SM-VER-002): SpaceMan's backups against a real S3 service
// (MinIO in a throwaway container, TLS with a test CA, path-style, generated credentials). Central backs up through
// the API and the CLI; the bucket holds only sealed objects plus the manifest; list and verify read from the bucket;
// a tampered object fails verification; central is stopped and the backup is restored offline from the bucket into
// an empty data directory; the restored central serves the same records. Opt-in: SPACEMAN_REAL_S3=1 and Docker.
func TestRealS3BackupRestore(t *testing.T) {
	if os.Getenv("SPACEMAN_REAL_S3") != "1" {
		t.Skip("set SPACEMAN_REAL_S3=1 (and run Docker) to back up to and restore from a real MinIO container")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not available")
	}
	dir, err := os.MkdirTemp("/tmp", "sms3-") // under /tmp: shared with Docker Desktop's VM for the certs mount
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

	// 0. SpaceMan's S3 client against the real server (what internal/s3client TestInterop checks, here over TLS with a
	// CA bundle): SigV4 accepted, a key needing URI encoding, a 3 MiB round trip, Head, List, a wrong payload hash
	// refused by the server, Delete.
	{
		cl := m.client(t)
		data := make([]byte, 3<<20)
		_, _ = rand.Read(data)
		sum := sha256.Sum256(data)
		key := "interop/dir with space/obj+1.bin"
		must(t, cl.Put(ctx, key, bytes.NewReader(data), int64(len(data)), hex.EncodeToString(sum[:])))
		if n, err := cl.Head(ctx, key); err != nil || n != int64(len(data)) {
			t.Fatalf("head %d %v", n, err)
		}
		rc, _, err := cl.Get(ctx, key)
		must(t, err)
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, data) {
			t.Fatal("S3 round trip mismatch")
		}
		if objs, err := cl.List(ctx, "interop/"); err != nil || len(objs) != 1 || objs[0].Key != key {
			t.Fatalf("list %+v %v", objs, err)
		}
		bad := sha256.Sum256([]byte("not the body"))
		if err := cl.Put(ctx, "interop/bad-hash", bytes.NewReader([]byte("body")), 4, hex.EncodeToString(bad[:])); err == nil {
			t.Fatal("the server accepted an upload whose payload hash does not match")
		}
		must(t, cl.Delete(ctx, key))
		if _, err := cl.Head(ctx, key); err == nil {
			t.Fatal("object still there after delete")
		}
		// The default (private networks refused) still refuses this loopback endpoint.
		strict, err := s3client.New(s3client.Config{Endpoint: m.endpoint, Bucket: s3Bucket, PathStyle: true, AccessKeyID: m.access,
			SecretAccessKey: m.secret, CABundlePEM: string(m.caPEM)})
		must(t, err)
		if _, err := strict.Head(ctx, "interop/none"); err == nil || !strings.Contains(err.Error(), "allow_private_network is off") {
			t.Fatalf("loopback endpoint not refused by default: %v", err)
		}
		// Without the CA bundle the server's certificate is not trusted (verification is never disabled).
		noCA, err := s3client.New(s3client.Config{Endpoint: m.endpoint, Bucket: s3Bucket, PathStyle: true, AccessKeyID: m.access,
			SecretAccessKey: m.secret, AllowPrivateNetwork: true})
		must(t, err)
		if _, err := noCA.Head(ctx, "interop/none"); err == nil || !strings.Contains(err.Error(), "certificate") {
			t.Fatalf("untrusted certificate accepted: %v", err)
		}
	}

	// 1. Central with records worth restoring.
	c := &s3Central{}
	c.cfg, c.url = s3WriteConfig(t, dir, "central", dir+"/central", m)
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
	if st := c.api(t, "POST", "/nodes", map[string]any{"name": "s3-node"}, &node); st != 201 {
		t.Fatalf("create node: %d", st)
	}
	var sp struct{ ID string }
	if st := c.api(t, "POST", "/spaces", map[string]any{"slug": "s3-space", "name": "Backed up", "node_id": node.Node.ID}, &sp); st != 201 {
		t.Fatalf("create space: %d", st)
	}
	accountSecret := "sm-real-s3-canary-" + s3RandHex(t, 16) // must never reach the bucket in plaintext
	var acc struct{ ID string }
	if st := c.api(t, "POST", "/accounts", map[string]any{"name": "s3-account", "kind": "anthropic_api_key", "secret": accountSecret}, &acc); st != 201 {
		t.Fatalf("create account: %d", st)
	}

	// 2. A backup made by central (API), and one made by the CLI while central runs.
	var item struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if st := c.api(t, "POST", "/system/backups", nil, &item); st != 202 {
		t.Fatalf("create backup: %d", st)
	}
	var status struct {
		Target      string `json:"target"`
		Location    string `json:"location"`
		TargetError string `json:"target_error"`
		Items       []struct {
			ID, State, Error string
			Stored           bool  `json:"stored"`
			StoredBytes      int64 `json:"stored_bytes"`
		} `json:"items"`
	}
	eventually(t, 60*time.Second, "API backup stored in S3", func() bool {
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
	if status.Target != "s3" || status.Location != "s3://"+s3Bucket+"/real/" || status.TargetError != "" {
		t.Fatalf("backup status: target %q location %q error %q", status.Target, status.Location, status.TargetError)
	}
	var manifest struct {
		ID     string `json:"id"`
		Counts struct {
			Spaces int64 `json:"spaces"`
		} `json:"counts"`
		Files []json.RawMessage `json:"files"`
	}
	must(t, json.Unmarshal([]byte(s3Spaceman(t, bin, c.cfg, false, "backup", "create", "--json")), &manifest))
	if manifest.Counts.Spaces != 1 || len(manifest.Files) == 0 {
		t.Fatalf("CLI backup manifest: %+v", manifest)
	}
	cliID := manifest.ID

	// 3. The bucket really holds both backups: sealed objects and manifest.sha256 (written last) under the prefix.
	objs, err := m.client(t).List(ctx, "real/")
	must(t, err)
	per := map[string]int{}
	complete := map[string]bool{}
	for _, o := range objs {
		id, rel, _ := strings.Cut(strings.TrimPrefix(o.Key, "real/"), "/")
		per[id]++
		if rel == "manifest.sha256" {
			complete[id] = true
		}
		// No plaintext secret in any object (only ciphertext and the plaintext metadata manifest reach the bucket).
		rc, _, err := m.client(t).Get(ctx, o.Key)
		must(t, err)
		b, err := io.ReadAll(rc)
		rc.Close()
		must(t, err)
		if bytes.Contains(b, []byte(accountSecret)) || bytes.Contains(b, []byte(m.secret)) || bytes.Contains(b, []byte(c.root)) {
			t.Fatalf("object %s contains a plaintext secret", o.Key)
		}
	}
	if !complete[item.ID] || !complete[cliID] || len(per) != 2 {
		t.Fatalf("bucket objects per backup %v, complete %v", per, complete)
	}

	// 4. List and verify read from the bucket.
	var listed []struct {
		ID       string `json:"id"`
		Complete bool   `json:"complete"`
	}
	must(t, json.Unmarshal([]byte(s3Spaceman(t, bin, c.cfg, false, "backup", "list", "--json")), &listed))
	if len(listed) != 2 || listed[0].ID != cliID || !listed[0].Complete || listed[1].ID != item.ID {
		t.Fatalf("backup list: %+v (want %s then %s)", listed, cliID, item.ID)
	}
	var rep struct {
		OK          bool     `json:"ok"`
		Keyed       bool     `json:"keyed"`
		Files       int      `json:"files"`
		AuditEvents int64    `json:"audit_events"`
		Problems    []string `json:"problems"`
	}
	must(t, json.Unmarshal([]byte(s3Spaceman(t, bin, c.cfg, false, "backup", "verify", "--json", cliID)), &rep))
	if !rep.OK || !rep.Keyed || rep.Files == 0 || rep.AuditEvents == 0 {
		t.Fatalf("verify from S3: %+v", rep)
	}
	if st := c.api(t, "POST", "/system/backups/"+item.ID+"/verify", nil, nil); st != 202 {
		t.Fatalf("API verify: %d", st)
	}
	eventually(t, 60*time.Second, "API verification ok", func() bool {
		var s struct {
			Items []struct {
				ID          string `json:"id"`
				VerifyState string `json:"verify_state"`
				VerifyError string `json:"verify_error"`
			} `json:"items"`
		}
		c.api(t, "GET", "/system/backups", nil, &s)
		for _, it := range s.Items {
			if it.ID == item.ID {
				if it.VerifyState == "failed" {
					t.Fatalf("API verify failed: %s", it.VerifyError)
				}
				return it.VerifyState == "ok"
			}
		}
		return false
	})

	// 5. A sealed object changed in the bucket fails verification (the API backup; the CLI one stays intact).
	for _, o := range objs {
		if strings.HasPrefix(o.Key, "real/"+item.ID+"/") && !strings.HasSuffix(o.Key, "/manifest.json") && !strings.HasSuffix(o.Key, "/manifest.sha256") {
			junk := []byte("tampered object " + s3RandHex(t, 8))
			sum := sha256.Sum256(junk)
			must(t, m.client(t).Put(ctx, o.Key, bytes.NewReader(junk), int64(len(junk)), hex.EncodeToString(sum[:])))
			break
		}
	}
	if out := s3Spaceman(t, bin, c.cfg, true, "backup", "verify", item.ID); !strings.Contains(out, "backup: FAILED") {
		t.Fatalf("tampered backup verified:\n%s", out)
	}

	// 6. Offline restore from the bucket into an empty data directory, then serve it.
	c.stop()
	r := &s3Central{root: c.root}
	r.cfg, r.url = s3WriteConfig(t, dir, "restored", dir+"/restored", m)
	out := s3Spaceman(t, bin, r.cfg, false, "backup", "restore", cliID)
	if !strings.Contains(out, "restored backup "+cliID) || !strings.Contains(out, "unlocks the keyring") {
		t.Fatalf("restore output:\n%s", out)
	}
	r.start(t, bin, filepath.Join(dir, "restored.log"))
	t.Cleanup(r.stop)
	var notice struct {
		Restored bool `json:"restored"`
		State    struct {
			BackupID     string   `json:"backup_id"`
			PausedSpaces []string `json:"paused_spaces"`
		} `json:"state"`
	}
	if st := r.api(t, "GET", "/system/restore", nil, &notice); st != 200 || !notice.Restored || notice.State.BackupID != cliID {
		t.Fatalf("restore notice: %d %+v", st, notice)
	}
	var space struct {
		ID, Slug, Name, State string
		NodeID                string `json:"node_id"`
	}
	if st := r.api(t, "GET", "/spaces/"+sp.ID, nil, &space); st != 200 || space.Slug != "s3-space" || space.NodeID != node.Node.ID {
		t.Fatalf("restored space: %d %+v", st, space)
	}
	var gotNode struct{ ID, Name string }
	if st := r.api(t, "GET", "/nodes/"+node.Node.ID, nil, &gotNode); st != 200 || gotNode.Name != "s3-node" {
		t.Fatalf("restored node: %d %+v", st, gotNode)
	}
	var gotAcc struct{ ID, Name, Kind string }
	if st := r.api(t, "GET", "/accounts/"+acc.ID, nil, &gotAcc); st != 200 || gotAcc.Name != "s3-account" {
		t.Fatalf("restored account: %d %+v", st, gotAcc)
	}
	// The restored database's audit chain still verifies, read straight from the bucket again after the restore.
	must(t, json.Unmarshal([]byte(s3Spaceman(t, bin, r.cfg, false, "backup", "verify", "--json", cliID)), &rep))
	if !rep.OK {
		t.Fatalf("verify after restore: %+v", rep)
	}
	t.Logf("real S3 (%s): API backup %s and CLI backup %s stored (%d objects), listed, verified (%d files, %d audit entries), "+
		"tamper detected, restored offline into a fresh data directory and served (space %s state %s, paused by restore %v)",
		m.endpoint, item.ID, cliID, len(objs), rep.Files, rep.AuditEvents, space.ID, space.State, notice.State.PausedSpaces)
}
