// Package repos is the central configuration of a space's repositories and the node's view of them (SM-REPO-001,
// SM-REPO-003, SM-SPACE-001, ADR-0044).
//
// A space lists its repositories: a name, the folder in the space folder, the clone URL (never with credentials: Git
// authentication is the node's own, SM-REPO-005), the default branch, a role (code or kb) and a clone policy. Central
// asks the space's node to materialize them (space.repos.sync): missing repositories are cloned according to their
// policy, fetched on request, and every repository is inspected; the node reports remotes, the default branch on the
// remote and locally, the last fetch, changes, untracked files, unpushed commits and each worktree's state. Writer
// worktrees and knowledge-base sessions use the configured repositories (cloned at their first use), and relocation
// reads the list.
//
// SpaceMan never resets, merges, pushes or deletes anything in a repository (SM-REPO-006): removing a repository from
// the list leaves its folder on the node, and deleting a space never touches upstream repositories (SM-SPACE-004).
// Repository content, branch names and paths in a report come from the node and from agent-writable repositories; they
// are untrusted data.
package repos

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/gitkeys"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Roles and limits.
const (
	RoleCode = "code"
	RoleKB   = "kb"

	// MaxPerSpace bounds a space's repositories (one sync inspects them all).
	MaxPerSpace = 50
	// autoDebounce is how long an unanswered automatic sync suppresses the next automatic one.
	autoDebounce = 30 * time.Second
	// maxBranches bounds the branches kept per repository in a stored report.
	maxBranches = 200
)

var (
	reName   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	reBranch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)
	system   = audit.Actor{Type: audit.ActorSystem, ID: "repos"}

	errNotFound = apperr.New(apperr.CodeNotFound, "repository not found")
	policies    = []string{proto.CloneAuto, proto.CloneOnDemand, proto.CloneManual}
	// reserved are the space folder's folders SpaceMan and the harnesses own.
	reserved = []string{"instances", "worktrees", "work", "dreams"}
)

// Repository is a configured repository with the node's latest view of it.
type Repository struct {
	ID            string   `json:"id"`
	SpaceID       string   `json:"space_id"`
	Name          string   `json:"name" doc:"Unique in the space; fixed after creation"`
	Path          string   `json:"path" doc:"Folder relative to the space folder on the node; fixed after creation"`
	Description   string   `json:"description"`
	URL           string   `json:"url" doc:"Clone URL without credentials; empty for a manual repository. With an SSH key SpaceMan holds the credential, otherwise the node's own Git authentication is used"`
	DefaultBranch string   `json:"default_branch"`
	Role          string   `json:"role" enum:"code,kb"`
	ClonePolicy   string   `json:"clone_policy" enum:"auto,on_demand,manual" doc:"auto: cloned while the node holds the space's lease; on_demand: cloned when a run needs it or on a sync; manual: never cloned by SpaceMan"`
	KBs           []string `json:"knowledge_bases" doc:"Slugs of knowledge bases that use this repository"`
	// SSHKey is the repository's SpaceMan SSH key (ADR-0057): only its public half and fingerprint ever leave central.
	SSHKey     *SSHKey    `json:"ssh_key,omitempty" doc:"The repository's own SSH key SpaceMan clones and fetches with (add the public key to the Git host as a read-only deploy key)"`
	GitKey     *GitKeyRef `json:"git_key,omitempty" doc:"The shared Git SSH key SpaceMan clones and fetches with instead (ADR-0058)"`
	KnownHosts string     `json:"known_hosts" doc:"known_hosts lines pinning the Git host's keys for the SSH key; empty: SpaceMan's built-in pins (github.com)"`
	Status     *Status    `json:"status,omitempty" doc:"The node's latest report (runtime state, untrusted: it comes from the node and the repository)"`
	Version    int64      `json:"version"`
	CreatedAt  time.Time  `json:"created_at"`
	CreatedBy  string     `json:"created_by"`
	UpdatedAt  time.Time  `json:"updated_at"`
	UpdatedBy  string     `json:"updated_by"`
}

// SSHKey is the public half of a repository's SpaceMan SSH key.
type SSHKey struct {
	PublicKey   string    `json:"public_key" doc:"authorized_keys format; add it to the Git host as a read-only deploy key"`
	Fingerprint string    `json:"fingerprint" doc:"SHA256 fingerprint"`
	CreatedAt   time.Time `json:"created_at"`
	Note        string    `json:"note,omitempty" doc:"Why the key is not in use (an outdated node agent)"`
}

// GitKeyRef is the shared Git SSH key a repository uses.
type GitKeyRef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	Note        string `json:"note,omitempty" doc:"Why the key is not in use (no longer allowed for the space, or an outdated node agent)"`
}

// Status is a repository as the space's node last reported it.
type Status struct {
	State      string                 `json:"state" enum:"present,missing,error" doc:"present: the folder holds a Git repository; missing: not cloned (yet); error: see error"`
	Stale      bool                   `json:"stale" doc:"The report is from an earlier node of the space (before a relocation)"`
	ReportedAt time.Time              `json:"reported_at"`
	Remote     *proto.RepoRemoteState `json:"remote,omitempty"`
	// DefaultHead is the local default branch's commit, TrackingHead its remote-tracking branch.
	DefaultHead  string     `json:"default_head,omitempty"`
	TrackingHead string     `json:"tracking_head,omitempty"`
	LastFetchAt  *time.Time `json:"last_fetch_at,omitempty"`
	Branch       string     `json:"branch,omitempty" doc:"The main folder's checked-out branch"`
	Head         string     `json:"head,omitempty"`
	Dirty        int        `json:"dirty" doc:"Changed tracked files in the main folder"`
	Untracked    int        `json:"untracked"`
	Stashes      int        `json:"stashes"`
	// Unpushed is the number of commits on local branches that no remote has, over every branch.
	Unpushed  int                 `json:"unpushed"`
	Branches  []proto.BranchCheck `json:"branches"`
	Worktrees []Worktree          `json:"worktrees"`
	Cloned    bool                `json:"cloned,omitempty" doc:"The last sync cloned it"`
	Fetched   bool                `json:"fetched,omitempty" doc:"The last sync fetched it"`
	Error     string              `json:"error,omitempty"`
}

// Worktree is a linked worktree of a repository with its owner when SpaceMan made it for a run.
type Worktree struct {
	Path      string `json:"path"`
	Branch    string `json:"branch,omitempty"`
	Head      string `json:"head"`
	Dirty     int    `json:"dirty"`
	Untracked int    `json:"untracked"`
	Unpushed  int    `json:"unpushed" doc:"Commits on the worktree's branch that no remote has"`
	RunID     string `json:"run_id,omitempty" doc:"The latest run SpaceMan launched in this worktree"`
	Error     string `json:"error,omitempty"`
}

// Input creates or edits a repository. Name and path are fixed after creation.
type Input struct {
	Name          string `json:"name,omitempty" maxLength:"63" doc:"Required on create: lowercase letters, digits, . _ -; fixed after creation"`
	Path          string `json:"path,omitempty" maxLength:"256" doc:"Folder in the space folder (default the name); fixed after creation"`
	Description   string `json:"description,omitempty" maxLength:"2000"`
	URL           string `json:"url,omitempty" maxLength:"2000" doc:"https:// or ssh:// URL or scp-like address, without credentials; required unless clone_policy is manual"`
	DefaultBranch string `json:"default_branch,omitempty" maxLength:"100" doc:"Default main"`
	Role          string `json:"role,omitempty" enum:"code,kb" doc:"Default code"`
	ClonePolicy   string `json:"clone_policy,omitempty" enum:"auto,on_demand,manual" doc:"Default auto with a URL, manual without"`
	KnownHosts    string `json:"known_hosts,omitempty" maxLength:"16384" doc:"known_hosts lines pinning the Git host's SSH keys, used with the repository's SSH key; empty: the built-in pins (github.com)"`
	GitKeyID      string `json:"git_key_id,omitempty" maxLength:"64" doc:"A shared Git SSH key (gky_…) the space may use; empty: the repository's own key, if any, or the node's own Git authentication"`
}

// SyncInput asks the node for a sync.
type SyncInput struct {
	Repositories []string `json:"repositories,omitempty" maxItems:"50" doc:"Names or IDs to fetch; empty fetches every repository with a URL"`
	Fetch        *bool    `json:"fetch,omitempty" doc:"Fetch from the remotes (default true); false only clones what is missing and refreshes the report"`
}

// SyncResult is a queued sync.
type SyncResult struct {
	CommandID   string    `json:"command_id"`
	RequestedAt time.Time `json:"requested_at"`
	Clone       []string  `json:"clone" doc:"Repositories the node clones when missing"`
	Fetch       []string  `json:"fetch" doc:"Repositories the node fetches"`
}

// Overview is a space's repositories with the report's space-level findings.
type Overview struct {
	Items       []*Repository `json:"items"`
	Unlisted    []string      `json:"unlisted" doc:"Repositories the node found in the space folder that are not configured"`
	ReportedAt  *time.Time    `json:"reported_at,omitempty"`
	ReportNode  string        `json:"report_node_id,omitempty"`
	Stale       bool          `json:"stale" doc:"The report is from an earlier node of the space"`
	SyncPending bool          `json:"sync_pending" doc:"A sync was requested after the last report"`
	RequestedAt *time.Time    `json:"requested_at,omitempty"`
}

// Options configure the service.
type Options struct {
	// AllowLocalURLs accepts file://, http://, git:// URLs and local paths (development mode only).
	AllowLocalURLs bool
	// Keyring seals repository SSH keys (ADR-0057); nil: SSH keys are unavailable.
	Keyring *keyring.Keyring
	// GitKeys holds shared Git SSH keys (ADR-0058); nil: none can be used.
	GitKeys *gitkeys.Service
	Clock   clock.Clock
	Log     *slog.Logger
}

// Service manages space repositories.
type Service struct {
	s       *store.Store
	audit   *audit.Writer
	changes *changefeed.Recorder
	fleet   *fleet.Service
	o       Options
	base    context.Context
}

// New returns the service.
func New(s *store.Store, aw *audit.Writer, ch *changefeed.Recorder, fl *fleet.Service, o Options) *Service {
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Service{s: s, audit: aw, changes: ch, fleet: fl, o: o, base: context.Background()}
}

func actorRef(a audit.Actor) string {
	if a.Type == audit.ActorRoot {
		return "root"
	}
	return a.Type + ":" + a.ID
}

func out(r db.SpaceRepository) *Repository {
	o := &Repository{ID: r.ID, SpaceID: r.SpaceID, Name: r.Name, Path: r.Path, Description: r.Description, URL: r.Url, DefaultBranch: r.DefaultBranch,
		Role: r.Role, ClonePolicy: r.ClonePolicy, KBs: []string{}, KnownHosts: r.KnownHosts, Version: r.Version, CreatedAt: time.UnixMicro(r.CreatedAt).UTC(),
		CreatedBy: r.CreatedBy, UpdatedAt: time.UnixMicro(r.UpdatedAt).UTC(), UpdatedBy: r.UpdatedBy}
	if r.SshPublicKey != "" && len(r.SshKeyEnc) > 0 {
		o.SSHKey = &SSHKey{PublicKey: r.SshPublicKey, Fingerprint: fingerprint(r.SshPublicKey), CreatedAt: time.UnixMicro(r.SshKeyCreatedAt.Int64).UTC()}
	}
	return o
}

// ValidPath reports whether p is a usable repository folder: relative, clean, at most 8 levels deep, outside the
// folders SpaceMan and the harnesses own.
func ValidPath(p string) bool {
	if p == "" || len(p) > 256 || path.IsAbs(p) || path.Clean(p) != p || p == "." || strings.HasPrefix(p, "..") || strings.Contains(p, "\\") ||
		strings.Count(p, "/") > 7 || strings.HasPrefix(p, ".spaceman-") || strings.IndexFunc(p, func(r rune) bool { return r < ' ' || r == 0x7f }) >= 0 {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == ".git" || strings.HasPrefix(seg, "-") {
			return false
		}
	}
	first, _, _ := strings.Cut(p, "/")
	return !slices.Contains(reserved, first)
}

// ValidBranch reports whether b is a plain branch name.
func ValidBranch(b string) bool {
	return reBranch.MatchString(b) && !strings.Contains(b, "..") && !strings.HasSuffix(b, ".lock") && !strings.HasSuffix(b, "/") && !strings.Contains(b, "//")
}

// normalize validates an input; cur is the stored repository on update.
func (svc *Service) normalize(in *Input, cur *db.SpaceRepository) error {
	v := &apperr.Validation{}
	in.Name, in.Path, in.URL, in.DefaultBranch = strings.TrimSpace(in.Name), strings.TrimSpace(in.Path), strings.TrimSpace(in.URL), strings.TrimSpace(in.DefaultBranch)
	if cur == nil {
		if !reName.MatchString(in.Name) {
			v.Add("name", "must be 1-63 lowercase letters, digits, dots, dashes or underscores, starting with a letter or digit")
		}
		if in.Path == "" {
			in.Path = in.Name
		}
		if !ValidPath(in.Path) {
			v.Add("path", "must be a relative folder inside the space (not instances/, worktrees/, work/ or dreams/)")
		}
	} else {
		if in.Name != "" && in.Name != cur.Name {
			v.Add("name", "is fixed after creation")
		}
		if in.Path != "" && in.Path != cur.Path {
			v.Add("path", "is fixed after creation; add another repository instead")
		}
		in.Name, in.Path = cur.Name, cur.Path
	}
	if in.DefaultBranch == "" {
		in.DefaultBranch = "main"
	}
	if !ValidBranch(in.DefaultBranch) {
		v.Add("default_branch", "is not a usable branch name")
	}
	if in.Role == "" {
		in.Role = RoleCode
	}
	if in.Role != RoleCode && in.Role != RoleKB {
		v.Add("role", "must be code or kb")
	}
	if in.ClonePolicy == "" {
		in.ClonePolicy = proto.CloneAuto
		if in.URL == "" {
			in.ClonePolicy = proto.CloneManual
		}
	}
	if !slices.Contains(policies, in.ClonePolicy) {
		v.Add("clone_policy", "must be auto, on_demand or manual")
	}
	if in.URL != "" {
		if err := proto.ValidateRepoURL(in.URL, svc.o.AllowLocalURLs); err != nil {
			v.Add("url", "%s", err.Error())
		}
	} else if in.ClonePolicy != proto.CloneManual {
		v.Add("url", "is required unless clone_policy is manual")
	}
	if len(in.Description) > 2000 {
		v.Add("description", "is longer than 2000 characters")
	}
	in.KnownHosts = strings.TrimSpace(strings.ReplaceAll(in.KnownHosts, "\r\n", "\n"))
	if err := ValidateKnownHosts(in.KnownHosts); err != nil {
		v.Add("known_hosts", "%s", err.Error())
	}
	return v.Err()
}

func spaceCheck(ctx context.Context, q *db.Queries, spaceID string) error {
	sp, err := q.GetSpace(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return apperr.New(apperr.CodeNotFound, "space not found")
	} else if err != nil {
		return err
	}
	if sp.State == "archived" {
		return apperr.New(apperr.CodePrecondition, "the space is archived")
	}
	return nil
}

func auditDetails(spaceID string, in Input) map[string]any {
	return map[string]any{"space_id": spaceID, "name": in.Name, "path": in.Path, "url": in.URL, "default_branch": in.DefaultBranch, "role": in.Role,
		"clone_policy": in.ClonePolicy, "git_key_id": in.GitKeyID}
}

// checkGitKey refuses a shared key the space may not use, one on a URL that is not SSH, and one beside the repository's
// own key (a repository uses one key: ADR-0058).
func checkGitKey(ctx context.Context, tx *store.Tx, spaceID string, in Input, ownKey bool) error {
	if in.GitKeyID == "" {
		return nil
	}
	if ownKey {
		return apperr.New(apperr.CodePrecondition, "the repository has its own SSH key; remove it before choosing a shared key")
	}
	if in.URL != "" && !proto.IsSSHURL(in.URL) {
		return apperr.New(apperr.CodeInvalid, "a Git SSH key needs an SSH URL (git@host:owner/repo.git or ssh://)")
	}
	ok, err := gitkeys.Allowed(ctx, tx.Q, spaceID, in.GitKeyID)
	if err != nil {
		return err
	}
	if !ok {
		return apperr.New(apperr.CodeInvalid, "Git SSH key %q does not exist or this space may not use it", in.GitKeyID)
	}
	return nil
}

// Create adds a repository. A repository with a URL and the auto policy is cloned on the node at once when it is
// connected.
func (svc *Service) Create(ctx context.Context, actor audit.Actor, spaceID string, in Input) (*Repository, error) {
	if err := svc.normalize(&in, nil); err != nil {
		return nil, err
	}
	id := ids.New(ids.Repository)
	queued := false
	err := svc.s.W().TxNamed(ctx, "repos.create", func(tx *store.Tx) error {
		if err := spaceCheck(ctx, tx.Q, spaceID); err != nil {
			return err
		}
		if n, err := tx.Q.CountSpaceRepositories(ctx, spaceID); err != nil {
			return err
		} else if n >= MaxPerSpace {
			return apperr.New(apperr.CodePrecondition, "a space has at most %d repositories", MaxPerSpace)
		}
		if _, err := tx.Q.GetSpaceRepositoryByName(ctx, db.GetSpaceRepositoryByNameParams{SpaceID: spaceID, Name: in.Name}); err == nil {
			return apperr.New(apperr.CodeAlreadyExists, "the space already has a repository named %q", in.Name)
		}
		if r, err := tx.Q.GetSpaceRepositoryByPath(ctx, db.GetSpaceRepositoryByPathParams{SpaceID: spaceID, Path: in.Path}); err == nil {
			return apperr.New(apperr.CodeAlreadyExists, "repository %q already uses the folder %s", r.Name, in.Path)
		}
		if err := checkGitKey(ctx, tx, spaceID, in, false); err != nil {
			return err
		}
		if err := tx.Q.InsertSpaceRepository(ctx, db.InsertSpaceRepositoryParams{ID: id, SpaceID: spaceID, Name: in.Name, Path: in.Path,
			Description: in.Description, Url: in.URL, DefaultBranch: in.DefaultBranch, Role: in.Role, ClonePolicy: in.ClonePolicy, KnownHosts: in.KnownHosts, GitKeyID: in.GitKeyID, CreatedAt: tx.NowMicros(),
			CreatedBy: actorRef(actor), UpdatedAt: tx.NowMicros(), UpdatedBy: actorRef(actor)}); err != nil {
			if store.IsConstraint(err) {
				return apperr.New(apperr.CodeAlreadyExists, "the space already has a repository with this name or folder")
			}
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "space_repository", id, "created", 1); err != nil {
			return err
		}
		if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "space_repository.created", Target: audit.Target{Type: "space_repository", ID: id},
			Details: auditDetails(spaceID, in)}); err != nil {
			return err
		}
		var err error
		queued, err = svc.queueAuto(ctx, tx, spaceID, actor, nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	if queued {
		svc.fleet.KickSpace(ctx, spaceID)
	}
	return svc.Get(ctx, spaceID, id)
}

// Update edits a repository at version (If-Match). Changing the URL never rewrites the folder's own remotes on the
// node: the configured URL is what SpaceMan clones and fetches from.
func (svc *Service) Update(ctx context.Context, actor audit.Actor, spaceID, id string, version int64, in Input) (*Repository, error) {
	queued := false
	err := svc.s.W().TxNamed(ctx, "repos.update", func(tx *store.Tx) error {
		cur, err := tx.Q.GetSpaceRepository(ctx, db.GetSpaceRepositoryParams{ID: id, SpaceID: spaceID})
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		} else if err != nil {
			return err
		}
		if err := spaceCheck(ctx, tx.Q, spaceID); err != nil {
			return err
		}
		if cur.Version != version {
			return apperr.New(apperr.CodePrecondition, "the repository changed (version %d, you sent %d); reload and retry", cur.Version, version)
		}
		if err := svc.normalize(&in, &cur); err != nil {
			return err
		}
		if len(cur.SshKeyEnc) > 0 && in.URL != "" && !proto.IsSSHURL(in.URL) {
			return apperr.New(apperr.CodeInvalid, "the repository has an SSH key, so its URL must be an SSH URL; remove the key first to use another URL")
		}
		if err := checkGitKey(ctx, tx, spaceID, in, len(cur.SshKeyEnc) > 0); err != nil {
			return err
		}
		n, err := tx.Q.UpdateSpaceRepository(ctx, db.UpdateSpaceRepositoryParams{Description: in.Description, Url: in.URL, DefaultBranch: in.DefaultBranch,
			Role: in.Role, ClonePolicy: in.ClonePolicy, KnownHosts: in.KnownHosts, GitKeyID: in.GitKeyID, Now: tx.NowMicros(), UpdatedBy: actorRef(actor), ID: id, SpaceID: spaceID, Version: version})
		if err != nil {
			return err
		}
		if n == 0 {
			return apperr.New(apperr.CodePrecondition, "the repository changed concurrently; reload and retry")
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "space_repository", id, "updated", version+1); err != nil {
			return err
		}
		if _, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "space_repository.updated", Target: audit.Target{Type: "space_repository", ID: id},
			Details: auditDetails(spaceID, in)}); err != nil {
			return err
		}
		queued, err = svc.queueAuto(ctx, tx, spaceID, actor, nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	if queued {
		svc.fleet.KickSpace(ctx, spaceID)
	}
	return svc.Get(ctx, spaceID, id)
}

// Delete removes a repository from the list (version optional: 0 skips the check). The folder on the node stays: SpaceMan
// never deletes source (SM-REPO-006). A repository a knowledge base uses cannot be removed.
func (svc *Service) Delete(ctx context.Context, actor audit.Actor, spaceID, id string, version int64) error {
	return svc.s.W().TxNamed(ctx, "repos.delete", func(tx *store.Tx) error {
		cur, err := tx.Q.GetSpaceRepository(ctx, db.GetSpaceRepositoryParams{ID: id, SpaceID: spaceID})
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound
		} else if err != nil {
			return err
		}
		if version != 0 && cur.Version != version {
			return apperr.New(apperr.CodePrecondition, "the repository changed (version %d, you sent %d); reload and retry", cur.Version, version)
		}
		kbs, err := tx.Q.ListKBsUsingRepository(ctx, db.ListKBsUsingRepositoryParams{SpaceID: spaceID, Repo: cur.Path})
		if err != nil {
			return err
		}
		if len(kbs) > 0 {
			return apperr.New(apperr.CodeConflict, "knowledge base %s uses this repository; point it at another repository first", strings.Join(kbs, ", "))
		}
		if _, err := tx.Q.DeleteSpaceRepository(ctx, db.DeleteSpaceRepositoryParams{ID: id, SpaceID: spaceID}); err != nil {
			return err
		}
		if err := svc.changes.Record(ctx, tx, spaceID, "space_repository", id, "deleted", cur.Version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "space_repository.deleted", Target: audit.Target{Type: "space_repository", ID: id},
			Details: map[string]any{"space_id": spaceID, "name": cur.Name, "path": cur.Path, "folder_kept": true}})
		return err
	})
}

// Get returns a repository with its status.
func (svc *Service) Get(ctx context.Context, spaceID, id string) (*Repository, error) {
	ov, err := svc.List(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	for _, r := range ov.Items {
		if r.ID == id || r.Name == id {
			return r, nil
		}
	}
	return nil, errNotFound
}

// List returns a space's repositories with the node's latest report.
func (svc *Service) List(ctx context.Context, spaceID string) (*Overview, error) {
	q := svc.s.R()
	sp, err := q.GetSpace(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, apperr.New(apperr.CodeNotFound, "space not found")
	} else if err != nil {
		return nil, err
	}
	rows, err := q.ListSpaceRepositories(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	ov := &Overview{Items: make([]*Repository, 0, len(rows)), Unlisted: []string{}}
	var rep proto.RepoStatusReport
	if r, err := q.GetRepositoryReport(ctx, spaceID); err == nil {
		if r.ReportedAt.Valid {
			t := time.UnixMicro(r.ReportedAt.Int64).UTC()
			ov.ReportedAt, ov.ReportNode = &t, r.NodeID
			ov.Stale = r.NodeID != sp.NodeID
			_ = json.Unmarshal([]byte(r.ReportJson), &rep)
			if !ov.Stale && rep.Unlisted != nil {
				ov.Unlisted = rep.Unlisted
			}
		}
		if r.RequestedAt.Valid {
			t := time.UnixMicro(r.RequestedAt.Int64).UTC()
			ov.RequestedAt = &t
			// Pending until the node answers the latest sync (a report names the command it answers).
			ov.SyncPending = r.CommandID != "" && rep.CommandID != r.CommandID
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	states := map[string]proto.RepoState{}
	for _, st := range rep.Repositories {
		states[st.ID] = st
	}
	owners := svc.worktreeOwners(ctx, spaceID)
	var nodeMinor int64 = -1 // the space's node's protocol minor (-1: unknown)
	if n, err := q.GetNode(ctx, sp.NodeID); err == nil && n.ProtocolMinor > 0 {
		nodeMinor = n.ProtocolMinor
	}
	for _, r := range rows {
		x := out(r)
		if r.GitKeyID != "" {
			x.GitKey = &GitKeyRef{ID: r.GitKeyID, Name: r.GitKeyID}
			if k, err := q.GetGitKey(ctx, r.GitKeyID); err == nil {
				x.GitKey.Name, x.GitKey.Fingerprint = k.Name, gitkeys.Fingerprint(k.PublicKey)
			}
			if ok, err := gitkeys.Allowed(ctx, q, spaceID, r.GitKeyID); err == nil && !ok {
				x.GitKey.Note = "This space may no longer use the key: the node clones with its own Git authentication until an administrator allows it again or you choose another key."
			} else if nodeMinor >= 0 && nodeMinor < proto.MinorRepoKeys {
				x.GitKey.Note = fmt.Sprintf("The space's node agent (protocol 1.%d) cannot use SpaceMan SSH keys: it clones with its own Git authentication until it is updated.", nodeMinor)
			}
		}
		if x.SSHKey != nil && nodeMinor >= 0 && nodeMinor < proto.MinorRepoKeys {
			x.SSHKey.Note = fmt.Sprintf("The space's node agent (protocol 1.%d) cannot use SpaceMan SSH keys: it clones with its own Git authentication until it is updated.", nodeMinor)
		}
		if kbs, err := q.ListKBsUsingRepository(ctx, db.ListKBsUsingRepositoryParams{SpaceID: spaceID, Repo: r.Path}); err == nil {
			x.KBs = kbs
		}
		if st, ok := states[r.ID]; ok && ov.ReportedAt != nil && st.Path == r.Path {
			x.Status = status(st, *ov.ReportedAt, ov.Stale, owners)
		}
		ov.Items = append(ov.Items, x)
	}
	return ov, nil
}

// worktreeOwners maps worktree folders to the latest run SpaceMan launched there (workspace ownership, SM-REPO-003).
func (svc *Service) worktreeOwners(ctx context.Context, spaceID string) map[string]string {
	owners := map[string]string{}
	runs, err := svc.fleet.ListRuns(ctx, spaceID, 500)
	if err != nil {
		return owners
	}
	// ListRuns is newest first: the first run seen for a folder is its latest owner.
	for _, r := range runs {
		if w := r.Launch.Worktree; w != nil && w.Branch != "" {
			p := "worktrees/" + strings.ReplaceAll(w.Branch, "/", "-")
			if _, ok := owners[p]; !ok {
				owners[p] = r.ID
			}
		}
	}
	return owners
}

func status(st proto.RepoState, at time.Time, stale bool, owners map[string]string) *Status {
	s := &Status{State: "missing", Stale: stale, ReportedAt: at, Remote: st.Remote, DefaultHead: st.DefaultHead, TrackingHead: st.Tracking,
		LastFetchAt: st.LastFetchAt, Branches: []proto.BranchCheck{}, Worktrees: []Worktree{}, Cloned: st.Cloned, Fetched: st.Fetched, Error: st.Error}
	if st.Present {
		s.State = "present"
	}
	if st.Error != "" && (!st.Present || st.Check == nil) {
		s.State = "error"
	}
	if c := st.Check; c != nil {
		s.Branch, s.Head, s.Dirty, s.Untracked, s.Stashes = c.Branch, c.Head, c.Dirty, c.Untracked, c.Stashes
		unpushed := map[string]int{}
		for _, b := range c.Branches {
			s.Unpushed += b.Unpushed
			unpushed[b.Name] = b.Unpushed
			if len(s.Branches) < maxBranches {
				s.Branches = append(s.Branches, b)
			}
		}
		s.Unpushed += c.Detached
		for _, w := range c.Worktrees {
			s.Worktrees = append(s.Worktrees, Worktree{Path: w.Path, Branch: w.Branch, Head: w.Head, Dirty: w.Dirty, Untracked: w.Untracked,
				Unpushed: unpushed[w.Branch], RunID: owners[w.Path], Error: w.Error})
		}
	}
	return s
}

// Sync asks the space's node to clone what is missing (auto and on_demand repositories), fetch the requested
// repositories (default every repository with a URL) and report the state of all of them. The node must be connected.
func (svc *Service) Sync(ctx context.Context, actor audit.Actor, spaceID string, in SyncInput) (*SyncResult, error) {
	if len(in.Repositories) > MaxPerSpace {
		return nil, apperr.New(apperr.CodeInvalid, "repositories: at most %d", MaxPerSpace)
	}
	fetch := in.Fetch == nil || *in.Fetch
	var res *SyncResult
	err := svc.s.W().TxNamed(ctx, "repos.sync", func(tx *store.Tx) error {
		if err := spaceCheck(ctx, tx.Q, spaceID); err != nil {
			return err
		}
		rows, err := tx.Q.ListSpaceRepositories(ctx, spaceID)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return apperr.New(apperr.CodePrecondition, "the space has no repositories")
		}
		want := map[string]bool{}
		for _, n := range in.Repositories {
			found := false
			for _, r := range rows {
				if r.ID == n || r.Name == n {
					want[r.ID], found = true, true
				}
			}
			if !found {
				return apperr.New(apperr.CodeNotFound, "repository %q not found in this space", n)
			}
		}
		p := payload(rows)
		for _, r := range rows {
			if r.ClonePolicy != proto.CloneManual {
				p.Clone = append(p.Clone, r.ID)
			}
			if fetch && r.Url != "" && (len(want) == 0 || want[r.ID]) {
				p.Fetch = append(p.Fetch, r.ID)
			}
		}
		id, err := svc.queue(ctx, tx, spaceID, actor, p, true)
		if err != nil {
			return err
		}
		res = &SyncResult{CommandID: id, RequestedAt: tx.Now.UTC(), Clone: names(rows, p.Clone), Fetch: names(rows, p.Fetch)}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "space_repository.sync_requested", Target: audit.Target{Type: "space", ID: spaceID},
			Details: map[string]any{"command_id": id, "clone": res.Clone, "fetch": res.Fetch}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.fleet.KickSpace(ctx, spaceID)
	return res, nil
}

func names(rows []db.SpaceRepository, idsIn []string) []string {
	out := []string{}
	for _, r := range rows {
		if slices.Contains(idsIn, r.ID) {
			out = append(out, r.Name)
		}
	}
	return out
}

func payload(rows []db.SpaceRepository) proto.ReposSyncPayload {
	p := proto.ReposSyncPayload{Repositories: make([]proto.RepoSource, 0, len(rows))}
	for _, r := range rows {
		p.Repositories = append(p.Repositories, Source(r))
	}
	return p
}

// Source is a configured repository as the node receives it.
func Source(r db.SpaceRepository) proto.RepoSource {
	return proto.RepoSource{ID: r.ID, Name: r.Name, Path: r.Path, URL: r.Url, DefaultBranch: r.DefaultBranch, ClonePolicy: r.ClonePolicy, KnownHosts: r.KnownHosts}
}

// queueAuto queues an automatic sync inside tx (clone the auto repositories, no fetch) when the space's node is connected
// and speaks the repository protocol; otherwise it does nothing (the node is asked when it connects). only limits the
// clones to these repository IDs (nil: every auto repository).
func (svc *Service) queueAuto(ctx context.Context, tx *store.Tx, spaceID string, actor audit.Actor, only []string) (bool, error) {
	rows, err := tx.Q.ListSpaceRepositories(ctx, spaceID)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	p := payload(rows)
	for _, r := range rows {
		if r.ClonePolicy == proto.CloneAuto && (only == nil || slices.Contains(only, r.ID)) {
			p.Clone = append(p.Clone, r.ID)
		}
	}
	_, err = svc.queue(ctx, tx, spaceID, actor, p, false)
	if err != nil {
		if c := apperr.CodeOf(err); c == apperr.CodeUnavailable || c == apperr.CodePrecondition {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// queue queues one space.repos.sync for the space's node inside tx and records the request.
func (svc *Service) queue(ctx context.Context, tx *store.Tx, spaceID string, actor audit.Actor, p proto.ReposSyncPayload, explicit bool) (string, error) {
	sp, err := tx.Q.GetSpace(ctx, spaceID)
	if err != nil {
		return "", err
	}
	if sp.DesiredNodeID.Valid {
		return "", apperr.New(apperr.CodePrecondition, "the space is relocating; its repositories are synced on the destination after the move")
	}
	n, err := tx.Q.GetNode(ctx, sp.NodeID)
	if err != nil {
		return "", err
	}
	if n.ProtocolMinor < proto.MinorRepositories {
		return "", apperr.New(apperr.CodePrecondition, "node %s speaks node protocol 1.%d; configured repositories need 1.%d (upgrade spaceman-agent)",
			n.Name, n.ProtocolMinor, proto.MinorRepositories)
	}
	key := "repos-sync:" + ids.New(ids.Operation)
	if !explicit {
		key = "repos-auto:" + ids.New(ids.Operation)
	}
	id, err := svc.fleet.QueueSpaceCommand(ctx, tx, spaceID, proto.CmdReposSync, key, p)
	if err != nil {
		return "", err
	}
	if err := tx.Q.SetRepositorySyncRequested(ctx, db.SetRepositorySyncRequestedParams{SpaceID: spaceID, CommandID: id,
		RequestedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, RequestedBy: actorRef(actor)}); err != nil {
		return "", err
	}
	return id, nil
}

// OnStatus stores a node's repository report (the hub's repos.status hook). Only the space's current node is heard, only
// configured repositories are kept, and an older report never replaces a newer one.
func (svc *Service) OnStatus(ctx context.Context, nodeID string, rep proto.RepoStatusReport) {
	if len(rep.Repositories) > MaxPerSpace {
		rep.Repositories = rep.Repositories[:MaxPerSpace]
	}
	if len(rep.Unlisted) > MaxPerSpace {
		rep.Unlisted = rep.Unlisted[:MaxPerSpace]
	}
	err := svc.s.W().TxNamed(ctx, "repos.status", func(tx *store.Tx) error {
		sp, err := tx.Q.GetSpace(ctx, rep.SpaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // the space is gone
		} else if err != nil {
			return err
		}
		if sp.NodeID != nodeID {
			return nil // not this node's space (any more)
		}
		rows, err := tx.Q.ListSpaceRepositories(ctx, rep.SpaceID)
		if err != nil {
			return err
		}
		known := map[string]bool{}
		for _, r := range rows {
			known[r.ID] = true
		}
		kept := rep.Repositories[:0]
		for _, st := range rep.Repositories {
			if known[st.ID] {
				if st.Check != nil && len(st.Check.Branches) > maxBranches {
					st.Check.Branches = st.Check.Branches[:maxBranches]
				}
				kept = append(kept, st)
			}
		}
		rep.Repositories = kept
		b, err := json.Marshal(rep)
		if err != nil {
			return err
		}
		// reported_at is central's receipt time (compared with sync requests); checked_at, the node's, orders the node's
		// own reports, so a re-sent older report never replaces a newer one.
		n, err := tx.Q.SetRepositoryReport(ctx, db.SetRepositoryReportParams{SpaceID: rep.SpaceID, NodeID: nodeID, ReportJson: string(b),
			ReportedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, CheckedAt: sql.NullInt64{Int64: rep.CheckedAt.UnixMicro(), Valid: true}})
		if err != nil || n == 0 {
			return err
		}
		return svc.changes.Record(ctx, tx, rep.SpaceID, "space_repository_status", rep.SpaceID, "updated", 0)
	})
	if err != nil {
		svc.o.Log.Warn("store repository report", slog.String(obs.FieldSpaceID, rep.SpaceID), obs.Err(err))
	}
}

// OnNodeConnected queues an automatic sync for each space of the node with configured repositories (the hub's hook,
// after the node's leases were granted): missing auto repositories are cloned, and every repository is reported.
func (svc *Service) OnNodeConnected(ctx context.Context, nodeID string) {
	spaces, err := svc.s.R().ListSpacesByNode(ctx, nodeID)
	if err != nil {
		return
	}
	for _, sp := range spaces {
		if sp.State == "archived" || sp.DesiredNodeID.Valid {
			continue
		}
		svc.autoSync(ctx, sp.ID)
	}
}

// OnRunEnded refreshes the report after a run of a space with configured repositories ended, so the changes and
// unpushed commits it left show (the fleet's run-ended hook). Reports follow events (repository changes, node
// connections, ended runs, sync requests); nothing polls idle spaces (SM-ARCH-004).
func (svc *Service) OnRunEnded(runID string) {
	ctx := svc.base
	r, err := svc.fleet.GetRun(ctx, runID)
	if err != nil {
		return
	}
	svc.autoSync(ctx, r.SpaceID)
}

// Bind sets the context hooks without a caller context (run-ended) use.
func (svc *Service) Bind(ctx context.Context) { svc.base = context.WithoutCancel(ctx) }

// autoSync queues an automatic sync unless one was queued moments ago and not answered yet (runs ending together).
func (svc *Service) autoSync(ctx context.Context, spaceID string) {
	queued := false
	err := svc.s.W().TxNamed(ctx, "repos.auto_sync", func(tx *store.Tx) error {
		if r, err := tx.Q.GetRepositoryReport(ctx, spaceID); err == nil && r.RequestedAt.Valid &&
			(!r.ReportedAt.Valid || r.ReportedAt.Int64 < r.RequestedAt.Int64) && tx.Now.Sub(time.UnixMicro(r.RequestedAt.Int64)) < autoDebounce {
			return nil
		}
		var err error
		queued, err = svc.queueAuto(ctx, tx, spaceID, system, nil)
		return err
	})
	if err != nil {
		svc.o.Log.Warn("queue repository sync", slog.String(obs.FieldSpaceID, spaceID), obs.Err(err))
	}
	if queued {
		svc.fleet.KickSpace(ctx, spaceID)
	}
}

// ForKB resolves a knowledge base's repository folder inside its transaction (kb.Service.ResolveRepo): a configured
// repository at that folder is used as it is; otherwise the folder is registered as a manual kb repository, so every
// knowledge base's repository is in the list. Knowledge bases name the folder (not the repository's name), so what
// Terraform configures is what is stored.
func (svc *Service) ForKB(ctx context.Context, tx *store.Tx, actor audit.Actor, spaceID, repo, baseBranch string) (string, error) {
	if _, err := tx.Q.GetSpaceRepositoryByPath(ctx, db.GetSpaceRepositoryByPathParams{SpaceID: spaceID, Path: repo}); err == nil {
		return repo, nil
	}
	if !ValidPath(repo) {
		v := &apperr.Validation{}
		v.Add("repo", "must be a configured repository or a relative folder inside the space (not instances/, worktrees/, work/ or dreams/)")
		return "", v.Err()
	}
	id := ids.New(ids.Repository)
	suffix := id[len(id)-6:]
	name := strings.ToLower(strings.ReplaceAll(repo, "/", "-"))
	if len(name) > 63 {
		name = name[:63]
	}
	if !reName.MatchString(name) {
		name = "kb-" + suffix
	}
	if _, err := tx.Q.GetSpaceRepositoryByName(ctx, db.GetSpaceRepositoryByNameParams{SpaceID: spaceID, Name: name}); err == nil {
		name = strings.TrimRight(name[:min(len(name), 56)], "-._") + "-" + suffix
	}
	if n, err := tx.Q.CountSpaceRepositories(ctx, spaceID); err != nil {
		return "", err
	} else if n >= MaxPerSpace {
		return "", apperr.New(apperr.CodePrecondition, "a space has at most %d repositories", MaxPerSpace)
	}
	if !ValidBranch(baseBranch) {
		baseBranch = "main"
	}
	if err := tx.Q.InsertSpaceRepository(ctx, db.InsertSpaceRepositoryParams{ID: id, SpaceID: spaceID, Name: name, Path: repo,
		Description: "Knowledge base repository", Url: "", DefaultBranch: baseBranch, Role: RoleKB, ClonePolicy: proto.CloneManual, CreatedAt: tx.NowMicros(),
		CreatedBy: actorRef(actor), UpdatedAt: tx.NowMicros(), UpdatedBy: actorRef(actor)}); err != nil {
		return "", err
	}
	if err := svc.changes.Record(ctx, tx, spaceID, "space_repository", id, "created", 1); err != nil {
		return "", err
	}
	_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "space_repository.created", Target: audit.Target{Type: "space_repository", ID: id},
		Details: map[string]any{"space_id": spaceID, "name": name, "path": repo, "role": RoleKB, "clone_policy": proto.CloneManual, "for": "knowledge_base"}})
	return repo, err
}
