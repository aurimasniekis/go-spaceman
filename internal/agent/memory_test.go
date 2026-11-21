package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

func memAgent(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()
	clk := clock.NewFake(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))
	j, err := OpenJournal(context.Background(), filepath.Join(dir, "journal"), nil, clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	a := New(j, Options{DataDir: dir, Clock: clk})
	if a.mem.git == nil {
		t.Skip("git not installed")
	}
	return a
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// SM-MEM-001…003: instances of a role share one history; concurrent edits merge; conflicting edits are preserved,
// never overwritten or force-pushed.
func TestMemoryInstancesShareAndPreserve(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	one, err := m.prepare(ctx, "spc_a", "implementor-backend", "ins_one")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, one, "README.md"), "implementor-backend") {
		t.Fatal("the first instance did not start the collection")
	}
	write(t, one, "decisions.md", "# Decisions\n\n- retries: 3\n")
	m.checkpoint(ctx, "spc_a", "implementor-backend", "ins_one", "idle")

	// A second instance starts from the shared history.
	two, err := m.prepare(ctx, "spc_a", "implementor-backend", "ins_two")
	if err != nil {
		t.Fatal(err)
	}
	if read(t, two, "decisions.md") != "# Decisions\n\n- retries: 3\n" {
		t.Fatal("the second instance does not share the history")
	}
	// Another role is a separate collection.
	fe, err := m.prepare(ctx, "spc_a", "implementor-frontend", "ins_fe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fe, "decisions.md")); err == nil {
		t.Fatal("frontend sees backend memory")
	}

	// Edits to different files merge cleanly.
	write(t, one, "notes/a.md", "from one\n")
	write(t, two, "notes/b.md", "from two\n")
	m.checkpoint(ctx, "spc_a", "implementor-backend", "ins_one", "idle")
	m.checkpoint(ctx, "spc_a", "implementor-backend", "ins_two", "idle")
	if read(t, two, "notes/a.md") != "from one\n" {
		t.Fatal("instance two did not merge instance one's notes")
	}
	m.checkpoint(ctx, "spc_a", "implementor-backend", "ins_one", "idle")
	if read(t, one, "notes/b.md") != "from two\n" {
		t.Fatal("instance one did not pick up instance two's notes")
	}

	// Both change the same line: the shared version wins, instance two's is preserved twice.
	write(t, one, "decisions.md", "# Decisions\n\n- retries: 5\n")
	m.checkpoint(ctx, "spc_a", "implementor-backend", "ins_one", "idle")
	write(t, two, "decisions.md", "# Decisions\n\n- retries: 1\n")
	m.checkpoint(ctx, "spc_a", "implementor-backend", "ins_two", "idle")
	if read(t, two, "decisions.md") != "# Decisions\n\n- retries: 5\n" {
		t.Fatalf("shared version not kept: %q", read(t, two, "decisions.md"))
	}
	matches, _ := filepath.Glob(filepath.Join(two, "conflicts", "ins_two-*", "decisions.md"))
	if len(matches) != 1 || read(t, filepath.Dir(matches[0]), "decisions.md") != "# Decisions\n\n- retries: 1\n" {
		t.Fatalf("conflicting version not preserved as a file: %v", matches)
	}
	shared := m.shared("spc_a", "implementor-backend")
	refs, err := m.git.Run(ctx, shared, nil, "for-each-ref", "--format=%(refname)", "refs/conflicts/")
	if err != nil || !strings.Contains(refs, "refs/conflicts/ins_two-") {
		t.Fatalf("conflicting history not preserved as a ref: %q %v", refs, err)
	}
	st := m.statusOf(ctx, "spc_a", "implementor-backend")
	if st.Conflicts != 1 || st.Instances != 2 || !st.Pending || st.LocalCommits == 0 || st.Uncheckpointed != 0 {
		t.Fatalf("status: %+v", st)
	}
	if len(st.PreservedFolders) != 1 || !strings.HasPrefix(st.PreservedFolders[0], "ins_two-") ||
		filepath.Dir(matches[0]) != filepath.Join(two, "conflicts", st.PreservedFolders[0]) {
		t.Fatalf("preserved folders: %v", st.PreservedFolders)
	}
	write(t, one, "scratch.md", "unsaved")
	if st := m.statusOf(ctx, "spc_a", "implementor-backend"); st.Uncheckpointed != 1 {
		t.Fatalf("uncheckpointed: %+v", st)
	}
	// An instance folder in the bare layout (HEAD, objects/, refs/, commondir, no .git) is counted as an instance but
	// nothing in it is counted and no git runs there, even with a commondir that leads to the shared history.
	bare := filepath.Join(m.root, "spc_a", "implementor-backend", "instances", "ins_bare")
	// HEAD is a commit the shared history does not have in main (a preserved conflict), so a Count there would show.
	lost, err := m.git.Run(ctx, shared, nil, "rev-parse", "--verify", refs[:strings.Index(refs, "\n")]+"^{commit}")
	if err != nil {
		t.Fatal(err)
	}
	write(t, bare, "HEAD", strings.TrimSpace(lost)+"\n")
	write(t, bare, "commondir", shared+"\n")
	for _, d := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(bare, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	before := st.LocalCommits
	got := m.statusOf(ctx, "spc_a", "implementor-backend")
	if got.Instances != 3 || got.Uncheckpointed != 1 || got.LocalCommits != before {
		t.Fatalf("bare-layout instance was counted: before %d, %+v", before, got)
	}
	if err := os.RemoveAll(bare); err != nil {
		t.Fatal(err)
	}
	if len(m.collections(ctx)) != 2 {
		t.Fatalf("collections: %v", m.collections(ctx))
	}
	// Invalid keys never reach git.
	if _, err := m.prepare(ctx, "spc_a", "../etc", "ins_one"); err == nil {
		t.Fatal("a path role was accepted")
	}
}

// An instance checkout that was unborn when it got its first file has a root commit unrelated to the shared history;
// the checkpoint still has to bring the file to shared main.
func TestMemoryUnbornCheckoutReachesShared(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	if _, err := m.prepare(ctx, "spc_u", "implementor-backend", "ins_one"); err != nil {
		t.Fatal(err)
	}
	inst := m.instance("spc_u", "implementor-backend", "ins_unborn")
	if err := os.MkdirAll(inst, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.git.Run(ctx, inst, nil, "init", "--quiet", "--initial-branch=main", "."); err != nil {
		t.Fatal(err)
	}
	write(t, inst, "notes/unborn.md", "from the unborn checkout\n")
	if err := m.checkpointErr(ctx, "spc_u", "implementor-backend", "ins_unborn", "idle"); err != nil {
		t.Fatal(err)
	}
	shared := m.shared("spc_u", "implementor-backend")
	got, err := m.git.Run(ctx, shared, nil, "show", "refs/heads/main:notes/unborn.md")
	if err != nil || got != "from the unborn checkout\n" {
		t.Fatalf("file did not reach shared main: %q %v", got, err)
	}
	if refs, _ := m.git.Run(ctx, shared, nil, "for-each-ref", "refs/conflicts/"); strings.TrimSpace(refs) != "" {
		t.Fatalf("unexpected conflict refs: %q", refs)
	}
}

// A conflict on one file must not hold back a non-conflicting change: it reaches shared main, and only the conflicting
// file keeps the shared version with the local copy under conflicts/.
func TestPreserveKeepsNonConflictingChangesOnMain(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	const sp, role = "spc_n", "implementor-backend"
	one, err := m.prepare(ctx, sp, role, "ins_one")
	if err != nil {
		t.Fatal(err)
	}
	write(t, one, "a.md", "base\n")
	m.checkpoint(ctx, sp, role, "ins_one", "idle")
	two, err := m.prepare(ctx, sp, role, "ins_two")
	if err != nil {
		t.Fatal(err)
	}
	write(t, one, "a.md", "one\n")
	m.checkpoint(ctx, sp, role, "ins_one", "idle")
	write(t, two, "a.md", "two\n")
	write(t, two, "b.md", "only two\n")
	if err := m.checkpointErr(ctx, sp, role, "ins_two", "idle"); err != nil {
		t.Fatal(err)
	}
	shared := m.shared(sp, role)
	show := func(p string) string {
		out, err := m.git.Run(ctx, shared, nil, "show", "refs/heads/main:"+p)
		if err != nil {
			t.Fatalf("show %s: %v", p, err)
		}
		return out
	}
	if got := show("b.md"); got != "only two\n" {
		t.Fatalf("non-conflicting file on main: %q", got)
	}
	if got := show("a.md"); got != "one\n" {
		t.Fatalf("conflicting file should keep the shared version: %q", got)
	}
	files, _ := m.git.Run(ctx, shared, nil, "ls-tree", "-r", "--name-only", "refs/heads/main")
	var copyPath string
	for _, f := range strings.Fields(files) {
		if strings.HasPrefix(f, "conflicts/ins_two-") && strings.HasSuffix(f, "/a.md") {
			copyPath = f
		}
	}
	if copyPath == "" {
		t.Fatalf("no conflict copy of a.md in %q", files)
	}
	if got := show(copyPath); got != "two\n" {
		t.Fatalf("conflict copy: %q", got)
	}
}

// Conflicting files with spaces or non-ASCII characters in their names are still preserved and the instance's other
// work still reaches shared main.
func TestPreserveConflictingOddNames(t *testing.T) {
	for _, name := range []string{"my notes.md", "zurnalas-ąčę.md"} {
		t.Run(name, func(t *testing.T) {
			a := memAgent(t)
			ctx := context.Background()
			m := a.mem
			const sp, role = "spc_o", "implementor-backend"
			one, err := m.prepare(ctx, sp, role, "ins_one")
			if err != nil {
				t.Fatal(err)
			}
			write(t, one, name, "base\n")
			m.checkpoint(ctx, sp, role, "ins_one", "idle")
			two, err := m.prepare(ctx, sp, role, "ins_two")
			if err != nil {
				t.Fatal(err)
			}
			write(t, one, name, "one\n")
			m.checkpoint(ctx, sp, role, "ins_one", "idle")
			write(t, two, name, "two\n")
			write(t, two, "b.md", "only two\n")
			if err := m.checkpointErr(ctx, sp, role, "ins_two", "idle"); err != nil {
				t.Fatal(err)
			}
			shared := m.shared(sp, role)
			show := func(p string) string {
				out, err := m.git.Run(ctx, shared, nil, "show", "refs/heads/main:"+p)
				if err != nil {
					t.Fatalf("show %s: %v", p, err)
				}
				return out
			}
			if got := show("b.md"); got != "only two\n" {
				t.Fatalf("non-conflicting file on main: %q", got)
			}
			if got := show(name); got != "one\n" {
				t.Fatalf("conflicting file should keep the shared version: %q", got)
			}
			files, _ := m.git.Run(ctx, shared, nil, "-c", "core.quotepath=false", "ls-tree", "-r", "-z", "--name-only", "refs/heads/main")
			var copyPath string
			for _, f := range splitNUL(files) {
				if strings.HasPrefix(f, "conflicts/ins_two-") && strings.HasSuffix(f, "/"+name) {
					copyPath = f
				}
			}
			if copyPath == "" {
				t.Fatalf("no conflict copy of %s in %q", name, files)
			}
			if got := show(copyPath); got != "two\n" {
				t.Fatalf("conflict copy: %q", got)
			}
		})
	}
}

// SM-MEM-004/005: central's answers move the acknowledged head; a missing base resets it for a full resend; failures
// are counted and kept for the status.
func TestMemoryAcks(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	one, err := m.prepare(ctx, "spc_a", "control", "ins_c")
	if err != nil {
		t.Fatal(err)
	}
	_ = one
	head := m.git.RevParse(ctx, m.shared("spc_a", "control"), "refs/heads/main")
	m.waiting[colKey("spc_a", "control")] = "msy_1"
	m.acked(ctx, proto.MemoryAck{SyncID: "msy_other", SpaceID: "spc_a", Role: "control", Result: proto.MemoryOK, Head: head})
	if st, _ := a.j.memory(ctx, "spc_a", "control"); st.Acked != "" {
		t.Fatal("an unexpected ack was applied")
	}
	m.acked(ctx, proto.MemoryAck{SyncID: "msy_1", SpaceID: "spc_a", Role: "control", Result: proto.MemoryOK, Head: head})
	st, _ := a.j.memory(ctx, "spc_a", "control")
	if st.Acked != head || st.LastAckAt == nil {
		t.Fatalf("ok ack: %+v", st)
	}
	if s := m.statusOf(ctx, "spc_a", "control"); s.Pending || s.LocalCommits != 0 {
		t.Fatalf("status after ack: %+v", s)
	}
	m.waiting[colKey("spc_a", "control")] = "msy_2"
	m.acked(ctx, proto.MemoryAck{SyncID: "msy_2", SpaceID: "spc_a", Role: "control", Result: proto.MemoryMissingBase})
	if st, _ := a.j.memory(ctx, "spc_a", "control"); st.Acked != "" {
		t.Fatal("missing base did not reset the acknowledged head")
	}
	m.waiting[colKey("spc_a", "control")] = "msy_3"
	m.acked(ctx, proto.MemoryAck{SyncID: "msy_3", SpaceID: "spc_a", Role: "control", Result: proto.MemoryConflict, Message: "kept as refs/conflicts/x"})
	st, _ = a.j.memory(ctx, "spc_a", "control")
	if st.Retries != 1 || !strings.Contains(st.LastError, "conflict") {
		t.Fatalf("conflict ack: %+v", st)
	}
	m.mu.Lock()
	for _, t := range m.syncs {
		t.Stop()
	}
	m.mu.Unlock()
}

// A committed conflicts -> /elsewhere symlink must not make preserve write outside the instance.
func TestPreserveRefusesSymlinkedConflicts(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	const sp, role = "spc_a", "implementor-backend"
	one, err := m.prepare(ctx, sp, role, "ins_one")
	if err != nil {
		t.Fatal(err)
	}
	write(t, one, "decisions.md", "base\n")
	m.checkpoint(ctx, sp, role, "ins_one", "idle")
	two, err := m.prepare(ctx, sp, role, "ins_two")
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(one, "conflicts")); err != nil {
		t.Fatal(err)
	}
	write(t, one, "decisions.md", "one\n")
	m.checkpoint(ctx, sp, role, "ins_one", "idle")
	write(t, two, "decisions.md", "two\n")
	if err := m.checkpointErr(ctx, sp, role, "ins_two", "idle"); err == nil {
		t.Fatal("preserve wrote through a symlinked conflicts/")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("the symlink target was written to: %v", ents)
	}
}

func conflictRoot(t *testing.T) (string, *os.Root) {
	t.Helper()
	inst := t.TempDir()
	if err := os.Mkdir(filepath.Join(inst, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(inst)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return inst, root
}

func requireEmptyDir(t *testing.T, dir string) {
	t.Helper()
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("%s was written to: %v", dir, ents)
	}
}

// Layer (a), os.Root alone: with the guards out of the way, a symlink to outside the instance is not followed.
func TestConflictWritesStayInsideRoot(t *testing.T) {
	inst, root := conflictRoot(t)
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(inst, "conflicts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(inst, "conflicts", "x")); err != nil {
		t.Fatal(err)
	}
	if err := writeUnderRoot(root, filepath.Join("conflicts", "x"), map[string][]byte{"decisions.md": []byte("mine")}, "note"); err == nil {
		t.Fatal("wrote through a symlink leaving the instance")
	}
	requireEmptyDir(t, outside)
}

// Layer (b), the guard alone: conflicts -> .git stays inside the instance, so os.Root would allow it.
func TestPrepareRefusesConflictsSymlinkIntoGit(t *testing.T) {
	inst, root := conflictRoot(t)
	if err := os.Symlink(".git", filepath.Join(inst, "conflicts")); err != nil {
		t.Fatal(err)
	}
	err := prepareConflictDir(root, filepath.Join("conflicts", "ins_one-1"))
	if err == nil || !strings.Contains(err.Error(), "Recover by removing") {
		t.Fatalf("conflicts -> .git accepted or error gives no recovery: %v", err)
	}
	requireEmptyDir(t, filepath.Join(inst, ".git"))
}

// Layer (c): a pre-committed conflicts/<inst>-<stamp>/ holding decisions.md -> ../../.git/description is refused.
func TestWriteConflictCopiesRefusesPrecreatedFolder(t *testing.T) {
	inst, root := conflictRoot(t)
	desc := filepath.Join(inst, ".git", "description")
	if err := os.WriteFile(desc, []byte("victim"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("conflicts", "ins_one-1")
	if err := os.MkdirAll(filepath.Join(inst, dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../.git/description", filepath.Join(inst, dir, "decisions.md")); err != nil {
		t.Fatal(err)
	}
	root.Close()
	err := writeConflictCopies(inst, dir, map[string][]byte{"decisions.md": []byte("mine")}, "note")
	if err == nil {
		t.Fatal("a pre-created conflict folder was accepted")
	}
	if b, _ := os.ReadFile(desc); string(b) != "victim" {
		t.Fatalf(".git/description was overwritten: %q", b)
	}
}

// A checkout whose own configuration redirects the work tree (core.worktree) or declares it bare is refused by every
// memory git operation, preserve and its reset --hard included, and nothing outside the checkout is touched.
func TestPreserveRefusesRedirectedWorktree(t *testing.T) {
	for _, tc := range []struct{ key, value string }{{"core.worktree", ""}, {"core.bare", "true"}} {
		t.Run(tc.key, func(t *testing.T) {
			a := memAgent(t)
			ctx := context.Background()
			m := a.mem
			inst, err := m.prepare(ctx, "spc_a", "role", "ins_one")
			if err != nil {
				t.Fatal(err)
			}
			head := m.git.RevParse(ctx, inst, "HEAD")
			outside := t.TempDir()
			write(t, outside, "victim.md", "keep\n")
			value := tc.value
			if value == "" {
				value = outside
			}
			if _, err := m.git.Run(ctx, inst, nil, "config", tc.key, value); err != nil {
				// the guard may already refuse "config"? it must not: config is how the key got there in this test
				t.Fatal(err)
			}
			if _, err := m.preserve(ctx, "spc_a", "role", "ins_one", head, head, []string{"README.md"}); err == nil {
				t.Fatal("preserve ran in a checkout whose work tree is redirected")
			}
			if got := read(t, outside, "victim.md"); got != "keep\n" {
				t.Fatalf("the folder outside was changed: %q", got)
			}
			entries, _ := os.ReadDir(outside)
			if len(entries) != 1 {
				t.Fatalf("files appeared outside the checkout: %v", entries)
			}
			if _, err := os.Stat(filepath.Join(inst, "conflicts")); err == nil {
				t.Fatal("preserve wrote into the checkout")
			}
			// Without the key it works again, so the refusal is the guard's.
			if _, err := m.git.Run(ctx, inst, nil, "config", "--unset", tc.key); err != nil {
				t.Fatal(err)
			}
			if _, err := m.git.Run(ctx, inst, nil, "status", "--porcelain"); err != nil {
				t.Fatalf("a clean checkout refused: %v", err)
			}
		})
	}
}
