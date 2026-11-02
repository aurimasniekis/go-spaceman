// Package buildinfo exposes version metadata injected at link time.
//
// Derived from github.com/aurimasniekis/go-hallpass internal/buildinfo (Apache-2.0, same author).
package buildinfo

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
)

// Set via -ldflags "-X github.com/aurimasniekis/go-spaceman/internal/buildinfo.Version=…".
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Info describes the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Date      string `json:"date,omitempty"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

// Get returns build information, falling back to VCS metadata embedded by the Go toolchain.
func Get() Info {
	info := Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	if info.Commit == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				switch s.Key {
				case "vcs.revision":
					info.Commit = s.Value
				case "vcs.time":
					if info.Date == "" {
						info.Date = s.Value
					}
				}
			}
		}
	}
	return info
}

// String renders a single-line version string.
func (i Info) String() string {
	commit := i.Commit
	if len(commit) > 12 {
		commit = commit[:12]
	}
	if commit == "" {
		commit = "unknown"
	}
	return fmt.Sprintf("spaceman %s (commit %s, %s, %s)", i.Version, commit, i.GoVersion, i.Platform)
}

// Print writes the build information as one line or as JSON.
func Print(w io.Writer, asJSON bool) error {
	info := Get()
	if asJSON {
		return json.NewEncoder(w).Encode(info)
	}
	_, err := fmt.Fprintln(w, info.String())
	return err
}
