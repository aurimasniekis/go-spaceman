package backup_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// SM-STORE-003/004/005: create → verify (with and without the KEK) → restore into another directory → every payload
// file is back, role memory is a repository again, and nothing in the backup is readable without the KEK.
func TestRoundTrip(t *testing.T) {
	f := newFixture(t)
	out := filepath.Join(t.TempDir(), "target", "b1")
	res := f.create(out)
	m := res.Manifest
	if m.KEKIncluded || m.KEKID != f.kek.ID() || m.SchemaVersion == 0 || m.SourceDataDir != f.st.Dir() || m.AuditHead.Seq == 0 ||
		m.Counts.Spaces != 1 || m.Counts.Runs != 1 || m.Counts.PendingApprovals != 1 || len(m.Included) == 0 || len(m.Excluded) == 0 {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	kinds := map[string]int{}
	for _, file := range m.Files {
		kinds[file.Kind]++
	}
	wantMemory := 0
	if f.git != nil {
		wantMemory = 1 // the empty repository has nothing to bundle
	}
	if kinds[backup.KindDatabase] != 1 || kinds[backup.KindRecording] != 1 || kinds[backup.KindAttachment] != 1 || kinds[backup.KindTranscript] != 2 ||
		kinds[backup.KindMemory] != wantMemory || kinds[backup.KindConfig] != 1 {
		t.Fatalf("files by kind: %v (%+v)", kinds, m.Files)
	}
	// Only sealed objects and the manifest files; no plaintext anywhere (the KEK, file contents, the database).
	must(t, filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		must(t, err)
		if d.IsDir() {
			if fi, _ := d.Info(); fi.Mode().Perm()&0o077 != 0 {
				t.Errorf("%s is accessible by group or others: %v", p, fi.Mode())
			}
			return nil
		}
		rel, _ := filepath.Rel(out, p)
		b, err := os.ReadFile(p)
		must(t, err)
		switch {
		case rel == backup.ManifestFile || rel == backup.ManifestSumFile:
		case strings.HasSuffix(rel, backup.SealedSuffix):
			if !bytes.HasPrefix(b, []byte(backup.SealedMagic)) {
				t.Errorf("%s is not sealed", rel)
			}
		default:
			t.Errorf("unexpected file in backup: %s", rel)
		}
		if bytes.Contains(b, []byte(f.secret)) || bytes.Contains(b, []byte("SQLite format 3")) {
			t.Errorf("%s contains plaintext", rel)
		}
		return nil
	}))

	rep, err := backup.Verify(f.ctx, out, backup.VerifyOptions{KEK: f.kek, TempDir: t.TempDir()})
	must(t, err)
	if !rep.OK || !rep.Keyed || rep.Files != len(m.Files) || rep.AuditEvents != m.AuditHead.Seq || rep.BinarySchema < m.SchemaVersion {
		t.Fatalf("verify: %+v", rep)
	}
	rep, err = backup.Verify(f.ctx, out, backup.VerifyOptions{})
	must(t, err)
	if !rep.OK || rep.Keyed {
		t.Fatalf("keyless verify: %+v", rep)
	}
	must(t, f.st.Close())

	target := filepath.Join(t.TempDir(), "restored")
	rr, err := backup.Restore(f.ctx, out, backup.RestoreOptions{DataDir: target, KEK: f.kek, Git: f.git})
	must(t, err)
	if rr.Files != len(m.Files) || len(rr.MovedAside) != 0 || rr.ConfigCopy == "" {
		t.Fatalf("restore: %+v", rr)
	}
	for rel, want := range f.files {
		got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s not restored: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "artifacts", "tmp", "upload-1")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("unfinished upload restored: %v", err)
	}
	if f.git != nil {
		repo := filepath.Join(target, "memory", f.spaceID, "implementor.git")
		if gitHead(t, repo, "refs/heads/main") == "" || gitHead(t, repo, "refs/conflicts/node-1") == "" {
			t.Errorf("memory repository not restored with all refs")
		}
	}
	if b, err := os.ReadFile(rr.ConfigCopy); err != nil || string(b) != "mode: development\n" {
		t.Errorf("config copy: %q %v", b, err)
	}
	// The original backup still verifies after the restore read it.
	if _, err := backup.Verify(f.ctx, out, backup.VerifyOptions{}); err != nil {
		t.Fatal(err)
	}
}

// SM-STORE-005: verification detects tampering, missing and extra files, a modified manifest and the wrong KEK.
func TestVerifyDetectsTampering(t *testing.T) {
	f := newFixture(t)
	base := filepath.Join(t.TempDir(), "good")
	m := f.create(base).Manifest
	clone := func(t *testing.T) string {
		t.Helper()
		dst := filepath.Join(t.TempDir(), "copy")
		must(t, filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			must(t, err)
			rel, _ := filepath.Rel(base, p)
			if d.IsDir() {
				return os.MkdirAll(filepath.Join(dst, rel), 0o700)
			}
			b, err := os.ReadFile(p)
			must(t, err)
			return os.WriteFile(filepath.Join(dst, rel), b, 0o600)
		}))
		return dst
	}
	var rec backup.File
	for _, file := range m.Files {
		if file.Kind == backup.KindRecording {
			rec = file
		}
	}
	rewriteManifest := func(t *testing.T, dir string, edit func(*backup.Manifest)) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, backup.ManifestFile))
		must(t, err)
		var mm backup.Manifest
		must(t, json.Unmarshal(b, &mm))
		edit(&mm)
		b, _ = json.MarshalIndent(&mm, "", "  ")
		b = append(b, '\n')
		must(t, os.WriteFile(filepath.Join(dir, backup.ManifestFile), b, 0o600))
		sum := sha256.Sum256(b)
		must(t, os.WriteFile(filepath.Join(dir, backup.ManifestSumFile), []byte(hex.EncodeToString(sum[:])+"  "+backup.ManifestFile+"\n"), 0o600))
	}
	cases := []struct {
		name    string
		mod     func(t *testing.T, dir string)
		keyless bool // the keyless check already fails
		kek     keyring.KeyWrapper
	}{
		{"flipped byte in a sealed object", func(t *testing.T, dir string) {
			p := filepath.Join(dir, filepath.FromSlash(rec.Object()))
			b, _ := os.ReadFile(p)
			b[len(b)-3] ^= 1
			must(t, os.WriteFile(p, b, 0o600))
		}, true, nil},
		{"missing object", func(t *testing.T, dir string) {
			must(t, os.Remove(filepath.Join(dir, filepath.FromSlash(rec.Object()))))
		}, true, nil},
		{"unlisted file", func(t *testing.T, dir string) { must(t, os.WriteFile(filepath.Join(dir, "extra"), []byte("x"), 0o600)) }, true, nil},
		{"manifest without its checksum", func(t *testing.T, dir string) { must(t, os.Remove(filepath.Join(dir, backup.ManifestSumFile))) }, true, nil},
		{"manifest edited and re-summed", func(t *testing.T, dir string) {
			rewriteManifest(t, dir, func(mm *backup.Manifest) { mm.Counts.Spaces = 99 })
		}, false, nil},
		{"object swapped for a resealed one with matching sums", func(t *testing.T, dir string) {
			// An attacker without the KEK cannot produce a valid object; with the sums updated, only opening it catches it.
			p := filepath.Join(dir, filepath.FromSlash(rec.Object()))
			var bb bytes.Buffer
			must(t, backup.Seal(&bb, strings.NewReader("forged"), newKEK(t), backup.SealedKindFile(rec.Path), m.ID))
			must(t, os.WriteFile(p, bb.Bytes(), 0o600))
			sum := sha256.Sum256(bb.Bytes())
			rewriteManifest(t, dir, func(mm *backup.Manifest) {
				for i := range mm.Files {
					if mm.Files[i].Path == rec.Path {
						mm.Files[i].SealedSize, mm.Files[i].SealedSHA256 = int64(bb.Len()), hex.EncodeToString(sum[:])
					}
				}
			})
		}, false, nil},
		{"manifest claims the KEK", func(t *testing.T, dir string) {
			rewriteManifest(t, dir, func(mm *backup.Manifest) { mm.KEKIncluded = true })
		}, true, nil},
		{"path outside the backup", func(t *testing.T, dir string) {
			rewriteManifest(t, dir, func(mm *backup.Manifest) { mm.Files[1].Path = "../escape" })
		}, true, nil},
		{"wrong KEK", func(*testing.T, string) {}, false, newKEK(t)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := clone(t)
			c.mod(t, dir)
			_, err := backup.Verify(f.ctx, dir, backup.VerifyOptions{})
			if c.keyless != (err != nil) {
				t.Fatalf("keyless verify: %v", err)
			}
			kek := keyring.KeyWrapper(f.kek)
			if c.kek != nil {
				kek = c.kek
			}
			rep, err := backup.Verify(f.ctx, dir, backup.VerifyOptions{KEK: kek, TempDir: t.TempDir()})
			if !errors.Is(err, backup.ErrInvalid) || rep.OK || len(rep.Problems) == 0 {
				t.Fatalf("keyed verify accepted a tampered backup: %v %+v", err, rep)
			}
			// A tampered backup is never restored.
			if _, err := backup.Restore(f.ctx, dir, backup.RestoreOptions{DataDir: filepath.Join(t.TempDir(), "r"), KEK: kek, Git: f.git}); err == nil {
				t.Fatal("restored a tampered backup")
			}
		})
	}
}

// Restore refuses a non-empty data directory unless forced; forced, it moves existing data aside and deletes nothing.
func TestRestoreTarget(t *testing.T) {
	f := newFixture(t)
	out := filepath.Join(t.TempDir(), "b")
	f.create(out)
	dataDir := f.st.Dir()
	must(t, f.st.Close())
	_, err := backup.Restore(f.ctx, out, backup.RestoreOptions{DataDir: dataDir, KEK: f.kek, Git: f.git})
	if !errors.Is(err, backup.ErrNotEmpty) {
		t.Fatalf("non-empty target: %v", err)
	}
	if _, err := backup.Restore(f.ctx, out, backup.RestoreOptions{DataDir: filepath.Join(t.TempDir(), "x"), Git: f.git}); !errors.Is(err, backup.ErrKEKMismatch) {
		t.Fatalf("restore without the KEK: %v", err)
	}
	if _, err := backup.Restore(f.ctx, out, backup.RestoreOptions{DataDir: filepath.Join(t.TempDir(), "y"), KEK: newKEK(t), Git: f.git}); !errors.Is(err, backup.ErrInvalid) {
		t.Fatalf("restore with another KEK: %v", err)
	}
	artifacts := filepath.Join(t.TempDir(), "elsewhere", "artifacts")
	must(t, os.MkdirAll(artifacts, 0o700))
	must(t, os.WriteFile(filepath.Join(artifacts, "old"), []byte("old"), 0o600))
	rr, err := backup.Restore(f.ctx, out, backup.RestoreOptions{DataDir: dataDir, ArtifactsDir: artifacts, KEK: f.kek, Git: f.git, Force: true})
	must(t, err)
	if len(rr.MovedAside) != 2 {
		t.Fatalf("moved aside: %v", rr.MovedAside)
	}
	if _, err := os.Stat(filepath.Join(rr.MovedAside[0], store.DBFile)); err != nil {
		t.Errorf("the previous database was not kept: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(rr.MovedAside[1], "old")); err != nil || string(b) != "old" {
		t.Errorf("the previous artifacts were not kept: %v", err)
	}
	n := 0
	must(t, filepath.WalkDir(filepath.Join(artifacts, "sha256"), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return err
	}))
	if n != 1 {
		t.Errorf("attachments restored into the artifacts folder: %d", n)
	}
}

func TestCreateRefusals(t *testing.T) {
	f := newFixture(t)
	out := filepath.Join(t.TempDir(), "b")
	must(t, os.MkdirAll(out, 0o700))
	if _, err := backup.Create(f.ctx, f.source(), f.kek, out, backup.Options{}); !errors.Is(err, backup.ErrExists) {
		t.Fatalf("existing destination: %v", err)
	}
	if _, err := backup.Create(f.ctx, f.source(), newKEK(t), filepath.Join(t.TempDir(), "c"), backup.Options{}); !errors.Is(err, backup.ErrKEKMismatch) {
		t.Fatalf("another KEK: %v", err)
	}
	if _, err := backup.Create(f.ctx, f.source(), f.kek, filepath.Join(t.TempDir(), "d"), backup.Options{ID: "nope"}); err == nil {
		t.Fatal("invalid ID accepted")
	}
	if _, err := backup.OpenReadOnly(f.ctx, t.TempDir()); err == nil {
		t.Fatal("opened a directory without a database")
	}
}
