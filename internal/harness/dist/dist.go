// Package dist resolves, downloads and caches Claude Code releases for the node agent (node-managed Claude Code,
// design 2026-11-12 §3.1-3.2).
//
// A version is an exact X.Y.Z, or a channel ("latest", "stable") whose pointer is fetched from {base}/{channel} and
// memoised for the refresh interval, in memory and on disk. Every URL and path is built only from a validated version
// and the node's platform (GOOS/GOARCH, plus a musl check on Linux), never from response text.
//
// An install fetches {base}/{v}/manifest.json, requires its version to equal v, then streams
// {base}/{v}/{platform}/claude through SHA-256 with a hard size cap (the manifest size) into a private temporary
// directory. Only after the size and checksum match is the binary synced, made executable and the directory renamed to
// <dir>/<v>. A version directory is never modified after that rename, so nothing partial is ever executable. The
// verification is checksum-only: the release manifest's signature is not checked yet (design §2).
//
// The package is self-contained: launch wiring (absolute binary path, DISABLE_AUTOUPDATER) is not part of it.
package dist

import (
	"errors"
	"fmt"
	"os"
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

// platformFor maps GOOS/GOARCH to a release platform. root is where the musl loader is looked for (tests use a
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

// isMusl reports whether the system's C library is musl: musl installs its dynamic loader as
// /lib/ld-musl-<arch>.so.1, glibc never does.
func isMusl(root string) bool {
	m, _ := filepath.Glob(filepath.Join(root, "lib", "ld-musl-*.so.1"))
	if len(m) > 0 {
		return true
	}
	_, err := os.Stat(filepath.Join(root, "etc", "alpine-release"))
	return err == nil
}
