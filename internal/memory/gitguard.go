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
	`credential\..*|uploadpack\.packobjectshook|gc\..*hook|remote\..+\.(uploadpack|receivepack|vcs)|gpg\..*program|protocol\..*allow|` +
	`url\..+\.(insteadof|pushinsteadof)|ssh\.variant|core\.worktree|` +
	`hook\..+|trailer\..+\.(cmd|command)|interactive\.difffilter|(difftool|mergetool)\..+\.(cmd|path)|` +
	`(browser|man)\..+\.(cmd|path)|web\.browser|gpg\.ssh\.defaultkeycommand)$`)

// Two keys are unsafe by value only: `git submodule init` writes submodule.<n>.update = rebase|merge|checkout|none,
// which is legitimate and only a value starting with "!" is a command; pager.<cmd> is a boolean unless it names a
// pager program.
var (
	reSubmoduleUpdate = regexp.MustCompile(`^submodule\..+\.update$`)
	rePagerKey        = regexp.MustCompile(`^pager\..+$`)
	// reBool is what git reads as a boolean: true/yes/on, false/no/off, "" or an integer (git_parse_maybe_bool).
	reBool = regexp.MustCompile(`^(true|false|yes|no|on|off|[+-]?[0-9]+[kmg]?)?$`)
)

// ConfigListArgs is the git command whose output UnsafeConfig checks.
var ConfigListArgs = []string{"config", "--list", "--show-scope", "-z"}

// core.worktree is in the list above for another reason: it points git's work tree at any folder, so `reset --hard`,
// `checkout` or `merge` would write there instead of in the checkout. core.bare = true does the same from the other
// side (it hides the work tree), but a bare repository legitimately sets it and `git init` writes core.bare = false
// into every checkout, so it is judged by what git reports for a checkout (Git.CheckoutIdentity).

// UnsafeConfig checks `git config --list --show-scope -z` output (ConfigListArgs). Every NUL-terminated field is the
// scope (when scoped) or an entry: a name, and a newline and the value unless the key has none. With scopes, only
// the repository's own (local and worktree) entries are checked: command-line options are SpaceMan's, and the node's
// global and system configuration belong to its operator.
func UnsafeConfig(list string, scoped bool) error {
	fields := strings.Split(list, "\x00")
	step := 1
	if scoped {
		step = 2
	}
	for i := 0; i+step-1 < len(fields); i += step {
		entry := fields[i+step-1]
		if scoped && fields[i] != "local" && fields[i] != "worktree" {
			continue
		}
		name, value, _ := strings.Cut(entry, "\n")
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.ToLower(strings.TrimSpace(value))
		switch {
		case name == "":
			continue
		case unsafeConfig.MatchString(name),
			reSubmoduleUpdate.MatchString(name) && strings.HasPrefix(value, "!"),
			rePagerKey.MatchString(name) && !reBool.MatchString(value):
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
