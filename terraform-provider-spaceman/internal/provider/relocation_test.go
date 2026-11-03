package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/client"
)

// A node_id change is a relocation through the API (SM-API-005, ADR-0037): it starts one with an idempotency key, waits
// for it, reports a refusal with the source's findings, resumes a blocked one with the configured policy instead of
// starting another, and never overrides a relocation to another node (never a destroy and recreate).
func TestRelocationOnNodeChange(t *testing.T) {
	relocationPoll = 10 * time.Millisecond
	var mu sync.Mutex
	state, started, retried := "", 0, ""
	steps := []string{"checking", "preparing_destination", "done"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		rel := func() {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "rlc_1", "destination_node_id": "nod_b", "state": state, "error": "refused: unpushed",
				"issues": []any{map[string]any{"code": "unpushed", "path": "app", "detail": "branch x: 1 commit(s)", "exportable": true}}})
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/relocation"):
			if state == "" {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"status":404,"code":"not_found"}`))
				return
			}
			if len(steps) > 0 && state != "blocked" {
				state, steps = steps[0], steps[1:]
			}
			rel()
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/relocation"):
			var in client.StartRelocationInput
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.DestinationNodeID != "nod_b" || in.DirtyState != "refuse" || in.Recovery != "retain" || r.Header.Get("Idempotency-Key") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			started++
			state = "draining"
			rel()
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/relocation/retry"):
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			retried, state = in["dirty_state"], "exporting"
			rel()
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c, err := client.New(srv.URL, "spaceman_at_test", client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := &spaceResource{base{c: c}}
	m := spaceModel{NodeID: types.StringValue("nod_b"), RelocDirtyState: types.StringValue("refuse"), RelocStopRuns: types.BoolValue(false),
		RelocRecovery: types.StringValue("retain"), RelocRetainDays: types.Int64Value(0), RelocTimeout: types.StringValue("10s")}
	var d diag.Diagnostics
	if !r.relocate(context.Background(), "spc_x", m, &d) || started != 1 || state != "done" {
		t.Fatalf("relocate: %v started=%d state=%s", d, started, state)
	}
	// Blocked: the apply fails with the findings and leaves the relocation for the operator.
	mu.Lock()
	state, steps = "", []string{"blocked"}
	mu.Unlock()
	d = nil
	if r.relocate(context.Background(), "spc_x", m, &d) || !d.HasError() || !strings.Contains(d[0].Detail(), "unpushed (app)") {
		t.Fatalf("blocked: %v", d)
	}
	// The next apply with export retries the blocked relocation instead of starting another.
	m.RelocDirtyState = types.StringValue("export")
	mu.Lock()
	steps = []string{"done"}
	mu.Unlock()
	d = nil
	ok := r.relocate(context.Background(), "spc_x", m, &d)
	mu.Lock()
	if !ok || retried != "export" || started != 2 {
		t.Fatalf("retry: %v retried=%q started=%d", d, retried, started)
	}
	// A relocation to another node in progress is not overridden.
	state, steps = "checking", nil
	mu.Unlock()
	d = nil
	m.NodeID = types.StringValue("nod_c")
	if r.relocate(context.Background(), "spc_x", m, &d) || !strings.Contains(d[0].Summary(), "Another relocation") {
		t.Fatalf("other destination: %v", d)
	}
}
