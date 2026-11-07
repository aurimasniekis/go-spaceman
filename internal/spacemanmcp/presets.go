package spacemanmcp

import (
	"fmt"
	"sort"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
)

// Preset names a server's default tool set and token grants (ADR-0063 §Decision 7).
type Preset string

// The presets. Dangerous and sensitive-read tools are never part of a preset: a server adds them by name.
const (
	// PresetRead offers the read tools, with viewer grants on one space.
	PresetRead Preset = "read"
	// PresetOperate offers the read and write tools, with operator grants on one space.
	PresetOperate Preset = "operate"
	// PresetAdmin ("infra", the default) offers the read and write tools, with admin grants on every space.
	PresetAdmin Preset = "admin"
	// PresetCustom offers only the tools a server names.
	PresetCustom Preset = "custom"
)

// DefaultPreset is the preset of a server that names none (operator decision for a trusted infra space).
const DefaultPreset = PresetAdmin

// Presets lists the presets.
var Presets = []Preset{PresetRead, PresetOperate, PresetAdmin, PresetCustom}

// Valid reports whether p is a known preset.
func (p Preset) Valid() bool {
	for _, q := range Presets {
		if p == q {
			return true
		}
	}
	return false
}

// Classes returns the tool classes a preset offers without naming them.
func (p Preset) Classes() []Class {
	switch p {
	case PresetRead:
		return []Class{Read}
	case PresetOperate, PresetAdmin:
		return []Class{Read, Write}
	}
	return nil
}

// Grants returns the token grants a preset suggests for a space ("" for the admin preset, which is global).
func (p Preset) Grants(spaceID string) []authz.Grant {
	switch p {
	case PresetRead:
		return []authz.Grant{{Role: authz.RoleViewer, Domain: spaceID}}
	case PresetOperate:
		return []authz.Grant{{Role: authz.RoleOperator, Domain: spaceID}}
	case PresetAdmin:
		return []authz.Grant{{Role: authz.RoleAdmin, Domain: authz.Global}}
	}
	return nil
}

// Selection is a server's tool choice: a preset plus tools named by name (without the slug).
type Selection struct {
	Preset Preset
	Tools  []string
}

// Select returns the tools of a selection that the grants can reach, sorted by name. Unknown or excluded names fail;
// a named tool the grants can never reach is left out like any other.
func (c *Catalog) Select(sel Selection, grants []authz.Grant) ([]*Tool, error) {
	preset := sel.Preset
	if preset == "" {
		preset = DefaultPreset
	}
	if !preset.Valid() {
		return nil, fmt.Errorf("unknown preset %q", sel.Preset)
	}
	want := map[string]bool{}
	for _, cls := range preset.Classes() {
		for _, t := range c.tools {
			if t.Class == cls {
				want[t.Name] = true
			}
		}
	}
	for _, n := range sel.Tools {
		if _, ok := c.byName[n]; !ok {
			if _, excluded := c.excluded[toOperationID(n)]; excluded {
				return nil, fmt.Errorf("tool %q is never offered: %s", n, c.excluded[toOperationID(n)])
			}
			return nil, fmt.Errorf("unknown tool %q", n)
		}
		want[n] = true
	}
	out := make([]*Tool, 0, len(want))
	for n := range want {
		if t := c.byName[n]; Reachable(t, grants) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Reachable reports whether some grant could ever satisfy the tool's rule: the role holds the permission, in "*" for a
// global rule or a global-only permission, in any domain for a space rule. The API still checks every call (token and
// owner); this only keeps tools the token can never use out of the catalog.
func Reachable(t *Tool, grants []authz.Grant) bool {
	for _, g := range grants {
		if !authz.RoleHas(g.Role, t.Rule.Perm) {
			continue
		}
		if g.Domain == authz.Global || (t.Rule.Space && !t.Rule.Perm.GlobalOnly) {
			return true
		}
	}
	return false
}

func toOperationID(name string) string {
	b := []byte(name)
	for i := range b {
		if b[i] == '_' {
			b[i] = '-'
		}
	}
	return string(b)
}
