// Package skills holds the built-in skills SpaceMan seeds into its catalog (SM-MCP-004).
//
// seed/hallpass is a copy of go-hallpass integrations/skills/hallpass at 79a3c09 (Apache-2.0, same author): guidance for
// agents that reach servers through a HallPass gateway. Hallpass stays responsible for its own enforcement (SM-APPR-006).
package skills

import (
	"embed"
	"io/fs"
	"path"
)

//go:embed seed
var seed embed.FS

// Builtin is a seeded skill.
type Builtin struct {
	Slug, Name, Description, SourceRef string
	Files                              map[string]string
}

// Builtins returns the skills seeded into every catalog.
func Builtins() []Builtin {
	return []Builtin{{
		Slug:        "hallpass",
		Name:        "HallPass gateway",
		Description: "Guidance for reaching servers and APIs through a HallPass SSH gateway: requesting access, waiting for approval, exit codes.",
		SourceRef:   "go-hallpass integrations/skills/hallpass@79a3c09",
		Files:       files("seed/hallpass"),
	}}
}

func files(root string) map[string]string {
	out := map[string]string{}
	_ = fs.WalkDir(seed, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := seed.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := relTo(root, p)
		out[rel] = string(b)
		return nil
	})
	return out
}

func relTo(root, p string) (string, bool) {
	if len(p) > len(root)+1 && p[:len(root)] == root {
		return path.Clean(p[len(root)+1:]), true
	}
	return p, false
}
