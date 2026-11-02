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
		if err := svc.conflict(ctx, col, "central", ref, m.Head, "the node's history does not continue central's acknowledged history"); err != nil {
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
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorNode, ID: nodeID}, Action: "memory.synced",
			Target: audit.Target{Type: "memory", ID: col.ID}, Details: map[string]any{"space_id": col.SpaceID, "role": col.Role, "from": col.Head, "to": m.Head}})
		return err
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

func (svc *Service) conflict(ctx context.Context, col db.MemoryCollection, source, ref, commit, detail string) error {
	return svc.s.W().TxNamed(ctx, "memory.conflict", func(tx *store.Tx) error {
		id := ids.New("mcf")
		if err := tx.Q.InsertMemoryConflict(ctx, db.InsertMemoryConflictParams{ID: id, CollectionID: col.ID, Source: source, Ref: ref, CommitID: commit,
			Detail: detail, CreatedAt: tx.NowMicros()}); err != nil {
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
	for _, c := range st.Collections {
		if !ReRole.MatchString(c.Role) || svc.ownedBy(ctx, nodeID, c.SpaceID) != nil {
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
			_ = svc.conflict(ctx, col, "node", "", "", fmt.Sprintf("the node preserved %d conflicting change(s) between instances; see conflicts/ in the collection", c.Conflicts-prev.Conflicts))
		}
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

// Available reports whether central can store memory.
func (svc *Service) Available() bool { return svc.git != nil }
