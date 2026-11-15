package notify

import (
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

func TestResourceMapping(t *testing.T) {
	k := Key{Type: TypeRun, ID: "x"}
	for obs, want := range map[string]bool{"failed": true, "lost": true, "running": false, "exited": false, "pending": false} {
		r := runResource(k, db.Run{Observed: obs, StatusKind: "failed"})
		if (r != nil && r.Notify) != want {
			t.Errorf("run observed %q: notify = %v, want %v", obs, r != nil && r.Notify, want)
		}
	}
	for state, want := range map[string]bool{"stored": true, "available": true, "acknowledged": false, "resolved": false} {
		r := messageResource(k, db.Message{ToKind: "operators", State: state})
		if r.Notify != want || r.Final == want {
			t.Errorf("message %q: notify %v final %v", state, r.Notify, r.Final)
		}
	}
	if messageResource(k, db.Message{ToKind: "run", State: "stored"}) != nil {
		t.Error("a message to a run is not an operator notification")
	}
	if r := inboxResource(k, db.InboxItem{Status: "new"}); !r.Notify || r.Final {
		t.Error("a new inbox item notifies")
	}
	if r := inboxResource(k, db.InboxItem{Status: "in_progress"}); r.Notify || r.Final {
		t.Error("an inbox item in progress is neither")
	}
	if r := inboxResource(k, db.InboxItem{Status: "done"}); r.Notify || !r.Final {
		t.Error("a done inbox item is final")
	}
}
