package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sqlc's SQLite engine can leave a named parameter unsubstituted in some positions (seen with `sqlc.arg(x) IN (…)`),
// which only fails at runtime with a syntax error. Every generated query must be fully substituted.
func TestGeneratedQueriesFullySubstituted(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("db", "*.go"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"sqlc.arg(", "sqlc.narg("} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s contains an unsubstituted %s parameter", f, bad)
			}
		}
	}
}
