package human

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
)

// ADR-0087: the dedup namespaces SpaceMan's own services use are refused for API and agent callers, who can then
// neither plant an action a service would reuse nor be handed back the service's action; a service can't use an
// internal key outside them.
func TestReservedDedupNamespaces(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	msg := func(key string, internal bool, who Requester) CreateInput {
		return CreateInput{Type: TypeMessage, Title: "Hello", Request: json.RawMessage(`{"requires_ack":false}`), DedupKey: key, Internal: internal, Requester: who}
	}
	system := Requester{Kind: "system", Label: "test"}
	operator := Requester{Kind: "operator", ID: op.ID}
	// The services' own keys, spelled out so the list can't lose one unnoticed.
	want := []string{"approval:", "usage-limit-human:", "account-pool-exhausted:", "control-failing:", "control-autostart-limit:", "control-autostart-refused:"}
	if strings.Join(reservedDedupPrefixes, ",") != strings.Join(want, ",") {
		t.Fatalf("reserved prefixes %v, want %v", reservedDedupPrefixes, want)
	}
	for _, p := range reservedDedupPrefixes {
		key := p + "x1"
		for _, who := range []Requester{agent, operator, system} {
			_, _, err := f.svc.Create(f.ctx, op, "spc_a", msg(key, false, who))
			code(t, err, apperr.CodeInvalid)
		}
		a, created, err := f.svc.Create(f.ctx, op, "spc_a", msg(key, true, system))
		must(t, err)
		if !created || a.DedupKey != key {
			t.Fatalf("internal %s: created=%v %+v", key, created, a)
		}
		again, created, err := f.svc.Create(f.ctx, op, "spc_a", msg(key, true, system))
		must(t, err)
		if created || again.ID != a.ID {
			t.Fatalf("internal %s repeated: created=%v id %s, want %s", key, created, again.ID, a.ID)
		}
		// The service's action is never handed back to an outside caller either.
		_, _, err = f.svc.Create(f.ctx, op, "spc_a", msg(key, false, agent))
		code(t, err, apperr.CodeInvalid)
	}
	for _, key := range []string{"", "mine:1", "approval", "xapproval:1"} {
		if _, _, err := f.svc.Create(f.ctx, op, "spc_a", msg(key, true, system)); err == nil {
			t.Fatalf("internal action with unreserved key %q accepted", key)
		}
	}
	// Other keys keep today's behaviour: a repeated key returns the existing action.
	a, created, err := f.svc.Create(f.ctx, op, "spc_a", msg("approval-of-x", false, agent))
	must(t, err)
	b, created2, err := f.svc.Create(f.ctx, op, "spc_a", msg("approval-of-x", false, agent))
	must(t, err)
	if !created || created2 || a.ID != b.ID {
		t.Fatalf("unreserved dedup: %v %v %s %s", created, created2, a.ID, b.ID)
	}
}
