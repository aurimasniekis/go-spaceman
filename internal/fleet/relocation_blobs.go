package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Relocation files on central (ADR-0037): <data>/relocations/<relocation id>/<name>. The source uploads exported state
// while the relocation is exporting; central generates memory bundles before preparing; the destination reads both
// while it prepares. A file is complete only once its size and SHA-256 match (a sidecar <name>.sha256 records the
// digest); a partial upload is <name>.part and resumes at its current size.

func fileDigest(path string) (int64, string, error) {
	f, err := os.Open(path) //nolint:gosec // central's own relocation folder; names are validated
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func dirBytes(dir string) int64 {
	var total int64
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() {
			total += fi.Size()
		}
	}
	return total
}

// relocationUpload stores one chunk of an exported file from the relocation's source.
func (svc *Service) relocationUpload(ctx context.Context, nodeID string, up proto.RelocationUpload) proto.RelocationUploadAck {
	ack := proto.RelocationUploadAck{RelocationID: up.RelocationID, Name: up.Name}
	fail := func(msg string) proto.RelocationUploadAck { ack.Error = msg; return ack }
	if !proto.ValidBlobName(up.Name) || strings.HasSuffix(up.Name, ".part") || strings.HasSuffix(up.Name, ".sha256") ||
		strings.HasPrefix(up.Name, "memory-") || len(up.Data) > proto.RelocationChunkMax || up.Offset < 0 {
		return fail("invalid upload")
	}
	r, err := svc.s.R().GetRelocation(ctx, up.RelocationID)
	if err != nil || r.State != RelocExporting || r.SourceNodeID != nodeID {
		return fail("no export of this relocation is in progress for this node")
	}
	dir := svc.relocDir(r.ID)
	if dir == "" {
		return fail("central does not store relocation files")
	}
	defer svc.relocLock("upload:" + r.ID)()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail("central could not store the file")
	}
	final := filepath.Join(dir, up.Name)
	part := final + ".part"
	if fi, err := os.Stat(final); err == nil {
		ack.Next, ack.Done = fi.Size(), true
		return ack
	}
	var size int64
	if fi, err := os.Stat(part); err == nil {
		size = fi.Size()
	}
	if up.Offset != size {
		ack.Next = size // resume where central is
		return ack
	}
	if dirBytes(dir)+int64(len(up.Data)) > proto.RelocationExportMax {
		_ = os.Remove(part)
		return fail("the export is larger than central accepts for one relocation")
	}
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // validated name in central's folder
	if err != nil {
		return fail("central could not store the file")
	}
	_, werr := f.Write(up.Data)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		return fail("central could not store the file")
	}
	size += int64(len(up.Data))
	ack.Next = size
	if !up.Final {
		return ack
	}
	n, sum, err := fileDigest(part)
	if err != nil || n != up.Size || sum != up.SHA256 {
		_ = os.Remove(part)
		ack.Next = 0
		return fail("the file does not match its size and SHA-256; upload it again")
	}
	if err := os.WriteFile(final+".sha256", []byte(sum), 0o600); err != nil {
		return fail("central could not store the file")
	}
	if err := os.Rename(part, final); err != nil {
		return fail("central could not store the file")
	}
	ack.Done = true
	return ack
}

// relocationFetch answers the destination's request for part of a relocation file.
func (svc *Service) relocationFetch(ctx context.Context, nodeID string, f proto.RelocationFetch) proto.RelocationData {
	out := proto.RelocationData{RelocationID: f.RelocationID, Name: f.Name, Offset: f.Offset, Data: []byte{}}
	fail := func(msg string) proto.RelocationData { out.Error = msg; return out }
	if !proto.ValidBlobName(f.Name) || strings.HasSuffix(f.Name, ".part") || strings.HasSuffix(f.Name, ".sha256") || f.Offset < 0 {
		return fail("invalid file name")
	}
	r, err := svc.s.R().GetRelocation(ctx, f.RelocationID)
	if err != nil || r.State != RelocPreparing || r.DestNodeID != nodeID {
		return fail("no preparation of this relocation is in progress for this node")
	}
	dir := svc.relocDir(r.ID)
	if dir == "" {
		return fail("central does not store relocation files")
	}
	path := filepath.Join(dir, f.Name)
	sum, err := os.ReadFile(path + ".sha256") //nolint:gosec // validated name in central's folder
	if err != nil {
		return fail("no such relocation file")
	}
	file, err := os.Open(path) //nolint:gosec // validated name in central's folder
	if err != nil {
		return fail("no such relocation file")
	}
	defer file.Close()
	fi, err := file.Stat()
	if err != nil {
		return fail("no such relocation file")
	}
	out.Size, out.SHA256 = fi.Size(), string(sum)
	if f.Offset > fi.Size() {
		return fail("offset beyond the end of the file")
	}
	buf := make([]byte, min(int64(proto.RelocationChunkMax), fi.Size()-f.Offset))
	n, err := file.ReadAt(buf, f.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return fail("central could not read the file")
	}
	out.Data = buf[:n]
	out.Final = f.Offset+int64(n) >= fi.Size()
	return out
}
