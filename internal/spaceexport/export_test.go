package spaceexport

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var admin = audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}

type nopSeeker struct{ *bytes.Reader }

func (nopSeeker) Close() error { return nil }

func setup(t *testing.T, max int64) (*Service, *store.Store) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))
	s := storetest.Open(t, storetest.WithClock(clk))
	kr := keyringtest.Load(t, s)
	blobs := map[string][]byte{"sum_1": []byte("hello attachment")}
	content := func(_ context.Context, sum string) (io.ReadSeekCloser, error) {
		b, ok := blobs[sum]
		if !ok {
			return nil, os.ErrNotExist
		}
		return nopSeeker{bytes.NewReader(b)}, nil
	}
	svc := New(s, audit.NewWriter(kr), content, func(context.Context, string) int64 { return max }, clk)
	ctx := context.Background()
	err := s.W().TxNamed(ctx, "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'pay', 'Payments', 'nod_1', 'active', 'a', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'other', 'Other', 'nod_1', 'active', 'b', 0, 't', 0)`,
			`INSERT INTO inbox_items (id, space_id, source_kind, title, content_type, body, trusted_json, received_at, created_by, updated_at)
			 VALUES ('inb_1', 'spc_a', 'operator', 'Fix the build', 'text/markdown', 'See spaceman://attachment/att_1', '{"user":"usr_x"}', 1, 't', 1)`,
			`INSERT INTO inbox_items (id, space_id, source_kind, title, content_type, received_at, created_by, updated_at)
			 VALUES ('inb_other', 'spc_b', 'operator', 'Not exported', 'text/markdown', 1, 't', 1)`,
			`INSERT INTO attachments (id, space_id, sha256, size, mime, name, uploaded_by, created_at) VALUES ('att_1', 'spc_a', 'sum_1', 16, 'text/plain', 'a.txt', 't', 1)`,
			`INSERT INTO attachments (id, space_id, sha256, size, mime, name, uploaded_by, created_at) VALUES ('att_gone', 'spc_a', 'sum_gone', 5, 'text/plain', 'b.txt', 't', 2)`,
			`INSERT INTO attachment_refs (attachment_id, owner_kind, owner_id, created_at) VALUES ('att_1', 'inbox_item', 'inb_1', 1)`,
			`INSERT INTO tasks (id, space_id, title, rank, status, created_at, created_by, updated_at) VALUES ('tsk_1', 'spc_a', 'Build', 1, 'done', 1, 't', 1)`,
			`INSERT INTO human_actions (id, space_id, type, title, requester_kind, rank, status, outcome, response_json, responded_by, created_at, created_by, updated_at)
			 VALUES ('hum_1', 'spc_a', 'question', 'Which DB?', 'agent', 1, 'resolved', 'answered', '{"answer":"sqlite"}', 'usr_x', 1, 't', 2)`,
			`INSERT INTO human_comments (id, action_id, body, author, created_at) VALUES ('hcm_1', 'hum_1', 'Why not?', 'usr_x', 3)`,
			`INSERT INTO messages (id, space_id, from_kind, to_kind, kind, subject, body, created_at) VALUES ('msg_1', 'spc_a', 'operator', 'control', 'message', 'Hi', 'Hello', 1)`,
		} {
			if _, err := tx.SQL.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("%s: %w", q, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, s
}

func readZip(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name], _ = io.ReadAll(rc)
		_ = rc.Close()
	}
	return out
}

func lines(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestExportArchive(t *testing.T) {
	svc, s := setup(t, 1<<30)
	ctx := context.Background()
	sum, err := svc.Summarize(ctx, "spc_a")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Counts != (Counts{InboxItems: 1, Tasks: 1, HumanActions: 1, HumanComments: 1, Messages: 1, Attachments: 2}) || !sum.WithinLimit || sum.Encrypted {
		t.Fatalf("summary %+v", sum)
	}
	slug, sum, err := svc.Prepare(ctx, admin, "spc_a")
	if err != nil || slug != "pay" {
		t.Fatalf("prepare %q %v", slug, err)
	}
	var buf bytes.Buffer
	m, err := svc.Write(ctx, &buf, admin, "spc_a", sum.MaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	files := readZip(t, buf.Bytes())
	for _, name := range []string{"README.txt", "manifest.json", "space.json", "inbox_items.jsonl", "tasks.jsonl", "task_dependencies.jsonl",
		"task_blockers.jsonl", "human_actions.jsonl", "human_comments.jsonl", "messages.jsonl", "attachments.jsonl", "attachment_refs.jsonl", "attachments/att_1"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("missing %s (have %v)", name, keys(files))
		}
	}
	if string(files["attachments/att_1"]) != "hello attachment" || !strings.Contains(string(files["README.txt"]), "NOT encrypted") {
		t.Fatal("attachment content or warning")
	}
	var man Manifest
	if err := json.Unmarshal(files["manifest.json"], &man); err != nil {
		t.Fatal(err)
	}
	if man.Format != Format || man.Encrypted || man.SpaceSlug != "pay" || man.Counts.InboxItems != 1 || len(man.Missing) != 1 || man.Missing[0] != "att_gone" ||
		man.ExportedBy != "user:usr_admin" {
		t.Fatalf("manifest %+v", man)
	}
	for _, f := range man.Files {
		h := sha256.Sum256(files[f.Path])
		if hex.EncodeToString(h[:]) != f.SHA256 || int64(len(files[f.Path])) != f.Bytes {
			t.Fatalf("checksum of %s", f.Path)
		}
	}
	if len(m.Files) != len(man.Files) {
		t.Fatal("returned manifest differs")
	}
	items := lines(t, files["inbox_items.jsonl"])
	if len(items) != 1 || items[0]["id"] != "inb_1" || items[0]["space_id"] != "spc_a" {
		t.Fatalf("items %v (another space's item must not be exported)", items)
	}
	if tr, ok := items[0]["trusted"].(map[string]any); !ok || tr["user"] != "usr_x" {
		t.Fatalf("trusted not embedded as JSON: %v", items[0])
	}
	ha := lines(t, files["human_actions.jsonl"])
	if r, ok := ha[0]["response"].(map[string]any); !ok || r["answer"] != "sqlite" || ha[0]["outcome"] != "answered" {
		t.Fatalf("human action response: %v", ha[0])
	}
	if c := lines(t, files["human_comments.jsonl"]); len(c) != 1 || c[0]["body"] != "Why not?" {
		t.Fatalf("comments %v", c)
	}
	var n int
	if err := s.ReadDB().QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE action = 'space.exported' AND target_id = 'spc_a'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("export not audited: %d %v", n, err)
	}
}

func TestExportSizeLimit(t *testing.T) {
	svc, _ := setup(t, 2048)
	ctx := context.Background()
	if _, _, err := svc.Prepare(ctx, admin, "spc_a"); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("over the limit: %v", err)
	}
	var buf bytes.Buffer
	if _, err := svc.Write(ctx, &buf, admin, "spc_a", 300); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("stream past the limit: %v", err)
	}
	if _, _, err := svc.Prepare(ctx, admin, "spc_missing"); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("missing space: %v", err)
	}
}

func TestSnake(t *testing.T) {
	for in, want := range map[string]string{"SpaceID": "space_id", "TrustedJson": "trusted_json", "ID": "id", "SourceIp": "source_ip",
		"TypeVersionID": "type_version_id", "Sha256": "sha256", "DeferUntil": "defer_until"} {
		if got := snake(in); got != want {
			t.Errorf("snake(%s) = %s, want %s", in, got, want)
		}
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
