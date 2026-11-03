package memory

import (
	"fmt"
	"regexp"
	"strings"
)

// unsafeConfig matches repository configuration that makes git run a program or reach a transport the daemon did not
// choose: filter, merge and diff drivers, pagers, editors, SSH and proxy commands, credential helpers, includes,
// URL rewriting and protocol allowances. Agents can write a repository's .git/config, and the node daemon runs git
// in those repositories (memory checkouts, KB worktrees, writer worktrees, relocation), so such keys would let an
// agent run code as the daemon. Hooks and fsmonitor are overridden on the command line instead (security review
// SR-M04).
var unsafeConfig = regexp.MustCompile(`^(filter\..+\.(clean|smudge|process)|merge\..+\.driver|diff\..+\.(command|textconv)|diff\.external|` +
	`core\.(sshcommand|gitproxy|askpass|pager|editor|alternaterefscommand)|sequence\.editor|include\.path|includeif\..+\.path|` +
	`credential\..*|uploadpack\.packobjectshook|remote\..+\.(uploadpack|receivepack|vcs)|gpg\..*program|protocol\..*allow|` +
	`url\..+\.(insteadof|pushinsteadof)|ssh\.variant)$`)

// UnsafeConfig checks `git config --list --name-only [--show-scope]` output. With scopes, only the repository's own
// (local and worktree) entries are checked: command-line options are SpaceMan's, and the node's global and system
// configuration belong to its operator.
func UnsafeConfig(list string, scoped bool) error {
	for line := range strings.SplitSeq(list, "\n") {
		name := strings.TrimSpace(line)
		if scoped {
			scope, rest, ok := strings.Cut(name, "\t")
			if !ok || (scope != "local" && scope != "worktree") {
				continue
			}
			name = rest
		}
		if name != "" && unsafeConfig.MatchString(strings.ToLower(name)) {
			return fmt.Errorf("the repository's git configuration sets %s, which would run a program for SpaceMan; remove it from .git/config", name)
		}
	}
	return nil
}

// Guarded reports whether a git subcommand works inside an existing repository and so needs its configuration
// checked first.
func Guarded(sub string) bool {
	switch sub {
	case "init", "clone", "config", "--version", "version":
		return false
	}
	return true
}
