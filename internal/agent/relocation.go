package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/memory"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// Space relocation on the node (ADR-0037). The node executes central's relocation steps and reports their outcome; it
// never moves a space on its own. Steps are idempotent: a repeated delivery re-sends the stored outcome, or runs the
// step again when the agent restarted before finishing it.

// relocState tracks relocation steps in progress and transfers waiting for central.
type relocState struct {
	mu      sync.Mutex
	running map[string]bool        // command ID → step in progress
	waiting map[string]chan []byte // transfer key → central's answer (JSON)
}

const (
	relocAnswerTimeout = 2 * time.Minute
	relocResultsKeep   = 7 * 24 * time.Hour
	markerFile         = ".spaceman-relocation"
)

func (a *Agent) relocWaiter(key string) (chan []byte, func()) {
	a.reloc.mu.Lock()
	defer a.reloc.mu.Unlock()
	if a.reloc.waiting == nil {
		a.reloc.waiting = map[string]chan []byte{}
	}
	ch := make(chan []byte, 1)
	a.reloc.waiting[key] = ch
	return ch, func() {
		a.reloc.mu.Lock()
		if a.reloc.waiting[key] == ch {
			delete(a.reloc.waiting, key)
		}
		a.reloc.mu.Unlock()
	}
}

// relocAnswer delivers central's answer to a waiting transfer.
func (a *Agent) relocAnswer(key string, body []byte) {
	a.reloc.mu.Lock()
	ch := a.reloc.waiting[key]
	a.reloc.mu.Unlock()
	if ch != nil {
		select {
		case ch <- body:
		default:
		}
	}
}

func uploadKey(rid, name string) string { return "u:" + rid + "/" + name }
func fetchKey(rid, name string, off int64) string {
	return fmt.Sprintf("f:%s/%s@%d", rid, name, off)
}

// handleRelocation runs a relocation step. Fresh commands are acknowledged at once; the outcome follows as a report.
func (a *Agent) handleRelocation(ctx context.Context, c proto.Command, fresh bool) {
	if fresh {
		a.ack(ctx, c, proto.AckAccepted, "")
	}
	if !fresh {
		if rep, ok := a.j.relocationResult(ctx, c.CommandID); ok {
			a.sendRelocationReport(ctx, rep)
			return
		}
	}
	a.reloc.mu.Lock()
	if a.reloc.running == nil {
		a.reloc.running = map[string]bool{}
	}
	if a.reloc.running[c.CommandID] {
		a.reloc.mu.Unlock()
		return
	}
	a.reloc.running[c.CommandID] = true
	a.reloc.mu.Unlock()
	bctx := context.WithoutCancel(ctx)
	a.goTrack(func() {
		defer func() {
			a.reloc.mu.Lock()
			delete(a.reloc.running, c.CommandID)
			a.reloc.mu.Unlock()
		}()
		rep := a.runRelocation(bctx, c)
		rep.CommandID, rep.Kind = c.CommandID, c.Kind
		if err := a.j.putRelocationResult(bctx, rep); err != nil {
			a.log.Warn("journal relocation result", obs.Err(err))
		}
		a.j.commandDone(bctx, c.CommandID, "done", "")
		a.sendRelocationReport(bctx, rep)
	})
}

func (a *Agent) sendRelocationReport(ctx context.Context, rep proto.RelocationReport) {
	if err := a.send(ctx, proto.TypeRelocationReport, rep); err != nil {
		a.log.Debug("relocation report deferred until reconnect", slog.String("relocation.id", rep.RelocationID))
	}
}

// resendRelocationReports re-sends recent outcomes after connecting; central applies only the one it waits for.
func (a *Agent) resendRelocationReports(ctx context.Context) {
	if !a.protocolAtLeast(proto.MinorRelocation) {
		return
	}
	reps, err := a.j.recentRelocationResults(ctx, a.now().Add(-relocResultsKeep))
	if err != nil {
		return
	}
	for _, r := range reps {
		a.sendRelocationReport(ctx, r)
	}
}

func (a *Agent) runRelocation(ctx context.Context, c proto.Command) proto.RelocationReport {
	var head struct {
		RelocationID string `json:"relocation_id"`
		Workdir      string `json:"workdir"`
	}
	_ = json.Unmarshal(c.Payload, &head)
	rep := proto.RelocationReport{RelocationID: head.RelocationID}
	fail := func(err error) proto.RelocationReport {
		rep.OK, rep.Error = false, truncateStr(err.Error(), 2000)
		a.log.Warn("relocation step failed", slog.String("relocation.id", head.RelocationID), slog.String("kind", c.Kind), obs.Err(err))
		return rep
	}
	if !strings.HasPrefix(head.RelocationID, "rlc_") || !safeName(strings.ReplaceAll(head.RelocationID, "_", "-")) {
		return fail(errors.New("invalid relocation id"))
	}
	if !validWorkdir(head.Workdir) {
		return fail(errors.New("invalid space folder name"))
	}
	_ = a.j.putWorkdir(ctx, c.SpaceID, head.Workdir)
	g, err := newProjectGit()
	if err != nil && c.Kind != proto.CmdRelocateRelease {
		return fail(err)
	}
	switch c.Kind {
	case proto.CmdRelocateCheck:
		var p proto.RelocateCheckPayload
		if err := json.Unmarshal(c.Payload, &p); err != nil {
			return fail(err)
		}
		rep.Check = a.relocCheck(ctx, g, c.SpaceID, p)
		rep.OK = true
	case proto.CmdRelocateExport:
		var p proto.RelocateExportPayload
		if err := json.Unmarshal(c.Payload, &p); err != nil {
			return fail(err)
		}
		ex, err := a.relocExport(ctx, g, c.SpaceID, p)
		if err != nil {
			return fail(err)
		}
		rep.Export, rep.OK = ex, true
	case proto.CmdRelocatePrepare:
		var p proto.RelocatePreparePayload
		if err := json.Unmarshal(c.Payload, &p); err != nil {
			return fail(err)
		}
		res, err := a.relocPrepare(ctx, g, c.SpaceID, p)
		if err != nil {
			return fail(err)
		}
		rep.Prepare, rep.OK = res, true
	case proto.CmdRelocateRelease:
		var p proto.RelocateReleasePayload
		if err := json.Unmarshal(c.Payload, &p); err != nil {
			return fail(err)
		}
		res, err := a.relocRelease(ctx, c.SpaceID, p)
		if err != nil {
			return fail(err)
		}
		rep.Release, rep.OK = res, true
	default:
		return fail(fmt.Errorf("unknown relocation step %q", c.Kind))
	}
	return rep
}

// validWorkdir accepts a space folder name as central creates it (<slug>-<8 id characters>).
func validWorkdir(w string) bool {
	return w != "" && len(w) <= 64 && safeName(w)
}

func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// relocCheck flushes what central must hold (memory, recordings, transcripts) and inspects the space folder.
func (a *Agent) relocCheck(ctx context.Context, g *projectGit, spaceID string, p proto.RelocateCheckPayload) *proto.SpaceCheck {
	c := &proto.SpaceCheck{Repositories: []proto.RepoCheck{}, Memory: []proto.MemoryCollectionStatus{}, Recordings: []string{}, Transcripts: []string{},
		ActiveRuns: []string{}, Instances: []proto.InstanceCheck{}, Errors: []string{}}
	space := a.spaceDir(p.Workdir)
	if p.Flush {
		wait := time.Duration(p.SyncWait) * time.Second
		if wait <= 0 || wait > 10*time.Minute {
			wait = time.Minute
		}
		c.Flushed = a.relocFlush(ctx, spaceID, wait)
	}
	c.Memory = a.spaceMemory(ctx, spaceID)
	c.Recordings, c.Transcripts = a.pendingUploadsOf(ctx, spaceID)
	if runs, err := a.j.runs(ctx, true); err == nil {
		for _, r := range runs {
			if r.SpaceID == spaceID {
				c.ActiveRuns = append(c.ActiveRuns, r.RunID)
			}
		}
	}
	if _, err := os.Stat(space); err == nil {
		c.FolderExists = true
		c.Repositories, c.Loose, c.Instances, c.Errors = g.inspectSpace(ctx, space)
	}
	c.CheckedAt = a.now()
	return c
}

// relocFlush checkpoints and syncs the space's role memory, re-ships recordings and resumes transcript uploads, then
// waits (bounded) until central acknowledged all of it.
func (a *Agent) relocFlush(ctx context.Context, spaceID string, wait time.Duration) bool {
	a.flushSpaceMemory(ctx, spaceID, "relocation")
	go a.catchUpRecordings(ctx)
	if ids, err := a.j.pendingUploads(ctx); err == nil {
		for _, id := range ids {
			if r, err := a.j.getRun(ctx, id); err == nil && r.SpaceID == spaceID {
				a.sendTranscriptChunk(ctx, id)
			}
		}
	}
	deadline := time.Now().Add(wait)
	for {
		clean, _ := a.memoryClean(ctx, spaceID)
		if recs, trs := a.pendingUploadsOf(ctx, spaceID); len(recs) > 0 || len(trs) > 0 {
			clean = false
		}
		if clean {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(flushPoll):
		}
	}
}

func (a *Agent) spaceMemory(ctx context.Context, spaceID string) []proto.MemoryCollectionStatus {
	out := []proto.MemoryCollectionStatus{}
	if a.mem.git == nil {
		return out
	}
	for _, k := range a.mem.collections() {
		if k[0] == spaceID {
			out = append(out, a.mem.statusOf(ctx, k[0], k[1]))
		}
	}
	return out
}

// pendingUploadsOf lists the space's runs whose recording central has not fully acknowledged and whose transcript upload
// is unfinished.
func (a *Agent) pendingUploadsOf(ctx context.Context, spaceID string) (recs, trs []string) {
	recs, trs = []string{}, []string{}
	runs, err := a.j.runs(ctx, false)
	if err != nil {
		return recs, trs
	}
	inSpace := map[string]bool{}
	for _, r := range runs {
		if r.SpaceID != spaceID {
			continue
		}
		inSpace[r.RunID] = true
		if _, err := os.Stat(a.RecordingDir(r.RunID)); err != nil {
			continue
		}
		a.mu.Lock()
		acked, known := a.acked[r.RunID]
		a.mu.Unlock()
		res, err := recording.Read(a.RecordingDir(r.RunID), acked)
		if err == nil && (len(res.Records) > 0 || !known && hasSegments(a.RecordingDir(r.RunID))) {
			recs = append(recs, r.RunID)
		}
	}
	if ids, err := a.j.pendingUploads(ctx); err == nil {
		for _, id := range ids {
			if inSpace[id] {
				trs = append(trs, id)
			}
		}
	}
	return recs, trs
}

func hasSegments(dir string) bool {
	segs, err := recording.Segments(dir)
	return err == nil && len(segs) > 0
}

// relocExport bundles the listed repositories and the loose files, uploads them to central and returns the manifest.
func (a *Agent) relocExport(ctx context.Context, g *projectGit, spaceID string, p proto.RelocateExportPayload) (*proto.RelocateExport, error) {
	space := a.spaceDir(p.Workdir)
	if _, err := os.Stat(space); err != nil {
		return nil, fmt.Errorf("the space folder is missing: %w", err)
	}
	work := filepath.Join(a.o.DataDir, "relocations", p.RelocationID, "export")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(filepath.Join(a.o.DataDir, "relocations", p.RelocationID)) }()
	maxBytes := p.MaxBytes
	if maxBytes <= 0 || maxBytes > proto.RelocationExportMax {
		maxBytes = proto.RelocationExportMax
	}
	ex := &proto.RelocateExport{Repos: []proto.ExportedRepo{}, Blobs: []proto.RelocateBlob{}}
	var total int64
	upload := func(name string) error {
		b, err := a.relocUpload(ctx, p.RelocationID, name, filepath.Join(work, name))
		if err != nil {
			return fmt.Errorf("upload %s: %w", name, err)
		}
		total += b.Size
		if total > maxBytes {
			return fmt.Errorf("the export is larger than %d bytes", maxBytes)
		}
		ex.Blobs = append(ex.Blobs, b)
		return nil
	}
	for i, rel := range p.Repos {
		if !safeRel(rel) {
			return nil, fmt.Errorf("invalid repository path %q", rel)
		}
		name := fmt.Sprintf("repo-%d.bundle", i)
		r, err := g.exportRepo(ctx, space, rel, filepath.Join(work, name))
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", rel, err)
		}
		if r.Blob != "" {
			if err := upload(name); err != nil {
				return nil, err
			}
		}
		ex.Repos = append(ex.Repos, r)
	}
	if p.Loose {
		l, err := walkSpace(space)
		if err != nil {
			return nil, err
		}
		if len(l.loose) > 0 {
			if err := writeLoose(space, l.loose, filepath.Join(work, "loose.tar.gz"), maxBytes); err != nil {
				return nil, fmt.Errorf("archive loose files: %w", err)
			}
			if err := upload("loose.tar.gz"); err != nil {
				return nil, err
			}
			ex.Loose, ex.LooseFiles = "loose.tar.gz", len(l.loose)
		}
	}
	return ex, nil
}

func safeRel(rel string) bool {
	c := filepath.Clean(filepath.FromSlash(rel))
	return rel != "" && c == filepath.FromSlash(rel) && !filepath.IsAbs(c) && c != ".." && !strings.HasPrefix(c, ".."+string(filepath.Separator)) &&
		!underReserved(c)
}

// reservedDirs are the space folder's top-level folders that hold the harnesses' own state (native configuration and
// transcripts per instance): nothing a relocation carries may be placed in them or replace them, whatever its type.
var reservedDirs = []string{"instances"}

// underReserved reports whether a cleaned relative path is a reserved folder or inside one (extra adds more names).
func underReserved(c string, extra ...string) bool {
	for _, d := range append(append([]string{}, reservedDirs...), extra...) {
		if c == d || strings.HasPrefix(c, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func digest(path string) (int64, string, error) {
	f, err := os.Open(path) //nolint:gosec // the agent's own relocation files
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return n, hex.EncodeToString(h.Sum(nil)), err
}

// relocUpload sends a file to central in chunks, stop-and-wait, resuming wherever central says it is.
func (a *Agent) relocUpload(ctx context.Context, rid, name, path string) (proto.RelocateBlob, error) {
	size, sum, err := digest(path)
	if err != nil {
		return proto.RelocateBlob{}, err
	}
	f, err := os.Open(path) //nolint:gosec // the agent's own relocation files
	if err != nil {
		return proto.RelocateBlob{}, err
	}
	defer f.Close()
	ch, done := a.relocWaiter(uploadKey(rid, name))
	defer done()
	var off int64
	buf := make([]byte, proto.RelocationChunkMax)
	for range 100000 {
		n, err := f.ReadAt(buf, off)
		if err != nil && !errors.Is(err, io.EOF) {
			return proto.RelocateBlob{}, err
		}
		up := proto.RelocationUpload{RelocationID: rid, Name: name, Offset: off, Data: buf[:n], Final: off+int64(n) >= size}
		if up.Final {
			up.Size, up.SHA256 = size, sum
		}
		if err := a.send(ctx, proto.TypeRelocationUpload, up); err != nil {
			return proto.RelocateBlob{}, err
		}
		var ack proto.RelocationUploadAck
		select {
		case b := <-ch:
			if err := json.Unmarshal(b, &ack); err != nil {
				return proto.RelocateBlob{}, err
			}
		case <-time.After(relocAnswerTimeout):
			return proto.RelocateBlob{}, errors.New("central did not acknowledge the upload")
		case <-ctx.Done():
			return proto.RelocateBlob{}, ctx.Err()
		}
		switch {
		case ack.Error != "":
			return proto.RelocateBlob{}, errors.New(ack.Error)
		case ack.Done:
			return proto.RelocateBlob{Name: name, Size: size, SHA256: sum}, nil
		}
		off = ack.Next
	}
	return proto.RelocateBlob{}, errors.New("the upload did not finish")
}

// relocFetch downloads a relocation file from central into dst and checks its size and digest.
func (a *Agent) relocFetch(ctx context.Context, rid string, b proto.RelocateBlob, dst string) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // the agent's own relocation files
	if err != nil {
		return err
	}
	defer f.Close()
	var off int64
	for range 100000 {
		ch, done := a.relocWaiter(fetchKey(rid, b.Name, off))
		err := a.send(ctx, proto.TypeRelocationFetch, proto.RelocationFetch{RelocationID: rid, Name: b.Name, Offset: off})
		if err != nil {
			done()
			return err
		}
		var d proto.RelocationData
		select {
		case body := <-ch:
			err = json.Unmarshal(body, &d)
		case <-time.After(relocAnswerTimeout):
			err = errors.New("central did not answer")
		case <-ctx.Done():
			err = ctx.Err()
		}
		done()
		if err != nil {
			return err
		}
		if d.Error != "" {
			return errors.New(d.Error)
		}
		if _, err := f.WriteAt(d.Data, off); err != nil {
			return err
		}
		off += int64(len(d.Data))
		if d.Final {
			break
		}
		if len(d.Data) == 0 {
			return errors.New("central sent an empty chunk")
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	size, sum, err := digest(dst)
	if err != nil {
		return err
	}
	if size != b.Size || sum != b.SHA256 {
		return fmt.Errorf("%s does not match its size and SHA-256", b.Name)
	}
	return nil
}

// relocPrepare materializes the space folder on the destination: an existing folder of the space is moved aside
// (never deleted), repositories are cloned or restored, role memory is restored at central's heads, loose files are
// unpacked, and the result is verified.
func (a *Agent) relocPrepare(ctx context.Context, g *projectGit, spaceID string, p proto.RelocatePreparePayload) (*proto.PrepareResult, error) {
	a.mu.Lock()
	_, holds := a.leases[spaceID]
	a.mu.Unlock()
	if holds {
		return nil, errors.New("this node holds the space's lease; it cannot be a relocation destination for it")
	}
	space := a.spaceDir(p.Workdir)
	memDir := filepath.Join(a.mem.root, spaceID)
	res := &proto.PrepareResult{Repos: []proto.PreparedRepo{}, Memory: []proto.PreparedMemory{}}
	for _, dir := range []string{space, memDir} {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, markerFile)); err == nil && strings.TrimSpace(string(b)) == p.RelocationID { //nolint:gosec // the agent's own folder
			// Our own unfinished attempt at this relocation: start it over.
			if err := os.RemoveAll(dir); err != nil {
				return nil, err
			}
			continue
		}
		name, err := a.moveAside(dir, p.Workdir+"-"+shortID(p.RelocationID)+"-previous-"+filepath.Base(filepath.Dir(dir)))
		if err != nil {
			return nil, fmt.Errorf("move the existing folder aside: %w", err)
		}
		res.MovedAside = append(res.MovedAside, name)
	}
	// A marker identifies this relocation's own partial copy, so a retry starts over instead of moving it aside.
	for _, dir := range []string{space, memDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, markerFile), []byte(p.RelocationID), 0o600); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Join(space, "work"), 0o700); err != nil {
		return nil, err
	}
	files := filepath.Join(a.o.DataDir, "relocations", p.RelocationID, "files")
	if err := os.MkdirAll(files, 0o700); err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(filepath.Join(a.o.DataDir, "relocations", p.RelocationID)) }()
	blobs := map[string]string{}
	for _, b := range p.Blobs {
		if !proto.ValidBlobName(b.Name) {
			return nil, fmt.Errorf("invalid relocation file %q", b.Name)
		}
		dst := filepath.Join(files, b.Name)
		if err := a.relocFetch(ctx, p.RelocationID, b, dst); err != nil {
			return nil, fmt.Errorf("fetch %s: %w", b.Name, err)
		}
		blobs[b.Name] = dst
	}
	for _, m := range p.Memory {
		head, err := a.restoreMemory(ctx, spaceID, m, blobs[m.Blob])
		if err != nil {
			return nil, fmt.Errorf("restore the %s memory: %w", m.Role, err)
		}
		res.Memory = append(res.Memory, proto.PreparedMemory{Role: m.Role, Head: head})
	}
	for _, rr := range p.Repos {
		if !safeRel(rr.Path) {
			return nil, fmt.Errorf("invalid repository path %q", rr.Path)
		}
		pr, err := g.cloneRepo(ctx, space, rr)
		if err != nil {
			return nil, err
		}
		res.Repos = append(res.Repos, pr)
	}
	if p.Export != nil {
		for _, ex := range p.Export.Repos {
			if !safeRel(ex.Path) {
				return nil, fmt.Errorf("invalid repository path %q", ex.Path)
			}
			bundle := ""
			if ex.Blob != "" {
				if bundle = blobs[ex.Blob]; bundle == "" {
					return nil, fmt.Errorf("the bundle of %s was not sent", ex.Path)
				}
			}
			pr, err := g.restoreRepo(ctx, space, ex, bundle)
			if err != nil {
				return nil, err
			}
			res.Repos = append(res.Repos, pr)
		}
		if p.Export.Loose != "" {
			n, err := readLoose(space, blobs[p.Export.Loose], proto.RelocationExportMax)
			if err != nil {
				return nil, fmt.Errorf("unpack loose files: %w", err)
			}
			res.LooseFiles = n
		}
	}
	_ = os.Remove(filepath.Join(space, markerFile))
	_ = os.Remove(filepath.Join(memDir, markerFile))
	return res, nil
}

// restoreMemory makes the collection's shared history central's acknowledged history and records it as acknowledged.
func (a *Agent) restoreMemory(ctx context.Context, spaceID string, m proto.RelocateMemory, bundle string) (string, error) {
	if a.mem.git == nil {
		return "", errors.New("git is not installed on this node")
	}
	if !memory.ReRole.MatchString(m.Role) || !memory.ValidCommit(m.Head) || bundle == "" {
		return "", errors.New("invalid memory collection")
	}
	defer a.mem.lock(colKey(spaceID, m.Role))()
	shared := a.mem.shared(spaceID, m.Role)
	if err := a.mem.git.InitBare(ctx, shared); err != nil {
		return "", err
	}
	if _, err := a.mem.git.Run(ctx, shared, nil, "bundle", "verify", bundle); err != nil {
		return "", fmt.Errorf("invalid bundle: %w", err)
	}
	if _, err := a.mem.git.Run(ctx, shared, nil, "fetch", "--quiet", "--no-tags", bundle, "+refs/relocation/main:refs/heads/main"); err != nil {
		return "", err
	}
	head := a.mem.git.RevParse(ctx, shared, "refs/heads/main")
	if head != m.Head {
		return head, errors.New("the restored history does not end at central's head")
	}
	now := a.now()
	if err := a.j.putMemory(ctx, memRow{SpaceID: spaceID, Role: m.Role, Acked: head, LastAckAt: &now}); err != nil {
		return head, err
	}
	return head, nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[len(id)-8:]
	}
	return id
}

// moveAside renames a folder into <data>/retained/<name> (never deleting anything) and returns the name used.
func (a *Agent) moveAside(dir, name string) (string, error) {
	root := filepath.Join(a.o.DataDir, "retained")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(root, name)
	for i := 1; exists(dst); i++ {
		dst = filepath.Join(root, fmt.Sprintf("%s-%d", name, i))
	}
	return filepath.Base(dst), os.Rename(dir, dst)
}

// relocRelease handles the source's folder after a finished relocation (retain or delete) and the destination's
// prepared copy after a cancelled one (discard). The node first makes sure none of the space's runs still runs here.
func (a *Agent) relocRelease(ctx context.Context, spaceID string, p proto.RelocateReleasePayload) (*proto.ReleaseResult, error) {
	// A node that comes back after a relocation learns that its lease is gone right after connecting.
	for i := 0; ; i++ {
		a.mu.Lock()
		_, holds := a.leases[spaceID]
		a.mu.Unlock()
		if !holds {
			break
		}
		if i == 50 {
			return nil, errors.New("this node holds the space's lease; its folder is in use")
		}
		time.Sleep(100 * time.Millisecond)
	}
	a.stopSpaceRuns(ctx, spaceID, func(runRow) bool { return true }, "kill", "the space was relocated to another node")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		active := false
		if runs, err := a.j.runs(ctx, true); err == nil {
			for _, r := range runs {
				if r.SpaceID == spaceID {
					active = true
				}
			}
		}
		if !active {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	space := a.spaceDir(p.Workdir)
	memDir := filepath.Join(a.mem.root, spaceID)
	autoMem := filepath.Join(a.o.DataDir, "automemory", spaceID) // the instances' Claude Code auto memory (ADR-0068)
	retained := p.Workdir + "-" + shortID(p.RelocationID)
	res := &proto.ReleaseResult{Action: p.Action}
	switch p.Action {
	case proto.ReleaseRetain:
		root := filepath.Join(a.o.DataDir, "retained", retained)
		if err := os.MkdirAll(root, 0o700); err != nil {
			return nil, err
		}
		for _, mv := range [][2]string{{space, filepath.Join(root, "space")}, {memDir, filepath.Join(root, "memory")}, {autoMem, filepath.Join(root, "automemory")}} {
			if exists(mv[0]) && !exists(mv[1]) {
				if err := os.Rename(mv[0], mv[1]); err != nil {
					return nil, err
				}
			}
		}
		note := fmt.Sprintf("# Retained after relocation %s\n\nThe space %s moved to another node on %s. This is its folder, role memory and auto memory as they were "+
			"here. SpaceMan deletes it when the retention ends or when an operator releases it; until then nothing uses it.\n", p.RelocationID, spaceID,
			a.now().Format(time.RFC3339))
		_ = os.WriteFile(filepath.Join(root, "RETAINED.md"), []byte(note), 0o600)
		res.RetainedAs = retained
	case proto.ReleaseDelete:
		for _, dir := range []string{filepath.Join(a.o.DataDir, "retained", retained), space, memDir, autoMem} {
			if err := os.RemoveAll(dir); err != nil {
				return nil, err
			}
		}
	case proto.ReleaseDiscard:
		for _, dir := range []string{space, memDir, autoMem} {
			if err := os.RemoveAll(dir); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("unknown release action %q", p.Action)
	}
	if err := a.j.forgetSpaceMemory(ctx, spaceID); err != nil {
		return nil, err
	}
	return res, nil
}

// workdirOf is the folder name of a space this node holds or held.
func (a *Agent) workdirOf(ctx context.Context, spaceID string) string {
	a.mu.Lock()
	l, ok := a.leases[spaceID]
	a.mu.Unlock()
	if ok && l.Workdir != "" {
		return l.Workdir
	}
	return a.j.workdir(ctx, spaceID)
}

// Journal: relocation outcomes and space folders.

func (j *Journal) putRelocationResult(ctx context.Context, rep proto.RelocationReport) error {
	b, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	return j.s.W().TxNamed(ctx, "journal.relocation_result", func(tx *store.Tx) error {
		if _, err := tx.SQL.ExecContext(ctx, `DELETE FROM relocation_results WHERE created_at < ?`, tx.Now.Add(-relocResultsKeep).UnixMicro()); err != nil {
			return err
		}
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO relocation_results (command_id, relocation_id, kind, report_json, created_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (command_id) DO UPDATE SET report_json = excluded.report_json, created_at = excluded.created_at`,
			rep.CommandID, rep.RelocationID, rep.Kind, string(b), tx.NowMicros())
		return err
	})
}

func (j *Journal) relocationResult(ctx context.Context, commandID string) (proto.RelocationReport, bool) {
	var s string
	if err := j.s.ReadDB().QueryRowContext(ctx, `SELECT report_json FROM relocation_results WHERE command_id = ?`, commandID).Scan(&s); err != nil {
		return proto.RelocationReport{}, false
	}
	var rep proto.RelocationReport
	return rep, json.Unmarshal([]byte(s), &rep) == nil
}

func (j *Journal) recentRelocationResults(ctx context.Context, since time.Time) ([]proto.RelocationReport, error) {
	rows, err := j.s.ReadDB().QueryContext(ctx, `SELECT report_json FROM relocation_results WHERE created_at >= ? ORDER BY created_at`, since.UnixMicro())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []proto.RelocationReport
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		var rep proto.RelocationReport
		if json.Unmarshal([]byte(s), &rep) == nil {
			out = append(out, rep)
		}
	}
	return out, rows.Err()
}

func (j *Journal) putWorkdir(ctx context.Context, spaceID, workdir string) error {
	return j.s.W().TxNamed(ctx, "journal.workdir", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `INSERT INTO space_workdirs (space_id, workdir, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (space_id) DO UPDATE SET workdir = excluded.workdir, updated_at = excluded.updated_at`, spaceID, workdir, tx.NowMicros())
		return err
	})
}

func (j *Journal) workdir(ctx context.Context, spaceID string) string {
	var w string
	_ = j.s.ReadDB().QueryRowContext(ctx, `SELECT workdir FROM space_workdirs WHERE space_id = ?`, spaceID).Scan(&w)
	return w
}

func (j *Journal) forgetSpaceMemory(ctx context.Context, spaceID string) error {
	return j.s.W().TxNamed(ctx, "journal.forget_memory", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(ctx, `DELETE FROM memory_sync WHERE space_id = ?`, spaceID)
		return err
	})
}
