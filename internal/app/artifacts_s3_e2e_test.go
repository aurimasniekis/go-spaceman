package app_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/s3client/s3test"
)

// SM-STORE-002, ADR-0048 end to end with artifacts.backend: s3 (the in-process fake bucket): an attachment goes to
// the bucket; a run's recording is spooled while it is shipped and archived to the bucket once the run ends (and
// still reads the same); its transcript is uploaded once received; a backup copies them into the backup; deleting
// the recording and the transcript removes their objects.
func TestS3ArtifactStoreEndToEnd(t *testing.T) {
	srv := s3test.New(t, "AKIDTEST")
	keys := t.TempDir()
	write := func(name, v string) string {
		p := filepath.Join(keys, name)
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const bucket = "sm-e2e"
	e := newFleetEnv(t, func(c *config.Config) {
		c.Artifacts.Backend = "s3"
		c.Artifacts.S3 = config.S3Config{Endpoint: srv.URL, Bucket: bucket, Prefix: "art/", AccessKeyFile: write("ak", "AKIDTEST"),
			SecretKeyFile: write("sk", "e2e-secret"), UsePathStyle: true, AllowPrivateNetwork: true, CAFile: write("ca.pem", srv.CAPEM())}
	})
	if e.c.Artifacts.Kind() != "s3" {
		t.Fatalf("artifact store %s", e.c.Artifacts.Kind())
	}
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "s3-art", Name: "S3", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)

	att, err := e.c.Attachments.Upload(ctx, admin, sp.ID, "notes.txt", strings.NewReader("kept in the bucket"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.Object(bucket, "art/"+artifacts.AttachmentKey(att.SHA256)); !ok {
		t.Fatalf("attachment not in the bucket: %v", srv.Keys(bucket))
	}

	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "run running", func() bool { return e.run1(r.ID).Observed == "running" && e.run1(r.ID).SessionID != "" })
	run := e.run1(r.ID)
	e.run.Output(e.run.LiveByLabel("spaceman.run", r.ID)[0].ID, "archived output\r\n")
	recorded := func() string {
		res, err := e.c.Fleet.Recordings().Read(ctx, r.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, rec := range res.Records {
			b.Write(rec.Data)
		}
		return b.String()
	}
	eventually(t, "output recorded in the spool", func() bool { return strings.Contains(recorded(), "archived output") })
	transcript := []byte(`{"type":"assistant","message":{"id":"msg_1","model":"claude-haiku-5-5","usage":{"input_tokens":1,"output_tokens":2}}}` + "\n")
	p := filepath.Join(e.agentDir, "spaces", sp.Workdir, "instances", run.InstanceID, "claude", "projects", "-work", run.SessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, transcript, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.c.Fleet.StopRun(ctx, admin, r.ID, "kill", "done"); err != nil {
		t.Fatal(err)
	}
	segKeys := func() []string {
		var out []string
		for _, k := range srv.Keys(bucket) {
			if strings.HasPrefix(k, "art/"+artifacts.RecordingPrefix(r.ID)) {
				out = append(out, k)
			}
		}
		return out
	}
	spool, _ := e.c.Fleet.Recordings().Dir(r.ID)
	eventually(t, "recording archived to the bucket", func() bool {
		_, err := os.Stat(spool)
		return len(segKeys()) > 0 && os.IsNotExist(err)
	})
	if got := recorded(); !strings.Contains(got, "archived output") {
		t.Fatalf("archived recording reads %q", got)
	}
	eventually(t, "transcript stored", func() bool {
		u, err := e.c.Usage.ForRun(ctx, sp.ID, r.ID)
		return err == nil && u.Transcript != nil && u.Transcript.State == "stored"
	})
	if b, ok := srv.Object(bucket, "art/"+artifacts.TranscriptKey(r.ID)); !ok || string(b) != string(transcript) {
		t.Fatalf("transcript not in the bucket: %v", srv.Keys(bucket))
	}
	if _, err := os.Stat(filepath.Join(e.cfg.DataDir, "transcripts", r.ID+".jsonl")); !os.IsNotExist(err) {
		t.Fatalf("transcript left on the local disk: %v", err)
	}
	f, _, err := e.c.Usage.OpenTranscript(ctx, sp.ID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	_ = f.Close()
	if string(got) != string(transcript) {
		t.Fatalf("transcript from the bucket: %q", got)
	}

	// A backup holds the artifacts copied from the bucket.
	b, err := e.c.Backups.Start(ctx, admin, backup.TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	e.c.Backups.Wait()
	m, err := backup.ReadManifest(filepath.Join(e.cfg.DataDir, "backups", b.ID))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, file := range m.Files {
		kinds[file.Kind]++
	}
	if m.ArtifactStore == nil || m.ArtifactStore.Kind != "s3" || m.ArtifactStore.Objects < 3 || kinds[backup.KindAttachment] != 1 ||
		kinds[backup.KindRecording] == 0 || kinds[backup.KindTranscript] != 1 {
		t.Fatalf("backup manifest: store %+v, files %v", m.ArtifactStore, kinds)
	}

	// Deleting the recording removes its objects; purging the space removes the transcript's too.
	if err := e.c.Fleet.DeleteRecording(ctx, admin, sp.ID, r.ID, nil); err != nil {
		t.Fatal(err)
	}
	if k := segKeys(); len(k) != 0 {
		t.Fatalf("recording objects left after delete: %v", k)
	}
	if recorded() != "" {
		t.Fatal("deleted recording still reads")
	}
	s, _ := e.c.Fleet.GetSpace(ctx, sp.ID)
	if _, err := e.c.Fleet.SetSpaceState(ctx, admin, sp.ID, s.Version, "archived", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Fleet.PurgeSpace(ctx, admin, sp.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.Object(bucket, "art/"+artifacts.TranscriptKey(r.ID)); ok {
		t.Fatal("transcript object left after the purge")
	}
}

// With the local store a purge deletes the space's recordings and transcripts from the data directory too.
func TestPurgeDeletesLocalArtifacts(t *testing.T) {
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "purge-art", Name: "Purge", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "run running", func() bool { return e.run1(r.ID).Observed == "running" })
	e.run.Output(e.run.LiveByLabel("spaceman.run", r.ID)[0].ID, "to be purged\r\n")
	dir, _ := e.c.Fleet.Recordings().Dir(r.ID)
	eventually(t, "recorded", func() bool { _, err := os.Stat(dir); return err == nil })
	if _, err := e.c.Fleet.StopRun(ctx, admin, r.ID, "kill", "done"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "run ended", func() bool { return e.run1(r.ID).EndedAt != nil })
	tr := filepath.Join(e.cfg.DataDir, "transcripts", r.ID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(tr), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tr, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := e.c.Fleet.GetSpace(ctx, sp.ID)
	if _, err := e.c.Fleet.SetSpaceState(ctx, admin, sp.ID, s.Version, "archived", ""); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Fleet.PurgeSpace(ctx, admin, sp.ID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, tr} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left after the purge: %v", p, err)
		}
	}
}
