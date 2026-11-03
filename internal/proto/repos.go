package proto

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	// reHelper is Git's remote-helper syntax (<transport>::<address>), which runs a program (ext::, fd:: and others).
	reHelper = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*::`)
	// reSCP is Git's scp-like SSH syntax: [user@]host:path, with no slash before the first colon.
	reSCP = regexp.MustCompile(`^(?:[A-Za-z0-9._~-]+@)?(?:[A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\]):[^:].*$`)
)

// ValidateRepoURL checks a repository's clone URL (ADR-0044). Accepted are https:// and ssh:// URLs and scp-like SSH
// addresses (git@host:org/repo.git); with local set (development mode, tests) also http://, git://, file:// URLs and
// absolute paths. Refused always: credentials in the URL (the node's own Git authentication is used, SM-REPO-005),
// Git's remote-helper syntax such as ext:: (it runs programs), option-like values and whitespace or control characters.
func ValidateRepoURL(raw string, local bool) error {
	switch {
	case raw == "":
		return errors.New("is required")
	case len(raw) > 2000:
		return errors.New("is longer than 2000 characters")
	case strings.IndexFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0:
		return errors.New("must not contain whitespace or control characters")
	case strings.HasPrefix(raw, "-"):
		return errors.New("must not start with -")
	case reHelper.MatchString(raw):
		return errors.New("must not use a Git remote helper (transport::address runs a program)")
	}
	if strings.HasPrefix(raw, "/") {
		if !local {
			return errors.New("local paths are only allowed in development mode")
		}
		return nil
	}
	if i := strings.Index(raw, "://"); i > 0 {
		u, err := url.Parse(raw)
		if err != nil {
			return errors.New("is not a valid URL")
		}
		scheme := strings.ToLower(u.Scheme)
		switch scheme {
		case "https", "ssh", "git+ssh", "ssh+git":
		case "http", "git", "file":
			if !local {
				return errors.New("must be an https:// or ssh:// URL (" + scheme + ":// is only allowed in development mode)")
			}
		default:
			return errors.New("uses an unsupported scheme " + scheme + "://")
		}
		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword || scheme == "https" || scheme == "http" {
				return errors.New("must not contain credentials; the node's own Git authentication is used")
			}
		}
		if scheme != "file" && u.Host == "" {
			return errors.New("has no host")
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return errors.New("must not have a query or fragment")
		}
		return nil
	}
	if !strings.Contains(raw, "/") || strings.Index(raw, ":") < strings.Index(raw, "/") {
		if reSCP.MatchString(raw) {
			return nil
		}
	}
	return errors.New("must be an https:// or ssh:// URL or an scp-like address such as git@host:org/repo.git")
}

// RedactRepoURL removes user information from a URL for display (a configured URL never has a password; a remote an
// agent configured in a repository's own Git configuration might).
func RedactRepoURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.User != nil && u.Scheme != "" {
		u.User = nil
		return u.String()
	}
	return raw
}

// MinorRepositories is the first minor version that carries configured space repositories (ADR-0044): the
// space.repos.sync command, repository sources on launches and the node's repository status reports.
const MinorRepositories = 4

// Repository clone policies (ADR-0044).
const (
	CloneAuto     = "auto"      // cloned while the node holds the space's lease
	CloneOnDemand = "on_demand" // cloned when a run needs it, or on a sync request
	CloneManual   = "manual"    // never cloned by SpaceMan; only inspected
)

// RepoSource is a configured repository of a space as the node materializes it. URL never carries credentials: the
// node's own Git authentication is used and never replaced (SM-REPO-005). Path is relative to the space folder.
type RepoSource struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Path          string `json:"path"`
	URL           string `json:"url,omitempty"`
	DefaultBranch string `json:"default_branch"`
	ClonePolicy   string `json:"clone_policy"`
}

// ReposSyncPayload is the body of space.repos.sync: the space's configured repositories; the node clones those in
// Clone that are missing, fetches those in Fetch from their configured URL, and reports the state of all of them.
// Nothing in a repository is reset, merged, pushed or deleted (SM-REPO-006).
type ReposSyncPayload struct {
	Repositories []RepoSource `json:"repositories"`
	Clone        []string     `json:"clone,omitempty" doc:"IDs of repositories to clone when their folder is missing"`
	Fetch        []string     `json:"fetch,omitempty" doc:"IDs of repositories to fetch from their URL"`
}

// RepoRemoteState is what the node learned from a repository's configured remote.
type RepoRemoteState struct {
	URL       string `json:"url,omitempty" doc:"Without user information"`
	Reachable bool   `json:"reachable"`
	Head      string `json:"head,omitempty" doc:"The default branch's commit on the remote"`
	Error     string `json:"error,omitempty"`
}

// RepoState is one configured repository on the node.
type RepoState struct {
	ID      string           `json:"id"`
	Path    string           `json:"path"`
	Present bool             `json:"present" doc:"The folder holds a Git repository"`
	Cloned  bool             `json:"cloned,omitempty" doc:"This sync cloned it"`
	Fetched bool             `json:"fetched,omitempty" doc:"This sync fetched it"`
	Remote  *RepoRemoteState `json:"remote,omitempty" doc:"Absent when the repository has no URL"`
	// DefaultHead is the local default branch's commit; Tracking its remote-tracking branch (refs/remotes/origin/<default>).
	DefaultHead string     `json:"default_head,omitempty"`
	Tracking    string     `json:"tracking_head,omitempty"`
	LastFetchAt *time.Time `json:"last_fetch_at,omitempty" doc:"When the repository was last fetched or cloned (as the node's files show it)"`
	// Check is the repository's inspection: remotes, branches with unpushed commits, changes, stashes and worktrees.
	Check *RepoCheck `json:"check,omitempty"`
	Error string     `json:"error,omitempty"`
}

// RepoStatusReport is the node's answer to a space.repos.sync command (minor ≥ 4).
type RepoStatusReport struct {
	CommandID    string      `json:"command_id"`
	SpaceID      string      `json:"space_id"`
	CheckedAt    time.Time   `json:"checked_at"`
	Repositories []RepoState `json:"repositories"`
	// Unlisted are repositories found in the space folder that are not configured (outside worktrees/ and instances/).
	Unlisted []string `json:"unlisted,omitempty"`
}
