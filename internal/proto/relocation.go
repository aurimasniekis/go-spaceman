package proto

import (
	"regexp"
	"strings"
	"time"
)

// Space relocation (protocol minor 3, ADR-0037). Central drives a relocation with durable commands to the source and
// the destination (space.relocate.*). The node answers each with a relocation.report; exported state travels to
// central as relocation.upload chunks, and the destination fetches what it needs with relocation.fetch.

// MinorRelocation is the first minor version that carries space relocation.
const MinorRelocation = 3

// Relocation transfer bounds: one chunk, and everything one relocation exports.
const (
	RelocationChunkMax  = 1 << 20
	RelocationExportMax = 4 << 30
)

var reBlob = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidBlobName reports whether name is a relocation file name (a single path element, no traversal).
func ValidBlobName(name string) bool {
	return reBlob.MatchString(name) && !strings.Contains(name, "..")
}

// RelocateCheckPayload asks the source to inspect the space folder (space.relocate.check). With Flush the node first
// checkpoints and syncs the space's role memory, ships unsent recordings and uploads finished transcripts, waiting at
// most SyncWait seconds for central's acknowledgements. Workdir is the space's folder name: the source's lease is
// revoked while the space relocates, so the command carries it.
type RelocateCheckPayload struct {
	RelocationID string `json:"relocation_id"`
	Workdir      string `json:"workdir"`
	Flush        bool   `json:"flush"`
	SyncWait     int    `json:"sync_wait_seconds,omitempty"`
}

// RelocateExportPayload asks the source to export the listed repositories' local state (every ref as a Git bundle,
// plus snapshot commits of changed and untracked work trees and every stash) and, with Loose, the files outside any
// repository, and to upload them to central (space.relocate.export). Nothing in the source folder is reset, committed
// to a branch, pushed or deleted.
type RelocateExportPayload struct {
	RelocationID string   `json:"relocation_id"`
	Workdir      string   `json:"workdir"`
	Repos        []string `json:"repos"`
	Loose        bool     `json:"loose"`
	MaxBytes     int64    `json:"max_bytes"`
}

// RelocateBlob is one file central stores for a relocation.
type RelocateBlob struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// RelocateMemory is one role-memory collection the destination restores from central's acknowledged history.
type RelocateMemory struct {
	Role string `json:"role"`
	Head string `json:"head"`
	Blob string `json:"blob"`
}

// GitRemote is a repository remote. URLs never carry credentials: the node removes user information before reporting.
type GitRemote struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Redacted bool   `json:"credentials_removed,omitempty" doc:"The URL held credentials; they were removed and are not carried"`
}

// BranchCheck is one local branch of a repository.
type BranchCheck struct {
	Name       string `json:"name"`
	Commit     string `json:"commit"`
	Remote     string `json:"remote,omitempty" doc:"The branch's configured remote"`
	Merge      string `json:"merge,omitempty" doc:"The branch's configured upstream branch, e.g. refs/heads/main"`
	Unpushed   int    `json:"unpushed" doc:"Commits not on the upstream, or on no remote-tracking ref when there is no upstream"`
	NoUpstream bool   `json:"no_upstream,omitempty"`
}

// WorktreeCheck is a linked worktree of a repository (SM-REPO-002).
type WorktreeCheck struct {
	Path      string `json:"path" doc:"Relative to the space folder"`
	Branch    string `json:"branch,omitempty"`
	Head      string `json:"head"`
	Dirty     int    `json:"dirty"`
	Untracked int    `json:"untracked"`
	Error     string `json:"error,omitempty"`
}

// RepoCheck is one repository in the space folder as the source sees it.
type RepoCheck struct {
	Path      string          `json:"path" doc:"Relative to the space folder"`
	Remotes   []GitRemote     `json:"remotes"`
	Branch    string          `json:"branch,omitempty" doc:"Checked-out branch; empty when HEAD is detached"`
	Head      string          `json:"head,omitempty"`
	Branches  []BranchCheck   `json:"branches"`
	Dirty     int             `json:"dirty" doc:"Changed tracked files"`
	Untracked int             `json:"untracked"`
	Paths     []string        `json:"paths,omitempty" doc:"Up to 20 changed or untracked paths"`
	Stashes   int             `json:"stashes"`
	Detached  int             `json:"detached_unpushed" doc:"Commits reachable only from a detached HEAD"`
	Worktrees []WorktreeCheck `json:"worktrees"`
	Error     string          `json:"error,omitempty"`
}

// LooseCheck counts files in the space folder that belong to no repository (outside instances/, which is harness state).
type LooseCheck struct {
	Files int      `json:"files"`
	Bytes int64    `json:"bytes"`
	Paths []string `json:"paths,omitempty" doc:"Up to 20 of them"`
}

// InstanceCheck is one agent instance's native harness folder (configuration, sessions, transcripts). It is node-local
// and not carried by a relocation: the next run on the destination starts a fresh native session (SM-SPACE-008).
type InstanceCheck struct {
	InstanceID string `json:"instance_id"`
	Files      int    `json:"files"`
	Bytes      int64  `json:"bytes"`
}

// SpaceCheck is the source's inspection of a space before relocation.
type SpaceCheck struct {
	CheckedAt    time.Time                `json:"checked_at"`
	FolderExists bool                     `json:"folder_exists"`
	Repositories []RepoCheck              `json:"repositories"`
	Loose        LooseCheck               `json:"loose"`
	Memory       []MemoryCollectionStatus `json:"memory"`
	Recordings   []string                 `json:"recordings_pending" doc:"Runs whose recording central has not fully stored"`
	Transcripts  []string                 `json:"transcripts_pending" doc:"Runs whose native transcript upload is unfinished"`
	ActiveRuns   []string                 `json:"active_runs" doc:"Runs the node still supervises in this space"`
	Instances    []InstanceCheck          `json:"instances"`
	Flushed      bool                     `json:"flushed" doc:"Memory, recordings and transcripts were flushed and acknowledged in time"`
	Errors       []string                 `json:"errors,omitempty"`
}

// ExportedWorktree is a worktree's place in a relocation.
type ExportedWorktree struct {
	Path     string `json:"path"`
	Branch   string `json:"branch,omitempty"`
	Head     string `json:"head"`
	Snapshot string `json:"snapshot,omitempty" doc:"Commit holding the worktree's changed and untracked files"`
}

// ExportedStash is one stash entry (newest first, as git lists them).
type ExportedStash struct {
	Commit  string `json:"commit"`
	Message string `json:"message"`
}

// ExportedRepo is one repository in an export: Blob is a Git bundle of every ref plus the snapshot and stash commits.
type ExportedRepo struct {
	Path      string             `json:"path"`
	Blob      string             `json:"blob"`
	Branch    string             `json:"branch,omitempty"`
	Head      string             `json:"head,omitempty"`
	Snapshot  string             `json:"snapshot,omitempty" doc:"Commit holding the work tree's changed and untracked files"`
	Remotes   []GitRemote        `json:"remotes"`
	Branches  []BranchCheck      `json:"branches"`
	Worktrees []ExportedWorktree `json:"worktrees"`
	Stashes   []ExportedStash    `json:"stashes"`
}

// RelocateExport is the manifest of exported state.
type RelocateExport struct {
	Repos      []ExportedRepo `json:"repos"`
	Loose      string         `json:"loose,omitempty" doc:"Blob holding the loose files (tar.gz)"`
	LooseFiles int            `json:"loose_files,omitempty"`
	Blobs      []RelocateBlob `json:"blobs"`
}

// RelocateRepo is a repository the destination clones from its remote because its state is pushed.
type RelocateRepo struct {
	Path      string             `json:"path"`
	Remotes   []GitRemote        `json:"remotes"`
	Branch    string             `json:"branch,omitempty"`
	Head      string             `json:"head,omitempty"`
	Branches  []BranchCheck      `json:"branches"`
	Worktrees []ExportedWorktree `json:"worktrees"`
}

// RelocatePreparePayload asks the destination to materialize the space folder (space.relocate.prepare): clone the
// clean repositories, restore exported ones from their bundles, restore role memory from central's history and unpack
// loose files. An existing folder of the same space is moved aside, never deleted. Blobs are fetched from central with
// relocation.fetch.
type RelocatePreparePayload struct {
	RelocationID string           `json:"relocation_id"`
	Workdir      string           `json:"workdir"`
	Repos        []RelocateRepo   `json:"repos"`
	Export       *RelocateExport  `json:"export,omitempty"`
	Memory       []RelocateMemory `json:"memory"`
	Blobs        []RelocateBlob   `json:"blobs"`
}

// Release actions.
const (
	ReleaseRetain  = "retain"  // source: move the folder and memory aside under retained/ (kept until deleted)
	ReleaseDelete  = "delete"  // source: delete the folder and memory, or the retained copy
	ReleaseDiscard = "discard" // destination: drop what a cancelled relocation prepared
)

// RelocateReleasePayload is the body of space.relocate.release.
type RelocateReleasePayload struct {
	RelocationID string `json:"relocation_id"`
	Workdir      string `json:"workdir"`
	Action       string `json:"action"`
}

// PreparedRepo is one materialized repository.
type PreparedRepo struct {
	Path      string `json:"path"`
	Head      string `json:"head,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Exported  bool   `json:"exported" doc:"Restored from the exported bundle rather than cloned"`
	Worktrees int    `json:"worktrees"`
	Dirty     int    `json:"dirty" doc:"Changed tracked files after restoring"`
	Untracked int    `json:"untracked"`
	Stashes   int    `json:"stashes"`
}

// PreparedMemory is one restored collection.
type PreparedMemory struct {
	Role string `json:"role"`
	Head string `json:"head"`
}

// PrepareResult is what the destination materialized and verified.
type PrepareResult struct {
	Repos      []PreparedRepo   `json:"repos"`
	Memory     []PreparedMemory `json:"memory"`
	LooseFiles int              `json:"loose_files"`
	MovedAside []string         `json:"moved_aside,omitempty" doc:"Existing folders of this space moved aside on the destination"`
}

// ReleaseResult reports a release.
type ReleaseResult struct {
	Action     string `json:"action"`
	RetainedAs string `json:"retained_as,omitempty" doc:"Folder name under the node's retained/ directory"`
}

// RelocationReport is the outcome of a space.relocate.* command (TypeRelocationReport). Reports are state: the node
// re-sends recent ones after reconnecting, and central applies only the one for the step it is waiting for.
type RelocationReport struct {
	RelocationID string          `json:"relocation_id"`
	CommandID    string          `json:"command_id"`
	Kind         string          `json:"kind"`
	OK           bool            `json:"ok"`
	Error        string          `json:"error,omitempty"`
	Check        *SpaceCheck     `json:"check,omitempty"`
	Export       *RelocateExport `json:"export,omitempty"`
	Prepare      *PrepareResult  `json:"prepare,omitempty"`
	Release      *ReleaseResult  `json:"release,omitempty"`
}

// RelocationUpload is part of an exported file (source → central). Offset is where Data starts; the last part has
// Final set with the whole file's size and SHA-256.
type RelocationUpload struct {
	RelocationID string `json:"relocation_id"`
	Name         string `json:"name"`
	Offset       int64  `json:"offset"`
	Data         []byte `json:"data"`
	Final        bool   `json:"final"`
	Size         int64  `json:"size,omitempty"`
	SHA256       string `json:"sha256,omitempty"`
}

// RelocationUploadAck tells the source how much of a file central stored (Next) and whether it is complete.
type RelocationUploadAck struct {
	RelocationID string `json:"relocation_id"`
	Name         string `json:"name"`
	Next         int64  `json:"next"`
	Done         bool   `json:"done"`
	Error        string `json:"error,omitempty"`
}

// RelocationFetch asks central for part of a relocation file (destination → central).
type RelocationFetch struct {
	RelocationID string `json:"relocation_id"`
	Name         string `json:"name"`
	Offset       int64  `json:"offset"`
}

// RelocationData answers a RelocationFetch with at most RelocationChunkMax bytes.
type RelocationData struct {
	RelocationID string `json:"relocation_id"`
	Name         string `json:"name"`
	Offset       int64  `json:"offset"`
	Data         []byte `json:"data"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
	Final        bool   `json:"final"`
	Error        string `json:"error,omitempty"`
}
