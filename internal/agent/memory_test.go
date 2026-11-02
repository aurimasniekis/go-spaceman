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
	write(t, one, "scratch.md", "unsaved")
	if st := m.statusOf(ctx, "spc_a", "implementor-backend"); st.Uncheckpointed != 1 {
		t.Fatalf("uncheckpointed: %+v", st)
	}
	if len(m.collections()) != 2 {
		t.Fatalf("collections: %v", m.collections())
	}
	// Invalid keys never reach git.
	if _, err := m.prepare(ctx, "spc_a", "../etc", "ins_one"); err == nil {
		t.Fatal("a path role was accepted")
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
