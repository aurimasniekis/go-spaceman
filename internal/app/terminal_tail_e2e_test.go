package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
)

// ADR-0081: a live terminal's catch-up can be only the end of the recording (tail=), preceded by a "tail" event that
// says what it leaves out; replay can fetch the same tail as one page. Pages of the full recording still work.
func TestTerminalTailCatchUp(t *testing.T) {
	t.Parallel()
	e := newFleetEnv(t)
	ctx := context.Background()
	e.enroll()
	e.startAgent()
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "tail-test", Name: "Tail", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })
	r, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor-backend", Harness: hns.KindCodex})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "running", func() bool { return e.run1(r.ID).Observed == "running" })
	term := e.run.LiveByLabel("spaceman.run", r.ID)[0].ID
	stored := func(want string) bool {
		res, err := e.c.Fleet.Recordings().Read(ctx, r.ID, 0)
		if err != nil {
			return false
		}
		var out strings.Builder
		for _, rec := range res.Records {
			out.Write(rec.Data)
		}
		return strings.Contains(out.String(), want)
	}
	// The recorder is attached once its first output is stored: everything after it is recorded.
	e.run.Output(term, "ready\r\n")
	eventually(t, "recorder attached", func() bool { return stored("ready") })
	// A few writes: the fake terminal drops output past a small buffer.
	e.run.Output(term, "\x1b[?1049h\x1b[?25l")
	for c := range 5 {
		var b strings.Builder
		for i := range 40 {
			fmt.Fprintf(&b, "line %03d of the earlier output\r\n", c*40+i)
		}
		e.run.Output(term, b.String())
	}
	e.run.Output(term, "the last line\r\n")
	eventually(t, "recording shipped", func() bool { return stored("the last line") })

	rootSecret, _, err := e.c.Auth.IssueRootToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/spaces/"+sp.ID+"/runs/"+r.ID+path, nil)
		req.Header.Set("Authorization", "Bearer "+rootSecret)
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %v %v", path, res, err)
		}
		return res
	}
	type tailInfo struct {
		From         uint64 `json:"from"`
		SkippedBytes int64  `json:"skipped_bytes"`
		Cols         int    `json:"cols"`
		Modes        []struct {
			Mode int  `json:"mode"`
			On   bool `json:"on"`
		} `json:"modes"`
	}
	type page struct {
		Records []struct {
			Seq  uint64 `json:"seq"`
			Kind string `json:"k"`
			Data []byte `json:"d"`
		} `json:"records"`
		More      bool      `json:"more"`
		NextAfter uint64    `json:"next_after"`
		Tail      *tailInfo `json:"tail"`
	}
	read := func(path string) page {
		res := get(path)
		defer res.Body.Close()
		var p page
		if err := json.NewDecoder(res.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	checkTail := func(what string, ti *tailInfo, records int, last []byte) {
		t.Helper()
		if ti == nil || ti.SkippedBytes == 0 || ti.From < 2 || ti.Cols == 0 || len(ti.Modes) != 2 || ti.Modes[0].Mode != 1049 || !ti.Modes[0].On ||
			ti.Modes[1].Mode != 25 || ti.Modes[1].On {
			t.Fatalf("%s: tail %+v", what, ti)
		}
		if records == 0 || records > 4 || !strings.Contains(string(last), "the last line") {
			t.Fatalf("%s: %d records, last %q", what, records, last)
		}
	}

	// Replay: the tail as one page.
	p := read("/recording?tail=100")
	checkTail("replay tail", p.Tail, len(p.Records), p.Records[len(p.Records)-1].Data)
	if p.More || p.NextAfter != p.Records[len(p.Records)-1].Seq {
		t.Fatalf("tail page more=%v next=%d", p.More, p.NextAfter)
	}
	// Plain pages are unchanged: one record, more to come.
	if p := read("/recording?limit=1"); len(p.Records) != 1 || !p.More || p.NextAfter != 1 || p.Tail != nil || p.Records[0].Kind != "open" {
		t.Fatalf("first page %+v", p)
	}

	// Live: a "tail" event, the tail's records, then live output.
	res := get("/terminal?tail=100")
	defer res.Body.Close()
	events := make(chan string, 256)
	go func() {
		buf := make([]byte, 64<<10)
		var acc strings.Builder
		for {
			n, err := res.Body.Read(buf)
			acc.Write(buf[:n])
			for {
				s := acc.String()
				i := strings.Index(s, "\n\n")
				if i < 0 {
					break
				}
				events <- s[:i]
				acc.Reset()
				acc.WriteString(s[i+2:])
			}
			if err != nil {
				close(events)
				return
			}
		}
	}()
	next := func() (string, string) {
		t.Helper()
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("stream ended early")
			}
			var kind, data string
			for l := range strings.SplitSeq(ev, "\n") {
				if v, ok := strings.CutPrefix(l, "event: "); ok {
					kind = v
				} else if v, ok := strings.CutPrefix(l, "data: "); ok {
					data = v
				}
			}
			return kind, data
		case <-time.After(10 * time.Second):
			t.Fatal("no event")
		}
		return "", ""
	}
	kind, data := next()
	var ti tailInfo
	if kind != "tail" || json.Unmarshal([]byte(data), &ti) != nil {
		t.Fatalf("first event %q %s", kind, data)
	}
	var n int
	var last []byte
	for {
		kind, data = next()
		if kind != "record" {
			t.Fatalf("event %q", kind)
		}
		var rec struct {
			Seq  uint64 `json:"seq"`
			Data []byte `json:"d"`
		}
		_ = json.Unmarshal([]byte(data), &rec)
		if n == 0 && rec.Seq != ti.From {
			t.Fatalf("first record %d, tail from %d", rec.Seq, ti.From)
		}
		n++
		last = rec.Data
		if strings.Contains(string(rec.Data), "the last line") {
			break
		}
	}
	checkTail("live tail", &ti, n, last)
	e.run.Output(term, "live line\r\n")
	for {
		kind, data = next()
		if kind == "gap" {
			t.Fatalf("a live record after the tail must not look like a gap: %s", data)
		}
		if kind == "record" && strings.Contains(data, b64("live line\r\n")) {
			break
		}
	}
	var liveRec struct {
		Seq uint64 `json:"seq"`
	}
	_ = json.Unmarshal([]byte(data), &liveRec)

	// A reconnect (Last-Event-ID) resumes after that record even with tail= in the URL: no tail event, nothing again.
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/spaces/"+sp.ID+"/runs/"+r.ID+"/terminal?tail=100", nil)
	req.Header.Set("Authorization", "Bearer "+rootSecret)
	req.Header.Set("Last-Event-ID", fmt.Sprint(liveRec.Seq-1))
	res2, err := http.DefaultClient.Do(req)
	if err != nil || res2.StatusCode != http.StatusOK {
		t.Fatalf("resume: %v %v", res2, err)
	}
	defer res2.Body.Close()
	buf := make([]byte, 64<<10)
	var got strings.Builder
	for !strings.Contains(got.String(), "\n\n") {
		n, err := res2.Body.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	first := got.String()
	if !strings.HasPrefix(first, "event: record\nid: "+fmt.Sprint(liveRec.Seq)+"\n") {
		t.Fatalf("resumed stream starts with %q, want the record after Last-Event-ID", first[:min(len(first), 80)])
	}
}
