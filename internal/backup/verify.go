package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Report is the outcome of Verify.
type Report struct {
	Manifest *Manifest `json:"manifest,omitempty"`
	// Keyed is true when the KEK was available: the sealed objects were opened and their plaintext checked, and the
	// database copy was checked. Without the KEK only the sealed objects' sizes and SHA-256 are checked.
	Keyed bool  `json:"keyed"`
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
	// AuditEvents is the number of audit-chain entries whose hash links were checked in the database copy.
	AuditEvents int64 `json:"audit_events"`
	// BinarySchema is the newest schema version this binary supports.
	BinarySchema int64 `json:"binary_schema"`
	// Warnings are conditions recorded when the backup was created.
	Warnings []string `json:"warnings,omitempty"`
	// Problems make the backup unusable as is.
	Problems []string `json:"problems,omitempty"`
	OK       bool     `json:"ok"`

	schemaTooNew bool
}

func (r *Report) problem(format string, args ...any) {
	r.Problems = append(r.Problems, fmt.Sprintf(format, args...))
}

// VerifyOptions configure Verify.
type VerifyOptions struct {
	// KEK opens the sealed objects. Nil verifies only what can be checked without it.
	KEK keyring.KeyWrapper
	// TempDir receives the decrypted database copy while it is checked (default os.TempDir()). It is removed afterwards.
	TempDir string
}

// Verify checks a backup directory: manifest.json against manifest.sha256, that the manifest describes a SpaceMan
// backup without the KEK, the sealed size and SHA-256 of every listed object, and that nothing unlisted is present.
// With the KEK it also checks that the sealed manifest equals manifest.json, opens every object and checks its plaintext
// size and SHA-256, and checks the database copy (PRAGMA integrity_check, schema version, keyring KEK, audit-chain hash
// links and head). The returned error wraps ErrInvalid when the backup must not be restored; the report is returned
// either way when the manifest could be read.
func Verify(ctx context.Context, dir string, o VerifyOptions) (*Report, error) {
	rep := &Report{Keyed: o.KEK != nil}
	m, raw, err := readManifest(dir)
	if err != nil {
		rep.problem("%v", err)
		return rep, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	rep.Manifest = m
	rep.Warnings = append(rep.Warnings, m.Warnings...)
	if o.KEK != nil {
		checkSealedManifest(dir, m, raw, o.KEK, rep)
	}
	listed := checkObjects(dir, m, rep)
	checkUnlisted(dir, listed, rep)
	if o.KEK != nil && len(rep.Problems) == 0 {
		checkPlaintext(ctx, dir, m, o, rep)
	}
	rep.OK = len(rep.Problems) == 0
	switch {
	case rep.schemaTooNew:
		return rep, fmt.Errorf("%w: %w: %s", ErrInvalid, store.ErrSchemaTooNew, strings.Join(rep.Problems, "; "))
	case !rep.OK:
		return rep, fmt.Errorf("%w: %s", ErrInvalid, strings.Join(rep.Problems, "; "))
	}
	return rep, nil
}

// ReadManifest reads manifest.json of a backup directory and checks it against manifest.sha256 (not authenticated:
// Verify with the KEK compares it with the sealed copy).
func ReadManifest(dir string) (*Manifest, error) {
	m, _, err := readManifest(dir)
	return m, err
}

func readManifest(dir string) (*Manifest, []byte, error) {
	b, err := readSmall(filepath.Join(dir, ManifestFile), 64<<20)
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest: %w", err)
	}
	sumLine, err := readSmall(filepath.Join(dir, ManifestSumFile), 1024)
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest checksum (an incomplete backup has none): %w", err)
	}
	fields := strings.Fields(string(sumLine))
	sum := sha256.Sum256(b)
	if len(fields) != 2 || fields[1] != ManifestFile || fields[0] != hex.EncodeToString(sum[:]) {
		return nil, nil, errors.New("manifest.json does not match manifest.sha256")
	}
	m, err := parseManifest(b)
	if err != nil {
		return nil, nil, err
	}
	return m, b, nil
}

func parseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	switch {
	case m.Format != Format:
		return nil, fmt.Errorf("not a SpaceMan backup (format %q)", m.Format)
	case m.Version != FormatVersion:
		return nil, fmt.Errorf("unsupported backup format version %d (this binary reads %d)", m.Version, FormatVersion)
	case m.KEKIncluded:
		return nil, errors.New("manifest claims to include the KEK; SpaceMan backups never contain it")
	case !ValidID(m.ID):
		return nil, fmt.Errorf("manifest has an invalid backup ID %q", m.ID)
	}
	return &m, nil
}

func checkSealedManifest(dir string, m *Manifest, raw []byte, kek keyring.KeyWrapper, rep *Report) {
	if m.KEKID != kek.ID() {
		rep.problem("%v: the backup is sealed under KEK %s, the supplied KEK is %s", ErrKEKMismatch, m.KEKID, kek.ID())
		return
	}
	f, err := openRegular(filepath.Join(dir, SealedManifestFile))
	if err != nil {
		rep.problem("sealed manifest: %v", err)
		return
	}
	defer f.Close()
	r, _, err := OpenSealed(f, kek, SealedKindManifest, m.ID)
	if err != nil {
		rep.problem("sealed manifest: %v", err)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r, 64<<20))
	if err != nil {
		rep.problem("sealed manifest: %v", err)
		return
	}
	if !bytes.Equal(b, raw) {
		rep.problem("manifest.json differs from the sealed manifest (it was modified)")
	}
}

// validPath reports whether a manifest path is a clean relative path in one of the backup's areas.
func validPath(p, kind string) bool {
	if !validRel(p) {
		return false
	}
	switch kind {
	case KindDatabase:
		return p == DBFile
	case KindConfig:
		return p == ConfigFile
	case KindRecording:
		return strings.HasPrefix(p, dirRecordings+"/")
	case KindAttachment:
		return strings.HasPrefix(p, dirArtifacts+"/")
	case KindTranscript:
		return strings.HasPrefix(p, dirTranscripts+"/")
	case KindMemory:
		return strings.HasPrefix(p, dirMemory+"/") && strings.HasSuffix(p, ".bundle") && reRepo.MatchString(strings.TrimPrefix(strings.TrimSuffix(p, ".bundle"), dirMemory+"/")+".git")
	}
	return false
}

// checkObjects hashes every listed sealed object and returns the set of listed object paths.
func checkObjects(dir string, m *Manifest, rep *Report) map[string]*File {
	listed := map[string]*File{}
	dbs := 0
	for i := range m.Files {
		f := &m.Files[i]
		if !validPath(f.Path, f.Kind) {
			rep.problem("manifest lists an invalid path %q (%s)", f.Path, f.Kind)
			continue
		}
		if _, dup := listed[f.Object()]; dup {
			rep.problem("manifest lists %s twice", f.Path)
			continue
		}
		listed[f.Object()] = f
		if f.Kind == KindDatabase {
			dbs++
		}
		got, err := hashFile(filepath.Join(dir, filepath.FromSlash(f.Object())))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			rep.problem("%s is missing", f.Object())
			continue
		case err != nil:
			rep.problem("%s: %v", f.Object(), err)
			continue
		}
		if got.size != f.SealedSize || got.sum != f.SealedSHA256 {
			rep.problem("%s does not match the manifest (size or SHA-256 differs)", f.Object())
			continue
		}
		rep.Files++
		rep.Bytes += got.size
	}
	if dbs != 1 {
		rep.problem("manifest must list exactly one database file, found %d", dbs)
	}
	return listed
}

// checkUnlisted reports files in the backup directory that the manifest does not list, and anything that is not a
// regular file or directory.
func checkUnlisted(dir string, listed map[string]*File, rep *Report) {
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			rep.problem("%s is not a regular file", rel)
		case rel == ManifestFile || rel == ManifestSumFile || rel == SealedManifestFile:
		case listed[rel] == nil:
			rep.problem("%s is not listed in the manifest", rel)
		}
		return nil
	})
	if err != nil {
		rep.problem("scan backup directory: %v", err)
	}
}

// checkPlaintext opens every sealed object and checks the database copy.
func checkPlaintext(ctx context.Context, dir string, m *Manifest, o VerifyOptions, rep *Report) {
	for _, f := range m.Files {
		if err := ctx.Err(); err != nil {
			rep.problem("%v", err)
			return
		}
		src := filepath.Join(dir, filepath.FromSlash(f.Object()))
		want := sizeSum{f.Size, f.SHA256}
		if f.Kind != KindDatabase {
			if err := readSealed(src, o.KEK, SealedKindFile(f.Path), m.ID, want); err != nil {
				rep.problem("%s: %v", f.Path, err)
			}
			continue
		}
		tmp, err := os.MkdirTemp(o.TempDir, ".verify-")
		if err != nil {
			rep.problem("temporary directory: %v", err)
			return
		}
		plain := filepath.Join(tmp, DBFile)
		if err := openFile(src, plain, o.KEK, SealedKindFile(f.Path), m.ID, want); err != nil {
			rep.problem("%s: %v", f.Path, err)
		} else {
			checkDatabase(ctx, plain, m, rep)
		}
		_ = os.RemoveAll(tmp)
	}
}

// checkDatabase checks the decrypted database copy against the manifest.
func checkDatabase(ctx context.Context, dbPath string, m *Manifest, rep *Report) {
	d, err := openImmutable(ctx, dbPath)
	if err != nil {
		rep.problem("database copy: %v", err)
		return
	}
	defer d.Close()
	if err := integrityCheck(ctx, d); err != nil {
		rep.problem("database copy: %v", err)
		return
	}
	schema, latest, err := store.SchemaVersions(ctx, d, store.EmbeddedMigrations())
	rep.BinarySchema = latest
	switch {
	case err != nil:
		rep.problem("database copy schema version: %v", err)
		return
	case schema != m.SchemaVersion:
		rep.problem("database copy is at schema version %d but the manifest says %d", schema, m.SchemaVersion)
	case schema > latest:
		rep.schemaTooNew = true
		rep.problem("backup schema version %d is newer than this binary supports (%d); restore with a newer spaceman", schema, latest)
	}
	ids, err := kekIDs(ctx, d)
	switch {
	case err != nil:
		rep.problem("%v", err)
	case len(ids) > 1 || (len(ids) == 1 && ids[0] != m.KEKID):
		rep.problem("keyring in the database copy is wrapped under KEK %q but the manifest says %q", strings.Join(ids, ","), m.KEKID)
	}
	res, err := audit.Verify(ctx, db.New(d))
	rep.AuditEvents = res.Events
	switch {
	case err != nil:
		rep.problem("audit chain in the database copy: %v", err)
	case !res.OK:
		rep.problem("audit chain in the database copy is broken at %d: %s", res.BrokenAt, res.Problem)
	case res.HeadSeq != m.AuditHead.Seq || (m.AuditHead.Seq > 0 && hex.EncodeToString(res.HeadHash) != m.AuditHead.Hash):
		// Retention may have pruned the oldest events: the head (sequence and hash) is what the manifest recorded.
		rep.problem("audit chain head (seq %d, %d entries) differs from the manifest (seq %d)", res.HeadSeq, res.Events, m.AuditHead.Seq)
	}
}

func integrityCheck(ctx context.Context, d *sql.DB) error {
	rows, err := d.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	var msgs []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		msgs = append(msgs, s)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(msgs) != 1 || msgs[0] != "ok" {
		if len(msgs) > 5 {
			msgs = append(msgs[:5], "…")
		}
		return fmt.Errorf("%w: integrity_check reported: %s", store.ErrCorrupt, strings.Join(msgs, "; "))
	}
	return nil
}

// objectPaths lists every object a complete backup directory holds (for upload), manifest files last and
// manifest.sha256 at the very end, so its presence marks a complete remote backup.
func objectPaths(m *Manifest) []string {
	out := make([]string, 0, len(m.Files)+3)
	for _, f := range m.Files {
		out = append(out, f.Object())
	}
	return append(out, ManifestFile, SealedManifestFile, ManifestSumFile)
}
