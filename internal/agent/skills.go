package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// managedMarker marks a skill folder SpaceMan wrote; only such folders are ever replaced or removed.
const managedMarker = ".spaceman-managed"

// syncSkills makes root hold exactly the run's SpaceMan skills (SM-MCP-004): stale SpaceMan-managed skill folders are
// removed, current ones rewritten. A folder SpaceMan did not write (the user's own skill of the same name) is never
// touched, and the SpaceMan skill is skipped. Files are written 0600 and never made executable: skills are guidance.
func syncSkills(root string, skills []proto.Skill) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	want := map[string]proto.Skill{}
	for _, s := range skills {
		if !safeName(s.Name) {
			return fmt.Errorf("skill name %q is not a plain name", s.Name)
		}
		want[s.Name] = s
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || !managed(filepath.Join(root, e.Name())) {
			continue
		}
		if _, keep := want[e.Name()]; !keep {
			if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
				return err
			}
		}
	}
	for name, s := range want {
		dir := filepath.Join(root, name)
		if _, err := os.Stat(dir); err == nil && !managed(dir) {
			continue // the user's own skill: never overwritten
		}
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		for p, content := range s.Files {
			clean := filepath.Clean(p)
			if filepath.IsAbs(clean) || clean == "." || strings.HasPrefix(clean, "..") || clean == managedMarker {
				return fmt.Errorf("skill %s: unsafe path %q", name, p)
			}
			full := filepath.Join(dir, clean)
			if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(dir, managedMarker), []byte(fmt.Sprintf("version %d\nsha256 %s\n", s.Version, s.SHA256)), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// managed reports whether dir carries SpaceMan's marker. Lstat: a marker that is a symlink does not count, and any doubt
// leaves the folder alone.
func managed(dir string) bool {
	fi, err := os.Lstat(filepath.Join(dir, managedMarker))
	return err == nil && fi.Mode().IsRegular()
}

func safeName(n string) bool {
	if n == "" || len(n) > 63 || n[0] == '.' {
		return false
	}
	for _, r := range n {
		lower, digit := r >= 'a' && r <= 'z', r >= '0' && r <= '9'
		if !lower && !digit && r != '-' {
			return false
		}
	}
	return true
}
