package backup_test

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/s3client"
	"github.com/aurimasniekis/go-spaceman/internal/s3client/s3test"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// reseal rewrites a backup as someone holding the KEK would: the database object is replaced by the output of mod and
// the manifest (plain, sealed and summed) by edit. It exercises the database checks that only the KEK can reach.
func reseal(t *testing.T, f *fixture, dir string, mod func(dbPath string), edit func(*backup.Manifest)) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, backup.ManifestFile))
	must(t, err)
	var m backup.Manifest
	must(t, json.Unmarshal(b, &m))
	if mod != nil {
		plain := filepath.Join(t.TempDir(), "db")
		sealed, err := os.Open(filepath.Join(dir, backup.DBFile+backup.SealedSuffix))
		must(t, err)
		r, _, err := backup.OpenSealed(sealed, f.kek, backup.SealedKindFile(backup.DBFile), m.ID)
		must(t, err)
		var pb bytes.Buffer
		_, err = pb.ReadFrom(r)
		must(t, err)
		_ = sealed.Close()
		must(t, os.WriteFile(plain, pb.Bytes(), 0o600))
		mod(plain)
		data, err := os.ReadFile(plain)
		must(t, err)
		var out bytes.Buffer
		must(t, backup.Seal(&out, bytes.NewReader(data), f.kek, backup.SealedKindFile(backup.DBFile), m.ID))
		must(t, os.WriteFile(filepath.Join(dir, backup.DBFile+backup.SealedSuffix), out.Bytes(), 0o600))
		ps, ss := sha256.Sum256(data), sha256.Sum256(out.Bytes())
		for i := range m.Files {
			if m.Files[i].Kind == backup.KindDatabase {
				m.Files[i].Size, m.Files[i].SHA256 = int64(len(data)), hex.EncodeToString(ps[:])
				m.Files[i].SealedSize, m.Files[i].SealedSHA256 = int64(out.Len()), hex.EncodeToString(ss[:])
			}
		}
	}
	if edit != nil {
		edit(&m)
	}
	b, _ = json.MarshalIndent(&m, "", "  ")
	b = append(b, '\n')
	must(t, os.WriteFile(filepath.Join(dir, backup.ManifestFile), b, 0o600))
	var sm bytes.Buffer
	must(t, backup.Seal(&sm, bytes.NewReader(b), f.kek, backup.SealedKindManifest, m.ID))
	must(t, os.WriteFile(filepath.Join(dir, backup.SealedManifestFile), sm.Bytes(), 0o600))
	sum := sha256.Sum256(b)
	must(t, os.WriteFile(filepath.Join(dir, backup.ManifestSumFile), []byte(hex.EncodeToString(sum[:])+"  "+backup.ManifestFile+"\n"), 0o600))
}

func sqlExec(t *testing.T, dbPath, q string) {
	t.Helper()
	d, err := sql.Open("sqlite", "file:"+dbPath)
	must(t, err)
	defer d.Close()
	if _, err := d.Exec(q); err != nil {
		t.Fatal(err)
	}
}

// The database copy is checked: integrity, schema version, keyring KEK and the audit chain.
func TestVerifyDatabaseCopy(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name string
		mod  func(string)
		edit func(*backup.Manifest)
		want string
	}{
		{"not a database", func(p string) { must(t, os.WriteFile(p, []byte("garbage that is not sqlite"), 0o600)) }, nil, "database copy"},
		{"schema differs", nil, func(m *backup.Manifest) { m.SchemaVersion-- }, "schema version"},
		{"audit chain edited", func(p string) { sqlExec(t, p, `UPDATE audit_events SET action = 'forged' WHERE seq = 1`) }, nil, "audit chain"},
		{"audit head differs", nil, func(m *backup.Manifest) { m.AuditHead.Seq++ }, "audit chain head"},
		{"keyring under another KEK", func(p string) { sqlExec(t, p, `UPDATE keyring_keys SET wrap_kek_id = 'other'`) }, nil, "keyring"},
		{"schema newer than the binary", func(p string) {
			sqlExec(t, p, `INSERT INTO goose_db_version (version_id, is_applied) VALUES (9999, 1)`)
		},
			func(m *backup.Manifest) { m.SchemaVersion = 9999 }, "newer than this binary"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "b")
			f.create(dir)
			reseal(t, f, dir, c.mod, c.edit)
			rep, err := backup.Verify(f.ctx, dir, backup.VerifyOptions{KEK: f.kek, TempDir: t.TempDir()})
			if err == nil || !strings.Contains(strings.Join(rep.Problems, "; "), c.want) {
				t.Fatalf("want a %q problem, got %v %+v", c.want, err, rep.Problems)
			}
		})
	}
}

func TestTargetFromConfig(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	tg, err := backup.TargetFromConfig(cfg)
	must(t, err)
	if tg.Kind() != "local" || tg.Location() != filepath.Join(cfg.DataDir, "backups") || tg.StagingDir() != tg.Location() {
		t.Fatalf("default target: %v %s", tg.Kind(), tg.Location())
	}
	keys := t.TempDir()
	ak, sk := filepath.Join(keys, "ak"), filepath.Join(keys, "sk")
	must(t, os.WriteFile(ak, []byte("AKID\n"), 0o600))
	must(t, os.WriteFile(sk, []byte("secret\n"), 0o600))
	cfg.Backups.Target = "s3"
	cfg.Backups.S3 = config.S3Config{Endpoint: "https://s3.example.com", Bucket: "sm-backups", Prefix: "prod/", AccessKeyFile: ak, SecretKeyFile: sk}
	tg, err = backup.TargetFromConfig(cfg)
	must(t, err)
	if tg.Kind() != "s3" || tg.Location() != "s3://sm-backups/prod/" || !strings.HasPrefix(tg.StagingDir(), cfg.DataDir) {
		t.Fatalf("s3 target: %s %s", tg.Location(), tg.StagingDir())
	}
	for name, mod := range map[string]func(*config.S3Config){
		"bad prefix":  func(s *config.S3Config) { s.Prefix = "/abs" },
		"missing key": func(s *config.S3Config) { s.AccessKeyFile = filepath.Join(keys, "none") },
		"empty secret": func(s *config.S3Config) {
			must(t, os.WriteFile(filepath.Join(keys, "empty"), nil, 0o600))
			s.SecretKeyFile = filepath.Join(keys, "empty")
		},
		"http endpoint":  func(s *config.S3Config) { s.Endpoint = "http://s3.example.com" },
		"missing CA":     func(s *config.S3Config) { s.CAFile = filepath.Join(keys, "none.pem") },
		"bucket invalid": func(s *config.S3Config) { s.Bucket = "X" },
	} {
		c := *cfg
		c.Backups.S3 = cfg.Backups.S3
		mod(&c.Backups.S3)
		if _, err := backup.TargetFromConfig(&c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	cfg.Backups.Target = "tape"
	if _, err := backup.TargetFromConfig(cfg); err == nil {
		t.Error("unknown target accepted")
	}
}

func TestValidatePrefix(t *testing.T) {
	for _, ok := range []string{"", "spaceman/", "a/b-c_d.e/"} {
		if err := backup.ValidatePrefix(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"noslash", "/lead/", "a//b/", "a/../b/", "sp ace/", strings.Repeat("a", 300) + "/"} {
		if err := backup.ValidatePrefix(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestLocalTargetEdges(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "backups")
	tg := backup.LocalTarget{Dir: dir}
	if es, err := tg.List(f.ctx); err != nil || len(es) != 0 {
		t.Fatalf("missing dir: %v %v", es, err)
	}
	if err := tg.Store(f.ctx, filepath.Join(t.TempDir(), "elsewhere")); err == nil {
		t.Error("stored a backup outside the target")
	}
	for _, id := range []string{"../x", "bkp_short"} {
		if _, _, err := tg.Fetch(f.ctx, id, ""); err == nil {
			t.Errorf("fetched %q", id)
		}
		if err := tg.Delete(f.ctx, id); err == nil {
			t.Errorf("deleted %q", id)
		}
	}
	// An incomplete leftover (no manifest.sha256) is listed as incomplete and removed by retention once old.
	id := "bkp_" + strings.Repeat("1", 26)
	must(t, os.MkdirAll(filepath.Join(dir, id), 0o700))
	old := f.st.Now().Add(-48 * 3600e9)
	must(t, os.Chtimes(filepath.Join(dir, id), old, old))
	es, err := tg.List(f.ctx)
	must(t, err)
	if len(es) != 1 || es[0].Complete {
		t.Fatalf("leftover: %+v", es)
	}
	svc := f.service(tg, 1)
	if n, err := svc.Prune(f.ctx); err != nil || n != 1 {
		t.Fatalf("prune leftover: %d %v", n, err)
	}
	if _, err := svc.AcknowledgeRestore(f.ctx, admin); err != nil {
		t.Fatal(err)
	}
	// Recover marks work a restart interrupted.
	must(t, f.st.W().TxNamed(f.ctx, "running", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, `INSERT INTO backups (id, target, trigger, state, started_at, verify_state) VALUES (?, 'local', 'manual', 'running', 1, 'running')`, id)
		return err
	}))
	must(t, svc.Recover(f.ctx))
	r, err := f.st.R().GetBackup(f.ctx, id)
	must(t, err)
	if r.State != backup.StateInterrupted || r.VerifyState != "failed" {
		t.Fatalf("recover: %+v", r)
	}
}

// Unsupported entries in the payload trees are skipped with a warning; a malformed manifest is refused.
func TestTreeAndManifestEdges(t *testing.T) {
	f := newFixture(t)
	must(t, os.Symlink("/etc/hosts", f.st.Path("recordings", "link")))
	f.write("transcripts/bad|name.jsonl", []byte("x"))
	delete(f.files, "transcripts/bad|name.jsonl")
	dir := filepath.Join(t.TempDir(), "b")
	m := f.create(dir).Manifest
	joined := strings.Join(m.Warnings, "; ")
	if !strings.Contains(joined, "recordings/link is not a regular file") || !strings.Contains(joined, "unsupported name") {
		t.Fatalf("warnings: %v", m.Warnings)
	}
	for name, edit := range map[string]func(*backup.Manifest){
		"format":  func(m *backup.Manifest) { m.Format = "hallpass-backup" },
		"version": func(m *backup.Manifest) { m.Version = 2 },
		"id":      func(m *backup.Manifest) { m.ID = "x" },
		"no db":   func(m *backup.Manifest) { m.Files = m.Files[1:] },
		"dup":     func(m *backup.Manifest) { m.Files = append(m.Files, m.Files[1]) },
	} {
		t.Run(name, func(t *testing.T) {
			d := filepath.Join(t.TempDir(), "b")
			f.create(d)
			reseal(t, f, d, nil, edit)
			if _, err := backup.Verify(f.ctx, d, backup.VerifyOptions{KEK: f.kek, TempDir: t.TempDir()}); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if _, err := backup.ReadManifest(t.TempDir()); err == nil {
		t.Fatal("read a manifest from an empty directory")
	}
}

func TestS3TargetEdges(t *testing.T) {
	f := newFixture(t)
	srv := s3test.New(t, "AKIDTEST")
	cl, err := s3client.New(s3client.Config{Endpoint: srv.URL, Bucket: "sm-backups", PathStyle: true, AccessKeyID: "AKIDTEST", SecretAccessKey: "secret",
		CABundlePEM: srv.CAPEM(), AllowPrivateNetwork: true})
	must(t, err)
	tg := backup.S3Target{Objects: cl, Bucket: "sm-backups", Staging: t.TempDir()}
	id := "bkp_" + strings.Repeat("2", 26)
	if _, _, err := tg.Fetch(f.ctx, id, t.TempDir()); err == nil {
		t.Fatal("fetched a backup that does not exist")
	}
	if _, _, err := tg.Fetch(f.ctx, "nope", t.TempDir()); err == nil {
		t.Fatal("fetched an invalid ID")
	}
	// Objects of an incomplete upload and a manifest that does not parse are listed as incomplete.
	srv.SetObject("sm-backups", id+"/spaceman.db.smbak", []byte("x"))
	other := "bkp_" + strings.Repeat("3", 26)
	srv.SetObject("sm-backups", other+"/manifest.json", []byte("{}"))
	srv.SetObject("sm-backups", other+"/manifest.sha256", []byte("x"))
	srv.SetObject("sm-backups", "unrelated/key", []byte("x"))
	es, err := tg.List(f.ctx)
	must(t, err)
	if len(es) != 2 || es[0].Complete || es[1].Complete {
		t.Fatalf("list: %+v", es)
	}
	must(t, tg.Delete(f.ctx, id))
	if _, ok := srv.Object("sm-backups", id+"/spaceman.db.smbak"); ok {
		t.Fatal("not deleted")
	}
	if err := tg.Store(f.ctx, t.TempDir()); err == nil {
		t.Fatal("stored a directory without a manifest")
	}
}

func TestMoreRefusals(t *testing.T) {
	f := newFixture(t)
	src := f.source()
	existing := filepath.Join(t.TempDir(), "exists")
	must(t, os.WriteFile(existing, nil, 0o600))
	if err := src.Snapshot(f.ctx, existing); err == nil {
		t.Error("snapshot over an existing file")
	}
	if err := src.Snapshot(f.ctx, filepath.Join(t.TempDir(), "it's")); err == nil {
		t.Error("snapshot path with a quote")
	}
	dir := filepath.Join(t.TempDir(), "b")
	f.create(dir)
	must(t, os.Remove(filepath.Join(dir, backup.SealedManifestFile)))
	if _, err := backup.Verify(f.ctx, dir, backup.VerifyOptions{KEK: f.kek}); err == nil {
		t.Error("accepted a backup without its sealed manifest")
	}
	if f.git != nil {
		d2 := filepath.Join(t.TempDir(), "b2")
		f.create(d2)
		if _, err := backup.Restore(f.ctx, d2, backup.RestoreOptions{DataDir: filepath.Join(t.TempDir(), "r"), KEK: f.kek}); err == nil ||
			!strings.Contains(err.Error(), "git") {
			t.Errorf("restored memory without git: %v", err)
		}
	}
}

// Retention by age deletes backups older than MaxAge but always keeps the newest.
func TestRetentionByAge(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "backups")
	now := f.st.Now()
	var ids []string
	for i, age := range []time.Duration{72 * time.Hour, 48 * time.Hour, time.Hour} {
		id := "bkp_" + strings.Repeat("0", 25) + string(rune('a'+i))
		_, err := backup.Create(f.ctx, f.source(), f.kek, filepath.Join(dir, id), backup.Options{ID: id, Clock: clock.NewFake(now.Add(-age))})
		must(t, err)
		ids = append(ids, id)
	}
	svc := backup.NewService(f.st, f.kek, f.aw, f.ch, backup.ServiceOptions{Target: backup.LocalTarget{Dir: dir}, KeepLast: 10, MaxAge: 24 * time.Hour})
	n, err := svc.Prune(f.ctx)
	if err != nil || n != 2 {
		t.Fatalf("prune: %d %v", n, err)
	}
	es, _ := backup.LocalTarget{Dir: dir}.List(f.ctx)
	if len(es) != 1 || es[0].ID != ids[2] {
		t.Fatalf("kept %+v", es)
	}
	// Even when every backup is too old, the newest stays.
	svc = backup.NewService(f.st, f.kek, f.aw, f.ch, backup.ServiceOptions{Target: backup.LocalTarget{Dir: dir}, KeepLast: 10, MaxAge: time.Minute})
	if n, err := svc.Prune(f.ctx); err != nil || n != 0 {
		t.Fatalf("prune newest: %d %v", n, err)
	}
}
