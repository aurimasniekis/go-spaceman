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

// ADR-0060 decision 6: a gateway run's stored credentials are removed before it starts: a regular file, and a link
// (only the link: its target, a shared login, stays).
func TestRemoveCredentialFiles(t *testing.T) {
	dir := t.TempDir()
	file, link, shared := filepath.Join(dir, "auth.json"), filepath.Join(dir, ".credentials.json"), filepath.Join(dir, "shared.json")
	for _, p := range []string{file, shared} {
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(shared, link); err != nil {
		t.Fatal(err)
	}
	if err := removeCredentialFiles([]string{file, link, filepath.Join(dir, "missing.json")}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{file, link} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s is still there: %v", p, err)
		}
	}
	if _, err := os.Stat(shared); err != nil {
		t.Fatalf("the link's target was removed: %v", err)
	}
	// A directory in its place can't be removed: the launch fails rather than start next to it.
	if err := os.MkdirAll(filepath.Join(file, "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := removeCredentialFiles([]string{file}); err == nil {
		t.Fatal("a credential path that could not be removed was accepted")
	}
}

// HP-T4 review F1: Codex's old shell snapshots are removed with their contents, never following a link: a link in the
// folder's place and a link inside it are removed themselves, and their targets stay.
func TestRemoveCredentialDirs(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "keep.sh")
	if err := os.WriteFile(target, []byte("export KEEP=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snaps := filepath.Join(dir, "shell_snapshots")
	if err := os.MkdirAll(filepath.Join(snaps, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snaps, "nested", "s.sh"), []byte("export OPENAI_API_KEY=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(snaps, "file-link.sh")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(snaps, "dir-link")); err != nil {
		t.Fatal(err)
	}
	if err := removeCredentialDirs([]string{snaps, filepath.Join(dir, "missing")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(snaps); !os.IsNotExist(err) {
		t.Fatalf("the folder is still there: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("a link's target was removed: %v", err)
	}

	// The folder itself is a link to somewhere else: only the link goes.
	if err := os.Symlink(outside, snaps); err != nil {
		t.Fatal(err)
	}
	if err := removeCredentialDirs([]string{snaps}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(snaps); !os.IsNotExist(err) {
		t.Fatalf("the link is still there: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the linked folder's contents were removed: %v", err)
	}

	// A folder that can't be removed fails the launch.
	if os.Geteuid() == 0 {
		t.Skip("root can remove anything")
	}
	if err := os.MkdirAll(filepath.Join(snaps, "s"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(snaps, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(snaps, 0o700) })
	if err := removeCredentialDirs([]string{snaps}); err == nil {
		t.Fatal("a folder that could not be removed was accepted")
	}
}
