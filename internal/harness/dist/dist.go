// Package dist resolves, downloads and caches Claude Code releases for the node agent (node-managed Claude Code,
// design 2026-11-12 §3.1-3.2).
//
// A version is an exact X.Y.Z, or a channel ("latest", "stable") whose pointer is fetched from {base}/{channel} and
// memoised for the refresh interval, in memory and on disk. Every URL and path is built only from a validated version
// and the node's platform (GOOS/GOARCH, plus a musl check of /bin/sh's ELF interpreter on Linux), never from response text.
//
// An install fetches {base}/{v}/manifest.json, requires its version to equal v, then streams
// {base}/{v}/{platform}/claude through SHA-256 with a hard size cap (the manifest size) into a private temporary
// directory. Only after the size and checksum match is the binary synced and made executable (0555), the manifest and
// verified.json written beside it (0400), and the directory made read-only (0500) and renamed to <dir>/<v>. A version
// directory is never modified after that rename, so nothing partial is ever executable; a <v> that fails the cheap
// check (opened as a handle, never through a link) is removed and installed again. The verification is
// checksum-only: the release manifest's signature is not checked yet (design §2).
//
// The read-only modes only stop accidental changes. Runs share the node's uid, so a run could still chmod and replace a
// cached binary; whether launches re-hash it is decided with the launch wiring (CC-3).
//
// The package is self-contained: launch wiring (absolute binary path, DISABLE_AUTOUPDATER) is not part of it.
package dist

import (
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// DefaultBaseURL is the public Claude Code release host (the same base is compiled into Claude Code itself).
const DefaultBaseURL = "https://downloads.claude.ai/claude-code-releases"

// Channels.
const (
	ChannelLatest = "latest"
	ChannelStable = "stable"
)

// BinaryName is the Claude Code binary inside a release platform directory and a cached version directory.
const BinaryName = "claude"

// versionRe is a strict X.Y.Z: no leading zeros, no prefix, no pre-release or build suffix.
var versionRe = regexp.MustCompile(`^(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})$`)

// Errors.
var (
	ErrInvalidVersion = errors.New("claude code: invalid version")
	ErrNotFound       = errors.New("claude code: version not found on the release server")
	ErrVerify         = errors.New("claude code: download failed verification")
	ErrNoVersion      = errors.New("claude code: no version is available")
)

// ValidVersion reports whether v is a strict X.Y.Z version.
func ValidVersion(v string) bool { return versionRe.MatchString(v) }

// ValidChannel reports whether c is a known channel.
func ValidChannel(c string) bool { return c == ChannelLatest || c == ChannelStable }

// ValidTarget reports whether s is a channel or an exact version: the values a node default or a profile pin accept.
func ValidTarget(s string) bool { return ValidChannel(s) || ValidVersion(s) }

// CompareVersions orders two valid versions numerically (-1, 0, 1).
func CompareVersions(a, b string) int {
	pa, pb := parts(a), parts(b)
	for i := range 3 {
		switch {
		case pa[i] < pb[i]:
			return -1
		case pa[i] > pb[i]:
			return 1
		}
	}
	return 0
}

func parts(v string) [3]int {
	var out [3]int
	for i, s := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(s)
	}
	return out
}

// Platform returns the release platform key of this node ("linux-x64", "linux-arm64-musl", "darwin-arm64", …).
func Platform() (string, error) { return platformFor(runtime.GOOS, runtime.GOARCH, "/") }

// platformFor maps GOOS/GOARCH to a release platform. root is where bin/sh is inspected for musl (tests use a
// temporary directory).
func platformFor(goos, goarch, root string) (string, error) {
	var arch string
	switch goarch {
	case "amd64":
		arch = "x64"
	case "arm64":
		arch = "arm64"
	default:
		return "", fmt.Errorf("claude code: unsupported architecture %s", goarch)
	}
	switch goos {
	case "darwin":
		return "darwin-" + arch, nil
	case "linux":
		if isMusl(root) {
			return "linux-" + arch + "-musl", nil
		}
		return "linux-" + arch, nil
	}
	return "", fmt.Errorf("claude code: unsupported operating system %s", goos)
}

// isMusl reports whether the system's C library is musl: the ELF interpreter (PT_INTERP) of root/bin/sh is musl's
// loader (ld-musl-<arch>.so.1). A loader file alone proves nothing: Debian's musl package installs one beside glibc.
// A /bin/sh that is not a dynamic ELF (a script, a static build, missing) counts as glibc.
func isMusl(root string) bool {
	f, err := elf.Open(filepath.Join(root, "bin", "sh"))
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(p.Open(), 4096))
		if err != nil {
			return false
		}
		return strings.HasPrefix(path.Base(strings.TrimRight(string(b), "\x00")), "ld-musl-")
	}
	return false
}
