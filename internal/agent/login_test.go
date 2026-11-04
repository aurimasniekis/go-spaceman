package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/harness"
)

// ADR-0053: a Codex ChatGPT login is one live file per account on the node, which every instance links to. A refresh by
// the harness survives later launches, a rotation in central replaces it, and an instance moved to an API key loses
// the links but keeps a login file of its own.
func TestLinkLogin(t *testing.T) {
	data := t.TempDir()
	a := &Agent{o: Options{DataDir: data}}
	links := func(home string) map[string]string {
		return map[string]string{filepath.Join(home, "auth.json"): "auth.json", filepath.Join(home, "auth.lock"): "auth.lock"}
	}
	homeA, homeB := filepath.Join(data, "spaces", "s", "instances", "a", "codex"), filepath.Join(data, "spaces", "s", "instances", "b", "codex")
	seed := []byte(`{"auth_mode":"chatgpt","tokens":{"refresh_token":"r1"}}`)
	for _, home := range []string{homeA, homeB} {
		if err := a.linkLogin(&harness.Login{Account: "acc_1", Seed: seed}, links(home)); err != nil {
			t.Fatal(err)
		}
	}
	ta, errA := os.Readlink(filepath.Join(homeA, "auth.json"))
	tb, errB := os.Readlink(filepath.Join(homeB, "auth.json"))
	if errA != nil || errB != nil || ta != tb || filepath.Dir(ta) != a.loginDir("acc_1") {
		t.Fatalf("links %q %v / %q %v", ta, errA, tb, errB)
	}
	if lk, err := os.Readlink(filepath.Join(homeA, "auth.lock")); err != nil || filepath.Dir(lk) != filepath.Dir(ta) {
		t.Fatalf("lock link %q %v", lk, err)
	}
	if fi, err := os.Stat(ta); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("login mode %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Dir(ta)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("login folder mode %v %v", fi, err)
	}

	// The harness refreshes the login through the link; the same seed again keeps the refreshed file.
	refreshed := []byte(`{"auth_mode":"chatgpt","tokens":{"refresh_token":"r2"}}`)
	if err := os.WriteFile(filepath.Join(homeB, "auth.json"), refreshed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.linkLogin(&harness.Login{Account: "acc_1", Seed: seed}, links(homeA)); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(homeA, "auth.json")); string(got) != string(refreshed) {
		t.Fatalf("a launch overwrote the refreshed login: %s", got)
	}
	// A rotated seed replaces it.
	rotated := []byte(`{"auth_mode":"chatgpt","tokens":{"refresh_token":"r3"}}`)
	if err := a.linkLogin(&harness.Login{Account: "acc_1", Seed: rotated}, links(homeA)); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(homeB, "auth.json")); string(got) != string(rotated) {
		t.Fatalf("rotation not applied: %s", got)
	}
	// Another account has its own live file.
	if err := a.linkLogin(&harness.Login{Account: "acc_2", Seed: seed}, links(filepath.Join(data, "other"))); err != nil {
		t.Fatal(err)
	}
	if a.loginDir("acc_2") == a.loginDir("acc_1") {
		t.Fatal("two accounts share a login folder")
	}

	// Instance A moves to an API key: its links go; instance C's own login file stays.
	if err := a.linkLogin(nil, links(homeA)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(homeA, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("link kept: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(homeB, "auth.json")); string(got) != string(rotated) {
		t.Fatal("removing one instance's links touched the live login")
	}
	homeC := filepath.Join(data, "c")
	if err := os.MkdirAll(homeC, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeC, "auth.json"), []byte(`{"auth_mode":"apikey"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.linkLogin(nil, links(homeC)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(homeC, "auth.json")); err != nil {
		t.Fatalf("a regular login file was removed: %v", err)
	}

	// File names are plain names in the login folder.
	for _, bad := range []string{"../x", ".hidden", "seed.sha256", ""} {
		if err := a.linkLogin(&harness.Login{Account: "acc_1", Seed: seed}, map[string]string{filepath.Join(homeA, "x"): bad}); err == nil {
			t.Fatalf("name %q accepted", bad)
		}
	}
}

// ADR-0054: an API key for Codex is a stored login in the instance's CODEX_HOME. Written over a link to a shared
// ChatGPT login, it replaces the link and leaves the shared login alone.
func TestWriteSecretFilesReplacesLinks(t *testing.T) {
	data := t.TempDir()
	a := &Agent{o: Options{DataDir: data}}
	home := filepath.Join(data, "instances", "i", "codex")
	links := map[string]string{filepath.Join(home, "auth.json"): "auth.json", filepath.Join(home, "auth.lock"): "auth.lock"}
	chatgpt := []byte(`{"auth_mode":"chatgpt","tokens":{"refresh_token":"r1"}}`)
	if err := a.linkLogin(&harness.Login{Account: "acc_1", Seed: chatgpt}, links); err != nil {
		t.Fatal(err)
	}
	shared, _ := os.Readlink(filepath.Join(home, "auth.json"))
	apikey := []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-x"}`)
	// The launch order: links of the earlier login go, then the key's login file is written.
	if err := a.linkLogin(nil, links); err != nil {
		t.Fatal(err)
	}
	// Even with a link still in place (a crash in between), the file replaces it and never writes through it.
	if err := os.Symlink(shared, filepath.Join(home, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretFiles(map[string][]byte{filepath.Join(home, "auth.json"): apikey}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(home, "auth.json"))
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("API-key login file %v %v", fi, err)
	}
	if got, _ := os.ReadFile(filepath.Join(home, "auth.json")); string(got) != string(apikey) {
		t.Fatalf("content %s", got)
	}
	if got, _ := os.ReadFile(shared); string(got) != string(chatgpt) {
		t.Fatalf("the shared ChatGPT login was overwritten through the link: %s", got)
	}
}
