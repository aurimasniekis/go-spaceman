package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/catalog"
)

// TestRenameProblem pins the statuses of the rename's refusals to the API's documented contract (ADR-0093): a busy role,
// a taken name or a node too old is 409 with the refusal's own code; an invalid name is 422 validation_failed and a
// stale revision is 409 precondition_failed (both through the usual mapping); a missing role stays 404.
func TestRenameProblem(t *testing.T) {
	busy := apperr.Wrap(apperr.CodePrecondition, &catalog.RenameRefused{Reason: catalog.RenameRoleBusy, Blockers: []string{"run run_1"}}, "busy")
	taken := apperr.Wrap(apperr.CodeAlreadyExists, &catalog.RenameRefused{Reason: catalog.RenameNameTaken}, "taken")
	tooOld := apperr.Wrap(apperr.CodePrecondition, &catalog.RenameRefused{Reason: catalog.RenameNodeTooOld}, "old node")
	d := &Deps{}
	for _, c := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"busy", busy, http.StatusConflict, "role_busy"},
		{"taken", taken, http.StatusConflict, "name_taken"},
		{"node too old", tooOld, http.StatusConflict, "node_too_old"},
		{"invalid", apperr.New(apperr.CodeInvalid, "bad"), http.StatusUnprocessableEntity, "validation_failed"},
		{"stale revision", apperr.New(apperr.CodePrecondition, "stale"), http.StatusConflict, "precondition_failed"},
	} {
		var p *Problem
		if !errors.As(d.fail(context.Background(), "roles.rename", renameProblem(c.err)), &p) || p.Status != c.status || p.Code != c.code {
			t.Errorf("%s: got %+v, want %d %s", c.name, p, c.status, c.code)
		}
	}
	nf := apperr.New(apperr.CodeNotFound, "no role")
	if !errors.Is(renameProblem(nf), nf) {
		t.Error("not found must pass through (404)")
	}
}
