package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// SM-API-002: the change feed is a durable watch. A client resumes from its last position (`after`, or the
// Last-Event-ID header an EventSource sends on reconnect) and receives exactly the changes it missed, in order; a
// position beyond the feed (for example after a restore) is reported as a gap and the stream continues from the end. A
// client without a position can start live, at the newest change, instead of replaying the retained history.
func TestWatchResumesFromItsPosition(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, err := h.c.Auth.IssueRootToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	space := ids.New(ids.Space)
	record := func(id string) int64 {
		t.Helper()
		if err := h.c.Store.W().Tx(ctx, func(tx *store.Tx) error { return h.c.Changes.Record(ctx, tx, space, "task", id, "updated", 1) }); err != nil {
			t.Fatal(err)
		}
		seq, err := h.c.Store.R().LatestChangeSeq(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return seq
	}
	s1, s2, s3 := record("tsk_1"), record("tsk_2"), record("tsk_3")
	seqOf := func(c map[string]any) int64 { f, _ := c["seq"].(float64); return int64(f) }
	auth := []string{"Authorization", "Bearer " + rootSecret}

	ch := openSSE(t, http.DefaultClient, h.srv.URL+api.Prefix+"/watch?after="+strconv.FormatInt(s1, 10), auth...)
	if c := nextChange(t, ch); seqOf(c) != s2 || c["resource_id"] != "tsk_2" {
		t.Fatalf("first change after %d: %v", s1, c)
	}
	if c := nextChange(t, ch); seqOf(c) != s3 || c["resource_id"] != "tsk_3" {
		t.Fatalf("second change after %d: %v", s1, c)
	}
	s4 := record("tsk_4")
	if c := nextChange(t, ch); seqOf(c) != s4 {
		t.Fatalf("live change: %v", c)
	}

	// A reconnecting EventSource sends its last event ID instead of the query parameter.
	re := openSSE(t, http.DefaultClient, h.srv.URL+api.Prefix+"/watch", append(auth, "Last-Event-ID", strconv.FormatInt(s3, 10))...)
	if c := nextChange(t, re); seqOf(c) != s4 {
		t.Fatalf("resumed with Last-Event-ID %d: %v", s3, c)
	}

	// A position the feed never reached: a gap event, then new changes.
	ahead := s4 + 1000
	gs := openSSE(t, http.DefaultClient, h.srv.URL+api.Prefix+"/watch?after="+strconv.FormatInt(ahead, 10), auth...)
	select {
	case ev := <-gs:
		var gap map[string]int64
		if ev.typ != "gap" || json.Unmarshal([]byte(ev.data), &gap) != nil || gap["after"] != ahead || gap["oldest"] != s4+1 {
			t.Fatalf("first event for a position beyond the feed: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no gap event")
	}
	s5 := record("tsk_5")
	if c := nextChange(t, gs); seqOf(c) != s5 {
		t.Fatalf("change after the gap: %v", c)
	}

	// A client without a position that has just loaded its data (a fresh web UI tab) asks for live=1: it learns the
	// newest position and gets only what changes from then on, never the retained history.
	ls := openSSE(t, http.DefaultClient, h.srv.URL+api.Prefix+"/watch?live=1", auth...)
	select {
	case ev := <-ls:
		var pos map[string]int64
		if ev.typ != "position" || json.Unmarshal([]byte(ev.data), &pos) != nil || pos["seq"] != s5 {
			t.Fatalf("first event of a live stream: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no position event")
	}
	s6 := record("tsk_6")
	if c := nextChange(t, ls); seqOf(c) != s6 {
		t.Fatalf("first change of a live stream: %v", c)
	}
	// live=1 with a position resumes from that position instead.
	lr := openSSE(t, http.DefaultClient, h.srv.URL+api.Prefix+"/watch?live=1&after="+strconv.FormatInt(s5, 10), auth...)
	if c := nextChange(t, lr); seqOf(c) != s6 {
		t.Fatalf("live stream with a position: %v", c)
	}
}
