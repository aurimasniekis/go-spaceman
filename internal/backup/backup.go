// Package backup creates, verifies and restores SpaceMan backups (SM-STORE-003…006, ADR-0031).
//
// A backup is a directory named after its ID holding one sealed object (SMBAK1, sealed.go) per backed-up file, the
// plaintext manifest.json that lists every file with its plaintext and sealed size and SHA-256, a sealed copy of the
// manifest (manifest.json.smbak) that authenticates it, and manifest.sha256. The files are a consistent database
// snapshot (VACUUM INTO), one Git bundle per synced role-memory collection, the recordings, attachments and transcripts
// stored centrally, and the effective configuration. Nothing in a backup is readable without the key-encryption key
// (KEK), and the KEK is never part of a backup: restoring requires the operator to supply it separately.
//
// Derived from go-hallpass internal/backup at bdd8329 (Apache-2.0, same author): the snapshot source, file helpers,
// the sealed object format, the verification walk, the staged restore and the remote layout are adapted from it.
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
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/buildinfo"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/memory"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// Backup layout and manifest constants.
const (
	// Format identifies a SpaceMan backup manifest.
	Format = "spaceman-backup"
	// FormatVersion is the manifest version written and accepted by this package.
	FormatVersion = 1
	// ManifestFile lists every file of the backup (plaintext metadata, never content).
	ManifestFile = "manifest.json"
	// SealedSuffix is appended to the path of every sealed object.
	SealedSuffix = ".smbak"
	// SealedManifestFile authenticates ManifestFile: it holds the same bytes, sealed under the KEK.
	SealedManifestFile = ManifestFile + SealedSuffix
	// ManifestSumFile holds the SHA-256 of ManifestFile in sha256sum format. It is written last.
	ManifestSumFile = "manifest.sha256"
	// DBFile is the database snapshot's path inside a backup.
	DBFile = store.DBFile
	// ConfigFile is the effective configuration at backup time (file paths and settings, never secret values).
	ConfigFile = "config.yaml"
	// KEKNotice is recorded in every manifest.
	KEKNotice = "The key-encryption key (KEK) is NOT included in this backup. Every file in it is sealed under the KEK identified " +
		"by kek_id, and the restored database's credentials are wrapped under it too. Back the KEK up separately; without it " +
		"this backup is unrecoverable."
)

// File kinds in a manifest.
const (
	KindDatabase   = "database"
	KindRecording  = "recording"
	KindAttachment = "attachment"
	KindTranscript = "transcript"
	KindMemory     = "memory"
	KindConfig     = "config"
)

// Top-level directories of the payload trees inside a backup.
const (
	dirRecordings  = "recordings"
	dirArtifacts   = "artifacts"
	dirTranscripts = "transcripts"
	dirMemory      = "memory"
)

// Included and Excluded describe what every backup holds and what it does not (SM-STORE-003/004). They are recorded in
// each manifest so a backup states its own scope.
var (
	Included = []string{
		"database: a consistent snapshot (SQLite VACUUM INTO) of every space, run, queue, inbox item, message, human action " +
			"(including outstanding ones), approval, catalog version (prompts, harness profiles, role templates, skills, " +
			"human-action types, event templates), event connection and subscription with its cursor, user, grant, token " +
			"verifier, the audit chain and the outbox",
		"credentials: accounts, connection credentials, MCP server headers and sign-in provider secrets, sealed by the keyring " +
			"exactly as stored (the keyring's data keys are wrapped under the KEK)",
		"role memory: one Git bundle of every collection central has synced (all refs, including preserved conflicts)",
		"recordings, attachments and artifacts, and run transcripts stored on central, including those in an S3 artifact " +
			"store (artifacts.backend: s3), which are copied into the backup and sealed like local files (ADR-0048)",
		"configuration: the effective central configuration at backup time (settings and file paths, no secret values)",
	}
	Excluded = []string{
		"the KEK and the development KEK file (dev-kek): back the KEK up separately",
		"node workspaces and worktrees, including uncommitted or unpushed changes (they live on nodes only)",
		"role memory a node has not synced to central yet",
		"node-local credentials, journals, leases and harness state (node.credential, the agent journal, harness configuration folders)",
		"native harness state not uploaded yet (transcripts still on nodes, live terminal sessions)",
		"secrets referenced by path in the configuration (TLS keys, Pulse .creds files, S3 key files): back them up with the KEK",
		"state held by other systems: Pulse streams and durable consumers, upstream Git repositories, identity providers",
		"the data-directory lock, pre-migration snapshots and earlier backups",
	}
)

// Errors.
var (
	// ErrInvalid means a backup failed verification.
	ErrInvalid = errors.New("backup verification failed")
	// ErrNotEmpty means the restore target already holds data and Force was not set.
	ErrNotEmpty = errors.New("restore target is not empty")
	// ErrKEKMismatch means the supplied KEK is not the one the backup is sealed under.
	ErrKEKMismatch = errors.New("the supplied key-encryption key (KEK) does not match the backup")
	// ErrExists means the backup destination already exists.
	ErrExists = errors.New("backup destination already exists")
)

// Manifest describes a backup.
type Manifest struct {
	Format          string    `json:"format"`
	Version         int       `json:"version"`
	ID              string    `json:"id"`
	CreatedAt       time.Time `json:"created_at"`
	SpaceManVersion string    `json:"spaceman_version"`
	SchemaVersion   int64     `json:"schema_version"`
	KEKID           string    `json:"kek_id"`
	KEKIncluded     bool      `json:"kek_included"`
	KEKNotice       string    `json:"kek_notice"`
	SourceDataDir   string    `json:"source_data_dir"`
	AuditHead       AuditHead `json:"audit_head"`
	Counts          Counts    `json:"counts"`
	Included        []string  `json:"included"`
	Excluded        []string  `json:"excluded"`
	Files           []File    `json:"files"`
	// Bytes is the plaintext total; SealedBytes what the backup occupies.
	Bytes       int64    `json:"bytes"`
	SealedBytes int64    `json:"sealed_bytes"`
	Warnings    []string `json:"warnings,omitempty"`
	// MemoryFlush records how role memory was prepared before the snapshot (SM-MEM-005, ADR-0045); absent in backups
	// made before it existed.
	MemoryFlush *MemoryFlush `json:"memory_flush,omitempty"`
	// ArtifactStore is where central kept its artifacts when the backup was made ("local", or "s3" with its
	// location); either way the backup holds them (ADR-0048). Absent in backups made before it existed (local).
	ArtifactStore *ArtifactStore `json:"artifact_store,omitempty"`
}

// ArtifactStore records the artifact store a backup copied artifacts from.
type ArtifactStore struct {
	Kind     string `json:"kind" enum:"local,s3"`
	Location string `json:"location,omitempty"`
	// Included is always true: S3 artifacts are copied into the backup, never only referenced.
	Included bool `json:"included"`
	// Objects is how many artifacts were copied from the remote store (0 for the local store, whose files are
	// counted with the other files).
	Objects int `json:"objects"`
}

// MemoryFlush is the outcome of asking the nodes to checkpoint and sync role memory before a backup. A backup holds
// only the memory history central acknowledged; Stale names the spaces whose newest memory may be missing.
type MemoryFlush struct {
	// Attempted is false for offline backups (central not running), which cannot reach nodes.
	Attempted bool       `json:"attempted"`
	Note      string     `json:"note,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	Seconds   float64    `json:"seconds,omitempty" doc:"How long the flush took"`
	// Flushed are the spaces whose node checkpointed and synced all their role memory before the snapshot.
	Flushed []string      `json:"flushed_spaces"`
	Stale   []StaleMemory `json:"stale" doc:"Spaces whose role memory is possibly stale in this backup"`
}

// StaleMemory is a space whose role memory may be newer on its node than in the backup.
type StaleMemory struct {
	SpaceID   string `json:"space_id"`
	SpaceSlug string `json:"space_slug,omitempty"`
	NodeID    string `json:"node_id,omitempty"`
	NodeName  string `json:"node_name,omitempty"`
	Reason    string `json:"reason" enum:"node_disconnected,protocol_too_old,timed_out,not_acknowledged"`
	Detail    string `json:"detail,omitempty"`
}

// OfflineMemoryFlush is what backups made without central running (the CLI) record: no node could be asked.
func OfflineMemoryFlush() *MemoryFlush {
	return &MemoryFlush{Note: "offline backup: central was not running, so no node was asked to flush role memory; " +
		"the backup holds the memory history central had acknowledged", Flushed: []string{}, Stale: []StaleMemory{}}
}

// staleWarning is the manifest warning for spaces whose memory may be stale.
func (f *MemoryFlush) staleWarning() string {
	if f == nil || len(f.Stale) == 0 {
		return ""
	}
	names := make([]string, 0, len(f.Stale))
	for _, s := range f.Stale {
		n := s.SpaceSlug
		if n == "" {
			n = s.SpaceID
		}
		names = append(names, n+" ("+s.Reason+")")
	}
	return "role memory possibly stale for spaces " + strings.Join(names, ", ") + ": their nodes did not confirm a flush before the snapshot"
}

// AuditHead is the last audit-chain entry in the snapshot.
type AuditHead struct {
	Seq  int64  `json:"seq"`
	Hash string `json:"hash,omitempty"`
}

// Counts summarize the snapshot (what the backup preserves).
type Counts struct {
	Spaces              int64 `json:"spaces"`
	Runs                int64 `json:"runs"`
	OpenHumanActions    int64 `json:"open_human_actions"`
	PendingApprovals    int64 `json:"pending_approvals"`
	Subscriptions       int64 `json:"subscriptions"`
	CatalogVersions     int64 `json:"catalog_versions"`
	Accounts            int64 `json:"accounts"`
	MemoryCollections   int64 `json:"memory_collections"`
	Attachments         int64 `json:"attachments"`
	OutboxPendingEvents int64 `json:"outbox_pending_events"`
}

// File is one file of a backup. Path is slash-separated and relative to the backup; the sealed object is at
// Path + SealedSuffix.
type File struct {
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	SealedSize   int64  `json:"sealed_size"`
	SealedSHA256 string `json:"sealed_sha256"`
}

// Object is the sealed object's path inside the backup.
func (f File) Object() string { return f.Path + SealedSuffix }

// Snapshotter writes a consistent copy of the database. ReadOnlySource is preferred both for the CLI and inside the
// server: it needs no data-directory lock and never blocks the store writer.
type Snapshotter interface {
	Snapshot(ctx context.Context, dest string) error
	// Dir is the absolute data directory.
	Dir() string
}

// Sources locate the payload trees outside the database. Empty directories are skipped.
type Sources struct {
	Recordings  string
	Artifacts   string
	Transcripts string
	Memory      string
	// Git creates memory bundles; nil leaves memory out (with a warning).
	Git *memory.Git
	// Store is a remote artifact store (S3) whose artifacts are copied into the backup as well; nil for the local
	// store, whose directories are the trees above.
	Store artifacts.Store
}

// DefaultSources returns the trees inside dataDir; artifactsDir overrides <data>/artifacts when set.
func DefaultSources(dataDir, artifactsDir string, git *memory.Git) Sources {
	if artifactsDir == "" {
		artifactsDir = filepath.Join(dataDir, dirArtifacts)
	}
	return Sources{Recordings: filepath.Join(dataDir, dirRecordings), Artifacts: artifactsDir, Transcripts: filepath.Join(dataDir, dirTranscripts),
		Memory: filepath.Join(dataDir, dirMemory), Git: git}
}

// Options configure Create.
type Options struct {
	// ID names the backup (default: a new bkp_ ID).
	ID      string
	Sources Sources
	// Config is the effective configuration (YAML); nil leaves it out.
	Config []byte
	Clock  clock.Clock
	// MemoryFlush is how role memory was flushed before the snapshot (recorded in the manifest; nil: not recorded).
	MemoryFlush *MemoryFlush
}

// Result is the outcome of Create.
type Result struct {
	Dir      string
	Manifest *Manifest
}

var reBackupID = regexp.MustCompile(`^bkp_[0-9a-z]{26}$`)

// ValidID reports whether id is a backup ID.
func ValidID(id string) bool { return reBackupID.MatchString(id) }

// Create writes a sealed backup of src to dest, which must not exist. The database is snapshotted first (VACUUM INTO,
// no store transaction is held), then the payload trees are sealed file by file. The backup is assembled in a temporary
// sibling directory, fsynced and renamed into place, so dest either holds a complete backup or does not exist.
func Create(ctx context.Context, src Snapshotter, kek keyring.KeyWrapper, dest string, o Options) (*Result, error) {
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.ID == "" {
		o.ID = ids.New(ids.Backup)
	}
	if !ValidID(o.ID) {
		return nil, fmt.Errorf("invalid backup ID %q", o.ID)
	}
	dest, err := filepath.Abs(dest)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(dest); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrExists, dest)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("create backup parent: %w", err)
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(dest)+".tmp-")
	if err != nil {
		return nil, fmt.Errorf("create backup staging dir: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp)
		}
	}()
	if err := os.Chmod(tmp, 0o700); err != nil { //nolint:gosec // G302: a directory needs the search bit; owner only
		return nil, err
	}
	b := &builder{kek: kek, dir: tmp, m: &Manifest{Format: Format, Version: FormatVersion, ID: o.ID, CreatedAt: o.Clock.Now().UTC(),
		SpaceManVersion: buildinfo.Get().Version, KEKNotice: KEKNotice, SourceDataDir: src.Dir(), Included: Included, Excluded: Excluded,
		MemoryFlush: o.MemoryFlush}}
	if w := o.MemoryFlush.staleWarning(); w != "" {
		b.m.Warnings = append(b.m.Warnings, w)
	}
	if err := b.database(ctx, src); err != nil {
		return nil, err
	}
	s := o.Sources
	for _, t := range []struct{ root, dir, kind string }{
		{s.Recordings, dirRecordings, KindRecording},
		{s.Artifacts, dirArtifacts, KindAttachment},
		{s.Transcripts, dirTranscripts, KindTranscript},
	} {
		if err := b.tree(ctx, t.root, t.dir, t.kind); err != nil {
			return nil, err
		}
	}
	if err := b.objects(ctx, s.Store); err != nil {
		return nil, err
	}
	if err := b.memory(ctx, s.Memory, s.Git); err != nil {
		return nil, err
	}
	if o.Config != nil {
		if err := b.bytes(ConfigFile, KindConfig, o.Config); err != nil {
			return nil, err
		}
	}
	if err := writeManifest(tmp, b.m, kek); err != nil {
		return nil, err
	}
	if err := fsyncTree(tmp); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return nil, fmt.Errorf("install backup: %w", err)
	}
	ok = true
	if err := store.FsyncDir(parent); err != nil {
		return nil, err
	}
	return &Result{Dir: dest, Manifest: b.m}, nil
}

type builder struct {
	kek keyring.KeyWrapper
	dir string
	m   *Manifest
}

func (b *builder) add(p, kind string, plain, sealed sizeSum) {
	b.m.Files = append(b.m.Files, File{Path: p, Kind: kind, Size: plain.size, SHA256: plain.sum, SealedSize: sealed.size, SealedSHA256: sealed.sum})
	b.m.Bytes += plain.size
	b.m.SealedBytes += sealed.size
}

func (b *builder) sealPath(p, kind, src string) error {
	plain, sealed, err := sealFile(src, filepath.Join(b.dir, filepath.FromSlash(p+SealedSuffix)), b.kek, SealedKindFile(p), b.m.ID)
	if err != nil {
		return err
	}
	b.add(p, kind, plain, sealed)
	return nil
}

func (b *builder) bytes(p, kind string, data []byte) error {
	plain, sealed, err := sealReader(bytes.NewReader(data), filepath.Join(b.dir, filepath.FromSlash(p+SealedSuffix)), b.kek, SealedKindFile(p), b.m.ID)
	if err != nil {
		return err
	}
	b.add(p, kind, plain, sealed)
	return nil
}

// database snapshots the database into the staging directory, reads what the manifest needs from the copy, seals it
// and removes the plaintext copy.
func (b *builder) database(ctx context.Context, src Snapshotter) error {
	plain := filepath.Join(b.dir, ".snapshot.db")
	if err := src.Snapshot(ctx, plain); err != nil {
		return fmt.Errorf("database snapshot: %w", err)
	}
	defer func() { _ = os.Remove(plain) }()
	info, err := readSnapshot(ctx, plain)
	if err != nil {
		return err
	}
	b.m.SchemaVersion, b.m.AuditHead, b.m.Counts = info.schema, info.head, info.counts
	switch len(info.kekIDs) {
	case 0:
		b.m.KEKID = b.kek.ID()
		b.m.Warnings = append(b.m.Warnings, "the database has no keyring yet")
	case 1:
		if info.kekIDs[0] != b.kek.ID() {
			return fmt.Errorf("%w: the keyring is wrapped under KEK %s, the supplied KEK is %s", ErrKEKMismatch, info.kekIDs[0], b.kek.ID())
		}
		b.m.KEKID = info.kekIDs[0]
	default:
		return fmt.Errorf("keyring keys are wrapped under several KEKs (%s); finish the KEK rotation first", strings.Join(info.kekIDs, ", "))
	}
	return b.sealPath(DBFile, KindDatabase, plain)
}

// tree seals every regular file below root under dir/ in the backup, in path order. Symlinks and special files are
// skipped with a warning; temporary upload files (artifacts tmp/) are skipped silently.
func (b *builder) tree(ctx context.Context, root, dir, kind string) error {
	if root == "" {
		return nil
	}
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			if kind == KindAttachment && rel == "tmp" {
				return fs.SkipDir
			}
			return nil
		case !d.Type().IsRegular():
			b.m.Warnings = append(b.m.Warnings, fmt.Sprintf("%s/%s is not a regular file and was skipped", dir, rel))
			return nil
		case !validRel(rel):
			b.m.Warnings = append(b.m.Warnings, fmt.Sprintf("%s/%s has an unsupported name and was skipped", dir, rel))
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan %s: %w", root, err)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := b.sealPath(path.Join(dir, rel), kind, filepath.Join(root, filepath.FromSlash(rel)))
		if errors.Is(err, fs.ErrNotExist) {
			// Removed after the snapshot (attachment garbage collection, a finished transcript upload's .part file).
			b.m.Warnings = append(b.m.Warnings, fmt.Sprintf("%s/%s disappeared while the backup was taken", dir, rel))
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// objectDirs map artifact-store namespaces to the backup's trees: the same layout as the local store, so a backup
// made with either backend restores the same way.
var objectDirs = map[string]string{artifacts.NSAttachments: dirArtifacts, artifacts.NSRecordings: dirRecordings, artifacts.NSTranscripts: dirTranscripts}

var objectKinds = map[string]string{artifacts.NSAttachments: KindAttachment, artifacts.NSRecordings: KindRecording, artifacts.NSTranscripts: KindTranscript}

// objects copies every artifact of a remote store into the backup, streaming each object into its sealed file. A
// path the local trees already provided (a spooled recording segment or transcript not yet uploaded) is not copied
// twice. Attachment content is checked against its address while it is sealed.
func (b *builder) objects(ctx context.Context, st artifacts.Store) error {
	if st == nil {
		b.m.ArtifactStore = &ArtifactStore{Kind: "local", Included: true}
		return nil
	}
	b.m.ArtifactStore = &ArtifactStore{Kind: st.Kind(), Location: st.Location(), Included: true}
	have := map[string]bool{}
	for _, f := range b.m.Files {
		have[f.Path] = true
	}
	for _, ns := range artifacts.Namespaces {
		objs, err := st.List(ctx, ns+"/")
		if err != nil {
			return fmt.Errorf("list %s in the artifact store: %w", ns, err)
		}
		for _, info := range objs {
			if err := ctx.Err(); err != nil {
				return err
			}
			p := objectDirs[ns] + "/" + strings.TrimPrefix(info.Key, ns+"/")
			if have[p] || !validRel(p) {
				continue
			}
			o, err := st.Open(ctx, info.Key)
			if errors.Is(err, fs.ErrNotExist) {
				b.m.Warnings = append(b.m.Warnings, fmt.Sprintf("%s disappeared from the artifact store while the backup was taken", info.Key))
				continue
			} else if err != nil {
				return fmt.Errorf("open %s in the artifact store: %w", info.Key, err)
			}
			var r io.ReadCloser = o
			if sum, ok := artifacts.AttachmentSum(info.Key); ok {
				r = artifacts.Verified(o, sum)
			}
			plain, sealed, err := sealReader(r, filepath.Join(b.dir, filepath.FromSlash(p+SealedSuffix)), b.kek, SealedKindFile(p), b.m.ID)
			_ = r.Close()
			if err != nil {
				return fmt.Errorf("copy %s from the artifact store: %w", info.Key, err)
			}
			b.add(p, objectKinds[ns], plain, sealed)
			have[p] = true
			b.m.ArtifactStore.Objects++
		}
	}
	return nil
}

var reRepo = regexp.MustCompile(`^spc_[0-9a-z]{26}/[a-z][a-z0-9-]{0,47}\.git$`)

// memory bundles every bare repository <space>/<role>.git with all its refs.
func (b *builder) memory(ctx context.Context, root string, git *memory.Git) error {
	if root == "" {
		return nil
	}
	spaces, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	var repos []string
	for _, sp := range spaces {
		if !sp.IsDir() {
			continue
		}
		roles, err := os.ReadDir(filepath.Join(root, sp.Name()))
		if err != nil {
			return err
		}
		for _, r := range roles {
			rel := sp.Name() + "/" + r.Name()
			if r.IsDir() && reRepo.MatchString(rel) {
				repos = append(repos, rel)
			}
		}
	}
	sort.Strings(repos)
	if len(repos) > 0 && git == nil {
		b.m.Warnings = append(b.m.Warnings, fmt.Sprintf("git is not installed: %d role-memory collection(s) were left out", len(repos)))
		return nil
	}
	for _, rel := range repos {
		if err := ctx.Err(); err != nil {
			return err
		}
		repo := filepath.Join(root, filepath.FromSlash(rel))
		bundle := filepath.Join(b.dir, ".memory.bundle")
		_ = os.Remove(bundle)
		if strings.TrimSpace(mustRun(ctx, git, repo, "for-each-ref", "--count=1")) == "" {
			continue // an empty repository has nothing to bundle
		}
		if _, err := git.Run(ctx, repo, nil, "bundle", "create", "--quiet", bundle, "--all"); err != nil {
			return fmt.Errorf("bundle memory %s: %w", rel, err)
		}
		err := b.sealPath(path.Join(dirMemory, strings.TrimSuffix(rel, ".git")+".bundle"), KindMemory, bundle)
		_ = os.Remove(bundle)
		if err != nil {
			return err
		}
	}
	return nil
}

func mustRun(ctx context.Context, git *memory.Git, dir string, args ...string) string {
	out, _ := git.Run(ctx, dir, nil, args...)
	return out
}

// validRel accepts clean relative slash paths without characters the sealed kind or object names cannot carry.
func validRel(p string) bool {
	return p != "" && path.Clean(p) == p && filepath.IsLocal(filepath.FromSlash(p)) && !strings.ContainsAny(p, "\\|\x00\n\r") &&
		!strings.HasSuffix(p, SealedSuffix)
}

type snapshotInfo struct {
	schema int64
	kekIDs []string
	head   AuditHead
	counts Counts
}

// readSnapshot reads what the manifest needs from the snapshot copy (the database view at time T).
func readSnapshot(ctx context.Context, dbPath string) (*snapshotInfo, error) {
	d, err := openImmutable(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	info := &snapshotInfo{}
	if info.schema, _, err = store.SchemaVersions(ctx, d, store.EmbeddedMigrations()); err != nil {
		return nil, fmt.Errorf("schema version: %w", err)
	}
	if info.kekIDs, err = kekIDs(ctx, d); err != nil {
		return nil, err
	}
	if info.head, err = auditHead(ctx, d); err != nil {
		return nil, err
	}
	info.counts = countRows(ctx, d)
	return info, nil
}

// kekIDs lists the KEKs wrapping the keys a restored keyring loads (retired keys are never loaded).
func kekIDs(ctx context.Context, d *sql.DB) ([]string, error) {
	rows, err := d.QueryContext(ctx, "SELECT DISTINCT wrap_kek_id FROM keyring_keys WHERE state != 'retired' ORDER BY 1")
	if err != nil {
		return nil, fmt.Errorf("read keyring: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func auditHead(ctx context.Context, d *sql.DB) (AuditHead, error) {
	var h AuditHead
	var hash []byte
	err := d.QueryRowContext(ctx, "SELECT seq, hash FROM audit_events ORDER BY seq DESC LIMIT 1").Scan(&h.Seq, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return AuditHead{}, nil
	}
	if err != nil {
		return AuditHead{}, fmt.Errorf("read audit head: %w", err)
	}
	h.Hash = hex.EncodeToString(hash)
	return h, nil
}

// countRows fills the manifest summary. A query that fails (an older schema without the table) counts zero.
func countRows(ctx context.Context, d *sql.DB) Counts {
	n := func(q string) int64 {
		var v int64
		if err := d.QueryRowContext(ctx, q).Scan(&v); err != nil {
			return 0
		}
		return v
	}
	return Counts{
		Spaces:           n("SELECT count(*) FROM spaces"),
		Runs:             n("SELECT count(*) FROM runs"),
		OpenHumanActions: n("SELECT count(*) FROM human_actions WHERE status IN ('open', 'in_progress')"),
		PendingApprovals: n("SELECT count(*) FROM approvals WHERE state = 'pending'"),
		Subscriptions:    n("SELECT count(*) FROM event_subscriptions"),
		CatalogVersions: n("SELECT (SELECT count(*) FROM prompt_versions) + (SELECT count(*) FROM skill_versions) + " +
			"(SELECT count(*) FROM event_template_versions) + (SELECT count(*) FROM human_type_versions)"),
		Accounts:            n("SELECT count(*) FROM accounts"),
		MemoryCollections:   n("SELECT count(*) FROM memory_collections"),
		Attachments:         n("SELECT count(*) FROM attachments"),
		OutboxPendingEvents: n("SELECT count(*) FROM event_outbox WHERE published_at IS NULL AND dead_at IS NULL"),
	}
}

// writeManifest writes manifest.json, its sealed copy and, last, manifest.sha256 (whose presence marks a complete backup).
func writeManifest(dir string, m *Manifest, kek keyring.KeyWrapper) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := writeFileSync(filepath.Join(dir, ManifestFile), b); err != nil {
		return err
	}
	if _, _, err := sealReader(bytes.NewReader(b), filepath.Join(dir, SealedManifestFile), kek, SealedKindManifest, m.ID); err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	return writeFileSync(filepath.Join(dir, ManifestSumFile), []byte(hex.EncodeToString(sum[:])+"  "+ManifestFile+"\n"))
}

// ReadOnlySource snapshots a data directory through a read-only connection without taking the data-directory lock,
// so backups work while the server is running and never block its writer (VACUUM INTO runs in a WAL read transaction).
type ReadOnlySource struct {
	dir string
	db  *sql.DB
}

// OpenReadOnly opens the database of dataDir read-only.
func OpenReadOnly(ctx context.Context, dataDir string) (*ReadOnlySource, error) {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	p := filepath.Join(dir, store.DBFile)
	if _, err := os.Stat(p); err != nil {
		return nil, fmt.Errorf("open database %s: %w", p, err)
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", store.DefaultBusyTimeout.Milliseconds()))
	q.Add("_pragma", "trusted_schema(0)")
	d, err := sql.Open("sqlite", "file:"+p+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(2)
	if err := d.PingContext(ctx); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("open database %s: %w", p, err)
	}
	return &ReadOnlySource{dir: dir, db: d}, nil
}

// Dir implements Snapshotter.
func (r *ReadOnlySource) Dir() string { return r.dir }

// Close closes the connection pool.
func (r *ReadOnlySource) Close() error { return r.db.Close() }

// Snapshot implements Snapshotter with VACUUM INTO on the read-only connection. dest must not exist.
func (r *ReadOnlySource) Snapshot(ctx context.Context, dest string) error {
	dest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("snapshot target %s already exists", dest)
	}
	if strings.ContainsAny(dest, "'\x00") {
		return fmt.Errorf("snapshot path %q contains unsupported characters", dest)
	}
	// VACUUM INTO takes no bind parameter for the file name; dest is absolute and checked for quotes and NUL above.
	if _, err := r.db.ExecContext(ctx, "VACUUM INTO '"+dest+"'"); err != nil { //nolint:gosec // G202: see above
		_ = os.Remove(dest)
		return fmt.Errorf("vacuum into %s: %w", dest, err)
	}
	if err := os.Chmod(dest, 0o600); err != nil {
		return err
	}
	f, err := os.Open(dest) //nolint:gosec // our own file
	if err != nil {
		return err
	}
	err = f.Sync()
	_ = f.Close()
	if err != nil {
		return err
	}
	return store.FsyncDir(filepath.Dir(dest))
}

// openImmutable opens a database file that nothing else writes (a snapshot or a decrypted backup copy) without
// creating journal, WAL or shm files.
func openImmutable(ctx context.Context, p string) (*sql.DB, error) {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Set("immutable", "1")
	q.Add("_pragma", "trusted_schema(0)")
	d, err := sql.Open("sqlite", "file:"+p+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	if err := d.PingContext(ctx); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("open %s: %w", p, err)
	}
	return d, nil
}
