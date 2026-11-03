package backup_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/s3client"
	"github.com/aurimasniekis/go-spaceman/internal/s3client/s3test"
)

// SM-STORE-002/003, ADR-0048: with the S3 artifact store a backup still holds every artifact. Objects are copied
// from the bucket into the backup and sealed like local files, in the same layout, so the restore writes them to the
// local folders; a segment that is still in the local spool is not copied twice; the manifest records the store; and
// damaged attachment content in the bucket fails the backup instead of being preserved.
func TestBackupCopiesS3Artifacts(t *testing.T) {
	f := newFixture(t)
	srv := s3test.New(t, "AKIDTEST")
	cl, err := s3client.New(s3client.Config{Endpoint: srv.URL, Bucket: "sm-art", PathStyle: true, AccessKeyID: "AKIDTEST", SecretAccessKey: "secret",
		CABundlePEM: srv.CAPEM(), AllowPrivateNetwork: true})
	must(t, err)
	remote := artifacts.NewS3(cl, "sm-art", "a/")
	local := artifacts.NewLocal(map[string]string{artifacts.NSAttachments: f.st.Path("artifacts"), artifacts.NSRecordings: f.st.Path("recordings"),
		artifacts.NSTranscripts: f.st.Path("transcripts")})
	// Move the fixture's artifacts into the bucket, as central with artifacts.backend: s3 would have them, but keep
	// the recording segment in the spool too (an archive interrupted after its upload).
	rep, err := artifacts.Migrate(f.ctx, local, remote, artifacts.MigrateOptions{TmpDir: t.TempDir()})
	must(t, err)
	if rep.Copied != 3 {
		t.Fatalf("fixture migration %+v", rep)
	}
	for _, k := range []string{artifacts.TranscriptKey(f.runID)} {
		must(t, local.Delete(f.ctx, k))
	}
	for rel := range f.files {
		if strings.HasPrefix(rel, "artifacts/sha256/") {
			must(t, os.Remove(filepath.Join(f.st.Dir(), filepath.FromSlash(rel))))
		}
	}
	// One more object only in the bucket.
	extra := []byte("archived second segment")
	key := artifacts.RecordingKey(f.runID, "seg-000002.smrec.zst")
	must(t, remote.Put(f.ctx, key, bytes.NewReader(extra), int64(len(extra)), sumHex(extra)))
	f.files["recordings/"+f.runID+"/seg-000002.smrec.zst"] = extra

	src := f.sources()
	src.Store = remote
	out := filepath.Join(t.TempDir(), "b")
	res, err := backup.Create(f.ctx, f.source(), f.kek, out, backup.Options{Sources: src})
	must(t, err)
	m := res.Manifest
	if m.ArtifactStore == nil || m.ArtifactStore.Kind != "s3" || !m.ArtifactStore.Included || m.ArtifactStore.Location != "s3://sm-art/a/" ||
		m.ArtifactStore.Objects != 3 { // the attachment, the transcript and the second segment; the first segment came from the spool
		t.Fatalf("artifact store in the manifest: %+v", m.ArtifactStore)
	}
	paths := map[string]int{}
	for _, file := range m.Files {
		paths[file.Path]++
	}
	for rel := range f.files {
		if paths[rel] != 1 {
			t.Errorf("%s is in the backup %d times", rel, paths[rel])
		}
	}
	rep2, err := backup.Verify(f.ctx, out, backup.VerifyOptions{KEK: f.kek, TempDir: t.TempDir()})
	must(t, err)
	if !rep2.OK {
		t.Fatalf("verify %+v", rep2)
	}
	must(t, f.st.Close())
	target := filepath.Join(t.TempDir(), "restored")
	_, err = backup.Restore(f.ctx, out, backup.RestoreOptions{DataDir: target, KEK: f.kek, Git: f.git})
	must(t, err)
	for rel, want := range f.files {
		got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s not restored: %v", rel, err)
		}
	}

	// The local backend records itself too.
	if lm := newFixture(t).create(filepath.Join(t.TempDir(), "l")).Manifest; lm.ArtifactStore == nil || lm.ArtifactStore.Kind != "local" {
		t.Fatalf("local manifest store %+v", lm.ArtifactStore)
	}
}

func TestBackupRefusesDamagedS3Attachment(t *testing.T) {
	f := newFixture(t)
	srv := s3test.New(t, "AKIDTEST")
	cl, err := s3client.New(s3client.Config{Endpoint: srv.URL, Bucket: "sm-art", PathStyle: true, AccessKeyID: "AKIDTEST", SecretAccessKey: "secret",
		CABundlePEM: srv.CAPEM(), AllowPrivateNetwork: true})
	must(t, err)
	remote := artifacts.NewS3(cl, "sm-art", "")
	srv.SetObject("sm-art", artifacts.AttachmentKey(sumHex([]byte("claimed"))), []byte("other bytes"))
	src := f.sources()
	src.Store = remote
	_, err = backup.Create(f.ctx, f.source(), f.kek, filepath.Join(t.TempDir(), "b"), backup.Options{Sources: src})
	if !errors.Is(err, artifacts.ErrChecksum) {
		t.Fatalf("damaged attachment backed up: %v", err)
	}
	srv.Fail = func(*http.Request) int { return http.StatusServiceUnavailable }
	if _, err := backup.Create(f.ctx, f.source(), f.kek, filepath.Join(t.TempDir(), "c"), backup.Options{Sources: src}); err == nil {
		t.Fatal("backup without the artifact store succeeded")
	}
}

func sumHex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
