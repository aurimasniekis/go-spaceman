package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/runner/runnertest"
)

func TestSafeAttachmentName(t *testing.T) {
	long := strings.Repeat("a", 300) + ".png"
	for in, want := range map[string]string{
		"shot.png":                "shot.png",
		"../x":                    "x",
		"../../etc/passwd":        "passwd",
		"/etc/passwd":             "passwd",
		`..\..\windows\evil.exe`:  "evil.exe",
		"..":                      "attachment",
		".":                       "attachment",
		"":                        "attachment",
		"/":                       "attachment",
		".bashrc":                 "bashrc",
		"...":                     "attachment",
		"Ekrano nuotrauka ąč.png": "Ekrano_nuotrauka___.png",
		"a\x00b\nc.txt":           "a_b_c.txt",
		"x/y*?[z].md":             "y___z_.md",
		long:                      strings.Repeat("a", maxSavedName-4) + ".png",
	} {
		got := safeAttachmentName(in)
		if got != want {
			t.Errorf("safeAttachmentName(%q) = %q, want %q", in, got, want)
		}
		if strings.ContainsAny(got, `/\`) || strings.HasPrefix(got, ".") || len(got) > maxSavedName {
			t.Errorf("safeAttachmentName(%q) = %q is not a plain name", in, got)
		}
	}
}

// attachAgent is an agent with a journal and one space folder (workdir "files-abcdefgh") holding two instance folders.
func attachAgent(t *testing.T) (a *Agent, spaceID, workdir, inst, other string) {
	t.Helper()
	dir := t.TempDir()
	j, err := OpenJournal(context.Background(), filepath.Join(dir, "journal"), nil, clock.System{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	a = New(j, Options{DataDir: dir, Clock: clock.System{}})
	spaceID, workdir = ids.New(ids.Space), "files-abcdefgh"
	if err := j.putWorkdir(context.Background(), spaceID, workdir); err != nil {
		t.Fatal(err)
	}
	inst, other = ids.New(ids.Instance), ids.New(ids.Instance)
	for _, i := range []string{inst, other} {
		if err := os.MkdirAll(filepath.Join(dir, "spaces", workdir, "instances", i, "claude"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return a, spaceID, workdir, inst, other
}

// A run's file tools can write its own instance folder: a link planted as the attachments folder, or as the instance
// folder, is refused, never followed, whether it points outside the space or inside it.
func TestOpenAttachmentsRefusesLinks(t *testing.T) {
	a, _, workdir, inst, other := attachAgent(t)
	instances := filepath.Join(a.o.DataDir, "spaces", workdir, "instances")
	root, dir, err := a.openAttachments(workdir, inst, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = root.Close()
	if real, _ := filepath.EvalSymlinks(filepath.Join(instances, inst, attachmentsDir)); dir != real {
		t.Fatalf("path %q, want %q", dir, real)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{t.TempDir(), "claude", filepath.Join(instances, other, "claude")} {
		if err := os.Symlink(target, dir); err != nil {
			t.Fatal(err)
		}
		if root, _, err := a.openAttachments(workdir, inst, true); err == nil {
			_ = root.Close()
			t.Fatalf("an attachments folder linked to %q was accepted", target)
		}
		_ = os.Remove(dir)
	}
	if _, _, err := a.openAttachments(workdir, ids.New(ids.Instance), true); err == nil {
		t.Fatal("a missing instance folder was accepted")
	}
	link := ids.New(ids.Instance)
	if err := os.Symlink(other, filepath.Join(instances, link)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.openAttachments(workdir, link, true); err == nil {
		t.Fatal("a linked instance folder was accepted")
	}
	for _, bad := range [][2]string{{"../x", inst}, {workdir, "../" + inst}, {"", inst}, {workdir, ""}} {
		if _, _, err := a.openAttachments(bad[0], bad[1], true); err == nil {
			t.Fatalf("openAttachments(%q, %q) was accepted", bad[0], bad[1])
		}
	}
}

// fakeCentral serves body as attachment att in ranged reads, the way central's spaceman_attachment_get does; before
// is called before each answer.
func fakeCentral(t *testing.T, att, name string, body []byte, before func(ctx context.Context, off int64)) func(context.Context, bridgeRequest) bridgeResponse {
	sum := sha256.Sum256(body)
	return func(ctx context.Context, req bridgeRequest) bridgeResponse {
		var in struct {
			Attachment string `json:"attachment"`
			Offset     int64  `json:"offset"`
			Length     int    `json:"length"`
		}
		if req.Tool != proto.ToolAttachmentGet || json.Unmarshal(req.Args, &in) != nil || in.Attachment != att || in.Length <= 0 {
			t.Errorf("unexpected call %s %s", req.Tool, req.Args)
			return bridgeResponse{Error: "bad call", Code: "invalid"}
		}
		if before != nil {
			before(ctx, in.Offset)
		}
		if ctx.Err() != nil {
			return bridgeResponse{Error: "the node agent is shutting down", Code: "unavailable"}
		}
		end := min(in.Offset+int64(in.Length), int64(len(body)))
		b := body[in.Offset:end]
		out, _ := json.Marshal(attachmentChunk{ID: att, Name: name, Size: int64(len(body)), MIME: "image/png", SHA256: hex.EncodeToString(sum[:]),
			Encoding: "base64", Content: base64.StdEncoding.EncodeToString(b), Offset: in.Offset, ReturnedBytes: len(b)})
		return bridgeResponse{OK: true, Result: out}
	}
}

// startRun journals a running run of the instance.
func startRun(t *testing.T, a *Agent, spaceID, inst string) runRow {
	t.Helper()
	ctx := context.Background()
	r := runRow{RunID: ids.New(ids.Run), SpaceID: spaceID, InstanceID: inst, Kind: "worker", Role: "implementor", Harness: "claude"}
	r.TerminalName = RunName(r.RunID)
	if err := a.j.insertRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.State, r.Desired = proto.RunRunning, "running"
	if err := a.j.updateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	return r
}

func saveArgs(att string) bridgeRequest {
	args, _ := json.Marshal(map[string]string{"attachment": att})
	return bridgeRequest{Tool: proto.ToolAttachmentSave, Args: args}
}

// A save copies the whole file (a 2 MB PNG in several ranges) into the run's folder, 0600, under a sanitized name, and
// within one overall deadline (review S3).
func TestSaveAttachmentWholeFile(t *testing.T) {
	a, spaceID, workdir, inst, _ := attachAgent(t)
	r := startRun(t, a, spaceID, inst)
	body := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 2<<20)...)
	_, _ = rand.Read(body[8:])
	att := ids.New(ids.Attachment)
	var deadlines []time.Duration
	a.fetchChunk = fakeCentral(t, att, "../../claude/settings.json", body, func(ctx context.Context, _ int64) {
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("a save's reads have no deadline")
		}
		deadlines = append(deadlines, time.Until(d))
	})
	res := a.saveAttachment(context.Background(), r, saveArgs(att))
	if !res.OK {
		t.Fatalf("save: %s", res.Error)
	}
	var out SavedAttachment
	if err := json.Unmarshal(res.Result, &out); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(a.o.DataDir, "spaces", workdir, "instances", inst, attachmentsDir, r.RunID, att))
	if out.Path != filepath.Join(want, "settings.json") || out.Size != int64(len(body)) {
		t.Fatalf("saved %+v", out)
	}
	got, err := os.ReadFile(out.Path)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("saved bytes differ (%v)", err)
	}
	if fi, _ := os.Stat(out.Path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if len(deadlines) < 2 || deadlines[0] > saveTimeout || deadlines[0] < saveTimeout-time.Minute ||
		deadlines[len(deadlines)-1] > deadlines[0] {
		t.Fatalf("deadlines %v, want one overall deadline of %s", deadlines, saveTimeout)
	}
	a.removeRunAttachments(context.Background(), r)
	if _, err := os.Stat(filepath.Dir(want)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the run's folder is still there: %v", err)
	}
}

// Review S4: a run that ends while its save is copying leaves no file behind, whether it ends before the rename or
// after the run's folder was removed.
func TestSaveAttachmentRunEndsDuringSave(t *testing.T) {
	a, spaceID, workdir, inst, _ := attachAgent(t)
	body := make([]byte, 3*proto.AttachmentChunkBytes/2)
	_, _ = rand.Read(body)
	for _, removeFirst := range []bool{false, true} {
		r := startRun(t, a, spaceID, inst)
		att := ids.New(ids.Attachment)
		a.fetchChunk = fakeCentral(t, att, "shot.png", body, func(ctx context.Context, off int64) {
			if off == 0 {
				return
			}
			// The last range: the run ends now (the journal first, then the folder, as observe does).
			e := r
			e.State = proto.RunExited
			if err := a.j.updateRun(ctx, e); err != nil {
				t.Error(err)
			}
			if removeFirst {
				a.removeRunAttachments(ctx, e)
			}
		})
		if res := a.saveAttachment(context.Background(), r, saveArgs(att)); res.OK || !strings.Contains(res.Error, "run ended") {
			t.Fatalf("save after the run ended: %+v", res)
		}
		dir := filepath.Join(a.o.DataDir, "spaces", workdir, "instances", inst, attachmentsDir, r.RunID)
		var left []string
		_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				left = append(left, p)
			}
			return nil
		})
		if len(left) != 0 {
			t.Fatalf("files left behind: %v", left)
		}
	}
}

// Review S1: runs the node finds lost or never started when it reconciles with its runner lose their saved files too.
func TestReconcileRemovesLostRunsAttachments(t *testing.T) {
	a, spaceID, workdir, inst, _ := attachAgent(t)
	a.o.Runner = runnertest.New()
	ctx := context.Background()
	lost := startRun(t, a, spaceID, inst)
	pending := runRow{RunID: ids.New(ids.Run), SpaceID: spaceID, InstanceID: inst}
	pending.TerminalName = RunName(pending.RunID)
	if err := a.j.insertRun(ctx, pending); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(a.o.DataDir, "spaces", workdir, "instances", inst, attachmentsDir)
	for _, r := range []string{lost.RunID, pending.RunID} {
		write(t, dir, r+"/att_x/shot.png", "png")
	}
	if err := a.reconcileRunner(ctx); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{lost.RunID, pending.RunID} {
		if _, err := os.Stat(filepath.Join(dir, r)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("run %s keeps its files: %v", r, err)
		}
	}
}

// A central before ADR-0073 refuses the ranged-read arguments with its strict decoder's message.
func TestErrUnknownArgument(t *testing.T) {
	old := bridgeResponse{Code: "validation_failed", Error: `invalid arguments for spaceman_attachment_get: json: unknown field "offset"`}
	if !errUnknownArgument(old) || errUnknownArgument(bridgeResponse{Code: "not_found", Error: "attachment not found"}) {
		t.Fatal("old central not recognized")
	}
}
