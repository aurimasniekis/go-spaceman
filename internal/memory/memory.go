// Package memory preserves role memory centrally (SM-MEM-*, ADR-0025). A collection is the Git history of one role in
// one space: every instance of the role on the space's node shares it. The node owns the history: it checkpoints the
// instances, publishes to its shared repository and offers new history to central as Git bundles. Central accepts
// only a fast-forward of the head it already has, so it stores acknowledged history and never a competing edit;
// anything else is kept under refs/conflicts and reported. Central reads (files, history) never write.
package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// ReFolder is a valid name of a folder under conflicts/ (an instance and a time stamp).
var ReFolder = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ReRole is a valid collection (role) name.
var ReRole = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}$`)

// Collection is one role's memory in a space.
type Collection struct {
	ID        string                        `json:"id"`
	SpaceID   string                        `json:"space_id"`
	Role      string                        `json:"role"`
	Head      string                        `json:"head,omitempty" doc:"Central's acknowledged head"`
	HeadAt    *time.Time                    `json:"head_at,omitempty"`
	Node      *proto.MemoryCollectionStatus `json:"node,omitempty" doc:"The node's last reported view"`
	NodeAt    *time.Time                    `json:"node_at,omitempty"`
	Conflicts int64                         `json:"open_conflicts"`
	UpdatedAt time.Time                     `json:"updated_at"`
}

// Conflict is history central or the node preserved instead of applying.
type Conflict struct {
	ID         string     `json:"id"`
	Source     string     `json:"source" enum:"central,node"`
	Ref        string     `json:"ref,omitempty"`
	Commit     string     `json:"commit,omitempty"`
	Detail     string     `json:"detail,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy string     `json:"resolved_by,omitempty"`
}

// Commit is one history entry.
type Commit struct {
	ID      string    `json:"id"`
	Author  string    `json:"author"`
	At      time.Time `json:"at"`
	Subject string    `json:"subject"`
}

// Service stores and serves central memory.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	changes *changefeed.Recorder
	git     *Git // nil when git is missing: syncs are refused and reported
	dir     string
	log     *slog.Logger

	mu    sync.Mutex
	locks map[string]*sync.Mutex

	// moves serializes role renames' repository moves (write) against a backup's view of the repositories (read,
	// HoldMoves); deferred records a move a backup held back, for its release to finish.
	moves    sync.RWMutex
	deferred atomic.Bool

	// Outbox records sync events for publication to Pulse (SM-OBS-004). Nil: nothing is published.
	Outbox *outbox.Outbox
}

// New returns the service; repositories live under dir.
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder, dir string, git *Git, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{s: s, audit: aw, changes: ch, git: git, dir: dir, log: log, locks: map[string]*sync.Mutex{}}
}

func (svc *Service) lock(key string) func() {
	svc.mu.Lock()
	l, ok := svc.locks[key]
	if !ok {
		l = &sync.Mutex{}
		svc.locks[key] = l
	}
	svc.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (svc *Service) repo(spaceID, role string) string {
	return filepath.Join(svc.dir, spaceID, role+".git")
}

// collection returns (creating) the collection row.
func (svc *Service) collection(ctx context.Context, spaceID, role string) (db.MemoryCollection, error) {
	c, err := svc.s.R().GetMemoryCollection(ctx, db.GetMemoryCollectionParams{SpaceID: spaceID, Role: role})
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return c, err
	}
	err = svc.s.W().TxNamed(ctx, "memory.collection", func(tx *store.Tx) error {
		if _, err := tx.Q.GetMemoryCollection(ctx, db.GetMemoryCollectionParams{SpaceID: spaceID, Role: role}); err == nil {
			return nil
		}
		id := ids.New(ids.MemoryColl)
		if err := tx.Q.InsertMemoryCollection(ctx, db.InsertMemoryCollectionParams{ID: id, SpaceID: spaceID, Role: role, CreatedAt: tx.NowMicros(),
			UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		return svc.changes.Record(ctx, tx, spaceID, "memory", id, "created", 1)
	})
	if err != nil {
		return db.MemoryCollection{}, err
	}
	return svc.s.R().GetMemoryCollection(ctx, db.GetMemoryCollectionParams{SpaceID: spaceID, Role: role})
}

// ownedBy checks that the space is assigned to the node (only the owning node writes a space's memory, SM-MEM-006).
func (svc *Service) ownedBy(ctx context.Context, nodeID, spaceID string) error {
	sp, err := svc.s.R().GetSpace(ctx, spaceID)
	if err != nil || sp.NodeID != nodeID {
		return errors.New("the space is not assigned to this node")
	}
	return nil
}

// Apply stores new history from the space's node. It runs git outside any database transaction.
func (svc *Service) Apply(ctx context.Context, nodeID string, m proto.MemorySync) proto.MemoryAck {
	ack := proto.MemoryAck{SyncID: m.SyncID, SpaceID: m.SpaceID, Role: m.Role, Result: proto.MemoryError}
	fail := func(msg string, err error) proto.MemoryAck {
		ack.Message = msg
		svc.log.Warn("memory sync refused", slog.String("space.id", m.SpaceID), slog.String("memory.role", m.Role), slog.String("reason", msg), slog.Any("error", err))
		return ack
	}
	if !ReRole.MatchString(m.Role) || !ValidCommit(m.Head) || (m.Parent != "" && !ValidCommit(m.Parent)) || len(m.Bundle) == 0 || len(m.Bundle) > proto.MemoryMaxBundle {
		return fail("invalid sync", nil)
	}
	if err := svc.ownedBy(ctx, nodeID, m.SpaceID); err != nil {
		return fail(err.Error(), nil)
	}
	if svc.git == nil {
		return fail("central cannot store memory: git is not installed", nil)
	}
	if err := svc.ensureMoved(ctx, m.SpaceID); err != nil {
		return fail("internal error", err)
	}
	if away, err := svc.renamedAway(ctx, m.SpaceID, m.Role); err != nil {
		return fail("internal error", err)
	} else if away {
		// The node sends this history again under the new name once it has moved the role's folder (ADR-0093).
		return fail(ErrRoleRenamed, nil)
	}
	defer svc.lock(m.SpaceID + "/" + m.Role)()
	col, err := svc.collection(ctx, m.SpaceID, m.Role)
	if err != nil {
		return fail("internal error", err)
	}
	ack.Head = col.Head
	dir := svc.repo(m.SpaceID, m.Role)
	if err := svc.git.InitBare(ctx, dir); err != nil {
		return fail("internal error", err)
	}
	incoming := "refs/incoming/" + m.SyncID
	got, err := svc.git.Unbundle(ctx, dir, m.Bundle, "refs/heads/main", incoming)
	defer func() { _, _ = svc.git.Run(context.WithoutCancel(ctx), dir, nil, "update-ref", "-d", incoming) }()
	if errors.Is(err, ErrMissingBase) {
		ack.Result, ack.Message = proto.MemoryMissingBase, "central does not have the base commit; send the full history"
		return ack
	}
	if err != nil {
		return fail("the bundle could not be applied", err)
	}
	if got != m.Head {
		return fail("the bundle does not contain the announced head", nil)
	}
	cur := svc.git.RevParse(ctx, dir, "refs/heads/main")
	if cur != col.Head {
		// The repository and the database disagree (for example after a restore); the database is the record.
		cur = col.Head
	}
	if cur == m.Head {
		ack.Result = proto.MemoryOK
		return ack
	}
	if cur != "" && !svc.git.IsAncestor(ctx, dir, cur, m.Head) {
		ref := fmt.Sprintf("refs/conflicts/node-%d", time.Now().UnixNano())
		if _, err := svc.git.Run(ctx, dir, nil, "update-ref", ref, m.Head); err != nil {
			return fail("internal error", err)
		}
		if err := svc.conflict(ctx, col, "central", ref, m.Head, "the node's history does not continue central's acknowledged history", ""); err != nil {
			return fail("internal error", err)
		}
		ack.Result, ack.Message = proto.MemoryConflict, "the history does not continue central's head; it was kept as "+ref
		return ack
	}
	args := []string{"update-ref", "refs/heads/main", m.Head}
	if cur != "" {
		args = append(args, cur)
	} else {
		args = append(args, strings.Repeat("0", len(m.Head)))
	}
	if _, err := svc.git.Run(ctx, dir, nil, args...); err != nil {
		return fail("internal error", err)
	}
	removed, rerr := svc.removedFolders(ctx, dir, cur, m.Head)
	if rerr != nil {
		svc.log.Warn("memory conflict folders unreadable; no conflict auto-resolved", slog.String("space.id", col.SpaceID), slog.String("memory.role", col.Role), slog.String("error", rerr.Error()))
	}
	err = svc.s.W().TxNamed(ctx, "memory.apply", func(tx *store.Tx) error {
		n, err := tx.Q.SetMemoryHead(ctx, db.SetMemoryHeadParams{Head: m.Head, Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ID: col.ID, Expected: col.Head})
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New("the collection head changed concurrently")
		}
		if err := svc.changes.Record(ctx, tx, col.SpaceID, "memory", col.ID, "updated", 0); err != nil {
			return err
		}
		if _, err := svc.Outbox.Enqueue(ctx, tx, outbox.Event{Payload: outbox.MemorySynced{SpaceID: col.SpaceID, CollectionID: col.ID, Role: col.Role,
			NodeID: nodeID, FromCommit: col.Head, ToCommit: m.Head}, Correlation: col.ID, Causation: m.SyncID}); err != nil {
			return err
		}
		if _, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorNode, ID: nodeID}, Action: "memory.synced",
			Target: audit.Target{Type: "memory", ID: col.ID}, Details: map[string]any{"space_id": col.SpaceID, "role": col.Role, "from": col.Head, "to": m.Head}}); err != nil {
			return err
		}
		return svc.autoResolve(ctx, tx, col, removed, m.Head)
	})
	if err != nil {
		// Roll the repository back so it matches the record.
		rb := []string{"update-ref", "-d", "refs/heads/main"}
		if col.Head != "" {
			rb = []string{"update-ref", "refs/heads/main", col.Head, m.Head}
		}
		_, _ = svc.git.Run(context.WithoutCancel(ctx), dir, nil, rb...)
		return fail("internal error", err)
	}
	ack.Result, ack.Head = proto.MemoryOK, m.Head
	return ack
}

func (svc *Service) conflict(ctx context.Context, col db.MemoryCollection, source, ref, commit, detail, folder string) error {
	return svc.s.W().TxNamed(ctx, "memory.conflict", func(tx *store.Tx) error {
		id := ids.New("mcf")
		if err := tx.Q.InsertMemoryConflict(ctx, db.InsertMemoryConflictParams{ID: id, CollectionID: col.ID, Source: source, Ref: ref, CommitID: commit,
			Detail: detail, CreatedAt: tx.NowMicros(), Folder: folder}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, col.SpaceID, "memory", col.ID, "updated", 0); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorSystem, ID: "memory"}, Action: "memory.conflict",
			Target: audit.Target{Type: "memory", ID: col.ID}, Details: map[string]any{"space_id": col.SpaceID, "role": col.Role, "source": source, "ref": ref}})
		return err
	})
}

// Report stores the node's view of its collections, and records conflicts the node preserved.
func (svc *Service) Report(ctx context.Context, nodeID string, st proto.MemoryStatus) {
	moved := map[string]error{}
	for _, c := range st.Collections {
		if !ReRole.MatchString(c.Role) || svc.ownedBy(ctx, nodeID, c.SpaceID) != nil {
			continue
		}
		err, ok := moved[c.SpaceID]
		if !ok {
			err = svc.ensureMoved(ctx, c.SpaceID)
			moved[c.SpaceID] = err
		}
		if err != nil {
			continue
		}
		// A folder under a name that was renamed away is reported under its new name once the node has moved it.
		if away, err := svc.renamedAway(ctx, c.SpaceID, c.Role); err != nil || away {
			continue
		}
		col, err := svc.collection(ctx, c.SpaceID, c.Role)
		if err != nil {
			continue
		}
		var prev proto.MemoryCollectionStatus
		_ = json.Unmarshal([]byte(col.NodeStatusJson), &prev)
		b, _ := json.Marshal(c)
		err = svc.s.W().TxNamed(ctx, "memory.status", func(tx *store.Tx) error {
			if err := tx.Q.SetMemoryNodeStatus(ctx, db.SetMemoryNodeStatusParams{NodeStatusJson: string(b), StatusAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
				UpdatedAt: tx.NowMicros(), ID: col.ID}); err != nil {
				return err
			}
			return svc.changes.Record(ctx, tx, c.SpaceID, "memory", col.ID, "updated", 0)
		})
		if err != nil {
			svc.log.Warn("store memory status", slog.Any("error", err))
			continue
		}
		if c.Conflicts > prev.Conflicts {
			svc.nodeConflicts(ctx, col, c.Conflicts-prev.Conflicts, c.PreservedFolders)
		}
	}
}

// nodeConflicts records n conflicts the node preserved. The node reports the folder of each conflict it preserved
// (oldest first, one per increment of its counter), so the last n entries belong to these n conflicts and each is tied
// to its folder exactly; the tie is never inferred from names or order. A conflict the report has no entry for (a
// node that predates the field, or one that kept fewer than n) is recorded without a folder, as is an entry that is not
// a valid name or is already tied to a conflict.
func (svc *Service) nodeConflicts(ctx context.Context, col db.MemoryCollection, n int, preserved []string) {
	known, _ := svc.s.R().ListMemoryConflictFolders(ctx, col.ID)
	have := map[string]bool{}
	for _, f := range known {
		have[f] = true
	}
	for i := 0; i < n; i++ {
		folder, ref := "", ""
		// Entry j of the list is the conflict n-len(preserved)+j of these n: the list's tail lines up with the new ones.
		if j := i + len(preserved) - n; j >= 0 {
			if f := preserved[j]; ReFolder.MatchString(f) && !have[f] {
				folder, ref = f, "refs/conflicts/"+f
				have[f] = true
			}
		}
		_ = svc.conflict(ctx, col, "node", ref, "", "the node preserved a conflicting change between instances; see conflicts/ in the collection", folder)
	}
}

func colOut(c db.MemoryCollection, open int64) *Collection {
	o := &Collection{ID: c.ID, SpaceID: c.SpaceID, Role: c.Role, Head: c.Head, Conflicts: open, UpdatedAt: time.UnixMicro(c.UpdatedAt).UTC()}
	if c.HeadAt.Valid {
		t := time.UnixMicro(c.HeadAt.Int64).UTC()
		o.HeadAt = &t
	}
	if c.StatusAt.Valid {
		t := time.UnixMicro(c.StatusAt.Int64).UTC()
		o.NodeAt = &t
		var st proto.MemoryCollectionStatus
		if json.Unmarshal([]byte(c.NodeStatusJson), &st) == nil {
			o.Node = &st
		}
	}
	return o
}

// List returns a space's collections.
func (svc *Service) List(ctx context.Context, spaceID string) ([]*Collection, error) {
	rows, err := svc.s.R().ListMemoryCollections(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make([]*Collection, 0, len(rows))
	for _, r := range rows {
		n, _ := svc.s.R().CountOpenMemoryConflicts(ctx, r.ID)
		out = append(out, colOut(r, n))
	}
	return out, nil
}

// Get returns one collection.
func (svc *Service) Get(ctx context.Context, spaceID, role string) (*Collection, error) {
	c, err := svc.s.R().GetMemoryCollection(ctx, db.GetMemoryCollectionParams{SpaceID: spaceID, Role: role})
	if err != nil {
		return nil, apperr.New(apperr.CodeNotFound, "no memory for role %q in this space", role)
	}
	n, _ := svc.s.R().CountOpenMemoryConflicts(ctx, c.ID)
	return colOut(c, n), nil
}

func (svc *Service) readable(ctx context.Context, spaceID, role string) (string, string, error) {
	if err := svc.ensureMoved(ctx, spaceID); err != nil {
		return "", "", err
	}
	c, err := svc.Get(ctx, spaceID, role)
	if err != nil {
		return "", "", err
	}
	if c.Head == "" {
		return "", "", apperr.New(apperr.CodeNotFound, "no history has been acknowledged yet")
	}
	if svc.git == nil {
		return "", "", apperr.New(apperr.CodeUnavailable, "git is not installed on central")
	}
	return svc.repo(spaceID, role), c.Head, nil
}

// Files lists the files of the acknowledged head.
func (svc *Service) Files(ctx context.Context, spaceID, role string) ([]string, error) {
	dir, head, err := svc.readable(ctx, spaceID, role)
	if err != nil {
		return nil, err
	}
	out, err := svc.git.Run(ctx, dir, nil, "ls-tree", "-r", "--name-only", "-z", head)
	if err != nil {
		return nil, err
	}
	files := []string{}
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// MaxFileBytes bounds a file read through the API.
const MaxFileBytes = 1 << 20

// File returns one file of the acknowledged head.
func (svc *Service) File(ctx context.Context, spaceID, role, p string) (string, error) {
	clean := path.Clean("/" + p)[1:]
	if clean == "" || clean != p || strings.HasPrefix(clean, ".git") {
		return "", apperr.New(apperr.CodeInvalid, "invalid path")
	}
	dir, head, err := svc.readable(ctx, spaceID, role)
	if err != nil {
		return "", err
	}
	size, err := svc.git.Run(ctx, dir, nil, "cat-file", "-s", head+":"+clean)
	if err != nil {
		return "", apperr.New(apperr.CodeNotFound, "no such file")
	}
	var n int
	_, _ = fmt.Sscan(strings.TrimSpace(size), &n)
	if n > MaxFileBytes {
		return "", apperr.New(apperr.CodeInvalid, "the file is larger than %d bytes", MaxFileBytes)
	}
	return svc.git.Run(ctx, dir, nil, "show", head+":"+clean)
}

// History returns the most recent acknowledged commits.
func (svc *Service) History(ctx context.Context, spaceID, role string, limit int) ([]Commit, error) {
	dir, head, err := svc.readable(ctx, spaceID, role)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out, err := svc.git.Run(ctx, dir, nil, "log", fmt.Sprintf("-n%d", limit), "--format=%H%x1f%an%x1f%at%x1f%s%x1e", head)
	if err != nil {
		return nil, err
	}
	res := []Commit{}
	for _, rec := range strings.Split(out, "\x1e") {
		f := strings.Split(strings.TrimSpace(rec), "\x1f")
		if len(f) != 4 {
			continue
		}
		var sec int64
		_, _ = fmt.Sscan(f[2], &sec)
		res = append(res, Commit{ID: f[0], Author: f[1], At: time.Unix(sec, 0).UTC(), Subject: f[3]})
	}
	return res, nil
}

// Conflicts lists a collection's conflicts.
func (svc *Service) Conflicts(ctx context.Context, spaceID, role string) ([]Conflict, error) {
	c, err := svc.Get(ctx, spaceID, role)
	if err != nil {
		return nil, err
	}
	rows, err := svc.s.R().ListMemoryConflicts(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	out := []Conflict{}
	for _, r := range rows {
		x := Conflict{ID: r.ID, Source: r.Source, Ref: r.Ref, Commit: r.CommitID, Detail: r.Detail, CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), ResolvedBy: r.ResolvedBy}
		if r.ResolvedAt.Valid {
			t := time.UnixMicro(r.ResolvedAt.Int64).UTC()
			x.ResolvedAt = &t
		}
		out = append(out, x)
	}
	return out, nil
}

// removedFolders lists the conflicts/ folders the old head had and the new head no longer has. A folder that never
// was in an accepted head is not in the list. When either head cannot be read it returns an error and the caller
// resolves nothing: a failed read is not evidence that the folders are gone.
func (svc *Service) removedFolders(ctx context.Context, dir, from, to string) ([]string, error) {
	if from == "" {
		return nil, nil
	}
	was, err := svc.git.ConflictFolders(ctx, dir, from)
	if err != nil {
		return nil, err
	}
	is, err := svc.git.ConflictFolders(ctx, dir, to)
	if err != nil {
		return nil, err
	}
	now := map[string]bool{}
	for _, f := range is {
		now[f] = true
	}
	var gone []string
	for _, f := range was {
		if !now[f] {
			gone = append(gone, f)
		}
	}
	return gone, nil
}

// autoResolve closes the conflicts whose folder an accepted checkpoint removed: the role reconciled them, as the
// folder's README asks. The preserved ref stays. It is idempotent: only open conflicts are touched.
func (svc *Service) autoResolve(ctx context.Context, tx *store.Tx, col db.MemoryCollection, folders []string, head string) error {
	for _, f := range folders {
		ids, err := tx.Q.ResolveMemoryConflictsByFolder(ctx, db.ResolveMemoryConflictsByFolderParams{ResolvedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true},
			ResolvedBy: AutoResolver.ID, CollectionID: col.ID, Folder: f})
		if err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: AutoResolver, Action: "memory.conflict_resolved", Target: audit.Target{Type: "memory", ID: col.ID},
				Details: map[string]any{"space_id": col.SpaceID, "role": col.Role, "conflict": id, "folder": f, "head": head, "auto": true}}); err != nil {
				return err
			}
		}
	}
	return nil
}

// AutoResolver is the audited actor of conflicts closed because their folder was removed.
var AutoResolver = audit.Actor{Type: audit.ActorSystem, ID: "memory-reconcile"}

// ResolveConflict records that a person reconciled a conflict. The preserved history stays.
func (svc *Service) ResolveConflict(ctx context.Context, actor audit.Actor, spaceID, role, id string) error {
	c, err := svc.Get(ctx, spaceID, role)
	if err != nil {
		return err
	}
	return svc.s.W().TxNamed(ctx, "memory.resolve", func(tx *store.Tx) error {
		r, err := tx.Q.GetMemoryConflict(ctx, id)
		if err != nil || r.CollectionID != c.ID {
			return apperr.New(apperr.CodeNotFound, "conflict not found")
		}
		n, err := tx.Q.ResolveMemoryConflict(ctx, db.ResolveMemoryConflictParams{ResolvedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, ResolvedBy: actor.ID, ID: id})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodePrecondition, "the conflict is already resolved")
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "memory", c.ID, "updated", 0); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "memory.conflict_resolved", Target: audit.Target{Type: "memory", ID: c.ID},
			Details: map[string]any{"space_id": spaceID, "role": role, "conflict": id}})
		return err
	})
}

// ExportBundles writes one Git bundle of the acknowledged history (refs/heads/main at the recorded head) of every
// collection of the space that has one, as memory-<role>.bundle in dir, for a relocation's destination (ADR-0037).
// The destination starts from exactly what central acknowledged; history central never received is not in it.
func (svc *Service) ExportBundles(ctx context.Context, spaceID, dir string) ([]proto.RelocateMemory, error) {
	if err := svc.ensureMoved(ctx, spaceID); err != nil {
		return nil, err
	}
	rows, err := svc.s.R().ListMemoryCollections(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := []proto.RelocateMemory{}
	for _, c := range rows {
		if c.Head == "" {
			continue
		}
		if svc.git == nil {
			return nil, errors.New("git is not installed on central")
		}
		if !ReRole.MatchString(c.Role) || !ValidCommit(c.Head) {
			return nil, fmt.Errorf("collection %q has an invalid record", c.Role)
		}
		unlock := svc.lock(spaceID + "/" + c.Role)
		repo := svc.repo(spaceID, c.Role)
		name := "memory-" + c.Role + ".bundle"
		ref := "refs/relocation/main"
		_, err := svc.git.Run(ctx, repo, nil, "update-ref", ref, c.Head)
		if err == nil {
			_, err = svc.git.Run(ctx, repo, nil, "bundle", "create", "--quiet", filepath.Join(dir, name), ref)
		}
		_, _ = svc.git.Run(context.WithoutCancel(ctx), repo, nil, "update-ref", "-d", ref)
		unlock()
		if err != nil {
			return nil, fmt.Errorf("bundle the %s memory: %w", c.Role, err)
		}
		out = append(out, proto.RelocateMemory{Role: c.Role, Head: c.Head, Blob: name})
	}
	return out, nil
}

// Available reports whether central can store memory.
func (svc *Service) Available() bool { return svc.git != nil }
