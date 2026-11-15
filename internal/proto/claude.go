package proto

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
)

// MinorClaudeSettings is the first minor version whose launches may carry Claude Code settings (ADR-0059): central
// refuses to dispatch a run with settings to an older node, which would ignore them.
const MinorClaudeSettings = 8

// ClaudeDefaults is the entry that keeps Claude Code's built-in rules in an auto mode list.
const ClaudeDefaults = "$defaults"

// MaxClaudeSettingsBytes bounds a profile's Claude Code settings (ADR-0059).
const MaxClaudeSettingsBytes = 256 << 10

// ClaudeSettings are Claude Code settings.json keys a harness profile gives its runs (ADR-0059): any keys, set at the
// top level of the run's $CLAUDE_CONFIG_DIR/settings.json. The keys SpaceMan's UI edits are checked for shape.
type ClaudeSettings map[string]any

// ClaudePermissionModes are the values of permissions.defaultMode (and of the permission_mode setting).
var ClaudePermissionModes = map[string]bool{"default": true, "acceptEdits": true, "plan": true, "auto": true, "dontAsk": true, "bypassPermissions": true}

// Validate checks the settings' size and the shape of the keys SpaceMan edits (permissions, autoMode, env); every
// other key is Claude Code's to interpret. Each message starts with the key path.
func (s ClaudeSettings) Validate() []string {
	var out []string
	if b, err := json.Marshal(s); err != nil {
		return []string{"claude_settings: not JSON"}
	} else if len(b) > MaxClaudeSettingsBytes {
		out = append(out, fmt.Sprintf("claude_settings: at most %d KiB", MaxClaudeSettingsBytes>>10))
	}
	stringList := func(path string, v any) {
		if _, ok := v.([]string); ok {
			return
		}
		l, ok := v.([]any)
		ok = ok && !slices.ContainsFunc(l, func(e any) bool { _, isString := e.(string); return !isString })
		if !ok {
			out = append(out, path+": must be a list of strings")
		}
	}
	object := func(key string, lists []string, other func(k string, v any)) {
		raw, ok := s[key]
		if !ok {
			return
		}
		m, ok := raw.(map[string]any)
		if !ok {
			out = append(out, key+": must be an object")
			return
		}
		for _, k := range sortedKeys(m) {
			switch {
			case slices.Contains(lists, k):
				stringList(key+"."+k, m[k])
			case other != nil:
				other(k, m[k])
			}
		}
	}
	object("permissions", []string{"allow", "ask", "deny", "additionalDirectories"}, func(k string, v any) {
		if m, ok := v.(string); k == "defaultMode" && (!ok || !ClaudePermissionModes[m]) {
			out = append(out, "permissions.defaultMode: must be plan, default, dontAsk, acceptEdits, auto or bypassPermissions")
		}
	})
	object("autoMode", []string{"environment", "allow", "soft_deny"}, nil)
	object("env", nil, func(k string, v any) {
		if _, ok := v.(string); !ok {
			out = append(out, "env."+k+": must be a string")
		}
	})
	return out
}

// DefaultMode returns permissions.defaultMode, or "".
func (s ClaudeSettings) DefaultMode() string {
	p, _ := s["permissions"].(map[string]any)
	m, _ := p["defaultMode"].(string)
	return m
}

// WithDefaultMode returns a copy of s with permissions.defaultMode set to mode; s itself is not changed.
func (s ClaudeSettings) WithDefaultMode(mode string) ClaudeSettings {
	out := ClaudeSettings{}
	for k, v := range s {
		out[k] = v
	}
	p := map[string]any{}
	if cur, ok := s["permissions"].(map[string]any); ok {
		for k, v := range cur {
			p[k] = v
		}
	}
	p["defaultMode"] = mode
	out["permissions"] = p
	return out
}

// Keys returns the settings' top-level keys, sorted.
func (s ClaudeSettings) Keys() []string { return sortedKeys(s) }

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
