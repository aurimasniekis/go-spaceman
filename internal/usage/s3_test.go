package usage

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/s3client"
	"github.com/aurimasniekis/go-spaceman/internal/s3client/s3test"
)

// SM-STORE-002, ADR-0048: transcripts with the S3 artifact store. A complete, checked transcript is uploaded and its
// .part file removed; when the bucket refuses it the transcript is kept (and served) locally and UploadSpooled moves
// it later; damaged content fails its download; deleting removes the object.
func TestTranscriptsInS3(t *testing.T) {
	base, ctx := newSvc(t)
	srv := s3test.New(t, "AKIDTEST")
	cl, err := s3client.New(s3client.Config{Endpoint: srv.URL, Bucket: "trs", PathStyle: true, AccessKeyID: "AKIDTEST", SecretAccessKey: "secret",
		CABundlePEM: srv.CAPEM(), AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	svc := New(base.s, base.audit, base.changes, dir, artifacts.NewS3(cl, "trs", ""), nil)
	data := []byte(`{"line":1}` + "\n")
	size := int64(len(data))
	if ack := svc.Chunk(ctx, "nod_1", proto.TranscriptChunk{RunID: runA, Data: data, Size: size, Final: true, SHA256: sum(data)}); !ack.Done || ack.Error != "" {
		t.Fatalf("final: %+v", ack)
	}
	if b, ok := srv.Object("trs", artifacts.TranscriptKey(runA)); !ok || string(b) != string(data) {
		t.Fatalf("bucket %v", srv.Keys("trs"))
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Fatalf("local files left: %d", len(ents))
	}
	read := func(run string) ([]byte, error) {
		o, _, err := svc.OpenTranscript(ctx, "spc_a", run)
		if err != nil {
			return nil, err
		}
		defer o.Close()
		return io.ReadAll(o)
	}
	if got, err := read(runA); err != nil || string(got) != string(data) {
		t.Fatalf("read %q %v", got, err)
	}
	srv.SetObject("trs", artifacts.TranscriptKey(runA), []byte(`{"line":2}`+"\n"))
	if _, err := read(runA); !errors.Is(err, artifacts.ErrChecksum) {
		t.Fatalf("damaged transcript: %v", err)
	}

	// The bucket is down: the transcript is stored locally, served from there, and uploaded later.
	srv.Fail = func(*http.Request) int { return http.StatusServiceUnavailable }
	if ack := svc.Chunk(ctx, "nod_1", proto.TranscriptChunk{RunID: runB, Data: data, Size: size, Final: true, SHA256: sum(data)}); !ack.Done || ack.Error != "" {
		t.Fatalf("final while the bucket is down: %+v", ack)
	}
	if _, err := os.Stat(filepath.Join(dir, runB+".jsonl")); err != nil {
		t.Fatalf("kept locally: %v", err)
	}
	if got, err := read(runB); err != nil || string(got) != string(data) {
		t.Fatalf("local read %q %v", got, err)
	}
	if n, err := svc.UploadSpooled(ctx); err == nil || n != 0 {
		t.Fatalf("upload while down: %d %v", n, err)
	}
	srv.Fail = nil
	if n, err := svc.UploadSpooled(ctx); err != nil || n != 1 {
		t.Fatalf("upload spooled: %d %v", n, err)
	}
	if _, ok := srv.Object("trs", artifacts.TranscriptKey(runB)); !ok {
		t.Fatal("spooled transcript not uploaded")
	}
	if got, err := read(runB); err != nil || string(got) != string(data) {
		t.Fatalf("read after upload %q %v", got, err)
	}
	if err := svc.DeleteTranscript(ctx, audit.Actor{Type: audit.ActorUser, ID: "usr_a"}, "spc_a", runB); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.Object("trs", artifacts.TranscriptKey(runB)); ok {
		t.Fatal("deleted transcript left in the bucket")
	}
	if _, err := read(runB); err == nil {
		t.Fatal("deleted transcript read")
	}
	// The local store has nothing to upload.
	if n, err := base.UploadSpooled(ctx); n != 0 || err != nil {
		t.Fatalf("local upload spooled %d %v", n, err)
	}
}
