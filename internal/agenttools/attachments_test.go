package agenttools

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
)

// TestLargeAttachmentsAreNeverTruncatedSilently: a 2 MB image is not returned inline (and says so), its ranged reads
// add up to exactly the stored bytes, and a large text file comes back in chunks that each say what follows.
func TestLargeAttachmentsAreNeverTruncatedSilently(t *testing.T) {
	f := newFx(t)
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 2<<20)...)
	_, _ = rand.Read(png[8:])
	img, err := f.d.Attachments.Upload(f.ctx, audit.System, "spc_a", "shot.png", bytes.NewReader(png))
	must(t, err)

	var out AttachmentContent
	must(t, f.call(f.wrk, "spaceman_attachment_get", map[string]any{"attachment": img.ID}, &out))
	if !out.Truncated || out.Content != "" || out.Size != int64(len(png)) || !strings.Contains(out.Note, AttachmentSaveTool) {
		t.Fatalf("inline read of a large image: truncated=%v content=%d size=%d note=%q", out.Truncated, len(out.Content), out.Size, out.Note)
	}

	var got []byte
	var off int64
	for calls := 0; ; calls++ {
		if calls > 10 {
			t.Fatal("ranged reads do not finish")
		}
		var c AttachmentContent
		must(t, f.call(f.wrk, "spaceman_attachment_get", map[string]any{"attachment": img.ID, "offset": off, "length": 1 << 30}, &c))
		b, err := base64.StdEncoding.DecodeString(c.Content)
		must(t, err)
		if c.Encoding != "base64" || c.Offset != off || c.ReturnedBytes != len(b) || len(b) > MaxAttachmentChunk {
			t.Fatalf("chunk at %d: %+v (%d bytes)", off, c.Encoding, len(b))
		}
		got = append(got, b...)
		if !c.Truncated {
			if c.NextOffset != 0 {
				t.Fatalf("last chunk has next_offset %d", c.NextOffset)
			}
			break
		}
		if c.NextOffset != off+int64(len(b)) {
			t.Fatalf("next_offset %d after %d+%d", c.NextOffset, off, len(b))
		}
		off = c.NextOffset
	}
	sum := sha256.Sum256(got)
	if !bytes.Equal(got, png) || hex.EncodeToString(sum[:]) != img.SHA256 {
		t.Fatalf("reassembled %d of %d bytes; sha256 matches: %v", len(got), len(png), hex.EncodeToString(sum[:]) == img.SHA256)
	}

	// Text: the first chunk inline, cut on a character boundary, with where to go on.
	text := strings.Repeat("é", MaxAttachmentBytes) // 2 bytes each: 1 MiB
	doc, err := f.d.Attachments.Upload(f.ctx, audit.System, "spc_a", "log.txt", strings.NewReader("x"+text))
	must(t, err)
	must(t, f.call(f.wrk, "spaceman_attachment_get", map[string]any{"attachment": doc.ID}, &out))
	if out.Encoding != "text" || !out.Truncated || out.ReturnedBytes != MaxAttachmentBytes-1 || out.NextOffset != MaxAttachmentBytes-1 ||
		!strings.Contains(out.Note, "offset=") {
		t.Fatalf("large text: encoding=%s truncated=%v returned=%d next=%d", out.Encoding, out.Truncated, out.ReturnedBytes, out.NextOffset)
	}
	var rest AttachmentContent
	must(t, f.call(f.wrk, "spaceman_attachment_get", map[string]any{"attachment": doc.ID, "offset": out.NextOffset}, &rest))
	if rest.Encoding != "text" || rest.Truncated || out.Content+rest.Content != "x"+text {
		t.Fatalf("text rest: encoding=%s truncated=%v", rest.Encoding, rest.Truncated)
	}

	// A small file is whole and says so.
	small, err := f.d.Attachments.Upload(f.ctx, audit.System, "spc_a", "a.txt", strings.NewReader("hi"))
	must(t, err)
	var whole AttachmentContent
	must(t, f.call(f.wrk, "spaceman_attachment_get", map[string]any{"attachment": small.ID}, &whole))
	if whole.Truncated || whole.Content != "hi" || whole.ReturnedBytes != 2 || whole.NextOffset != 0 || whole.Note != "" {
		t.Fatalf("small file %+v", whole)
	}
	code(t, f.call(f.wrk, "spaceman_attachment_get", map[string]any{"attachment": small.ID, "offset": 3}, nil), apperr.CodeInvalid)
	code(t, f.call(f.wrk, "spaceman_attachment_get", map[string]any{"attachment": small.ID, "offset": -1}, nil), apperr.CodeInvalid)
}
