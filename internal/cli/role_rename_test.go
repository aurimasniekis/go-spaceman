package cli

import (
	"net/http"
	"strings"
	"testing"
)

// ADR-0093 on the CLI: role rename checks the role's revision (an optional If-Match read just before the change, which
// an explicit --if-match overrides), and after a table-format rename it says what is left to do: the node's memory
// move, when the node has not made it yet.
func TestRoleRename(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	const route = "/spaces/" + spaceID + "/roles/implementor"
	// The version (9) differs from the revision (4) the API compares; the fake's ETag comes from version too.
	f.json("GET "+route, 200, map[string]any{"name": "implementor", "revision": 4, "version": 9})
	pending := map[string]any{"name": "implementor-hard", "revision": 5, "rename": map[string]any{"from": "implementor", "to": "implementor-hard",
		"node_id": nodeID, "node_state": "pending"}}
	f.on("POST "+route+"/rename", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { reply(w, 200, pending) })
	e := f.env()

	r := e.run(t, "role", "rename", "implementor", "--to", "implementor-hard", "--space", spaceID)
	if r.code != 0 {
		t.Fatalf("rename: exit %d\nstdout: %s\nstderr: %s", r.code, r.out, r.err)
	}
	req := f.last("POST", route+"/rename")
	if req.header.Get("If-Match") != `"4"` || req.body["new_name"] != "implementor-hard" {
		t.Fatalf("rename request: If-Match %q body %v", req.header.Get("If-Match"), req.body)
	}
	if want := "Role renamed. The role's memory moves when node " + nodeID + " reconnects and reports; launches of the role wait until then."; !strings.Contains(r.err, want) {
		t.Fatalf("pending note: stderr %q, want it to contain %q", r.err, want)
	}

	// An explicit --if-match wins over the revision read.
	e.ok(t, "role", "rename", "implementor", "--to", "implementor-hard", "--space", spaceID, "--if-match", "7")
	if h := f.last("POST", route+"/rename").header.Get("If-Match"); h != `"7"` {
		t.Fatalf("explicit If-Match %q", h)
	}

	// JSON output prints no note.
	r = e.run(t, "-o", "json", "role", "rename", "implementor", "--to", "implementor-hard", "--space", spaceID)
	if r.code != 0 || strings.Contains(r.err, "Role renamed") {
		t.Fatalf("json output: exit %d, stderr %q", r.code, r.err)
	}

	// Once the node has moved the memory (done), the note says only that the role was renamed.
	done := map[string]any{"name": "implementor-hard", "revision": 5, "rename": map[string]any{"from": "implementor", "to": "implementor-hard",
		"node_id": nodeID, "node_state": "done"}}
	f.on("POST "+route+"/rename", func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { reply(w, 200, done) })
	r = e.run(t, "role", "rename", "implementor", "--to", "implementor-hard", "--space", spaceID)
	if r.code != 0 || strings.TrimSpace(r.err) != "Role renamed." {
		t.Fatalf("done note: exit %d, stderr %q", r.code, r.err)
	}

	// A role that does not exist stops before the rename, with not found.
	e.code(t, ExitNotFound, "role", "rename", "nobody", "--to", "nobody-2", "--space", spaceID)
	if f.count("POST", "/spaces/"+spaceID+"/roles/nobody/rename") != 0 {
		t.Fatal("a missing role was renamed")
	}
}

// renameNote: a pending memory move names the node; done and not_needed say only that the role was renamed.
func TestRenameNote(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		m    map[string]any
		want string
	}{
		{"pending", map[string]any{"rename": map[string]any{"node_id": "nod_a", "node_state": "pending"}},
			"Role renamed. The role's memory moves when node nod_a reconnects and reports; launches of the role wait until then."},
		{"done", map[string]any{"rename": map[string]any{"node_id": "nod_a", "node_state": "done"}}, "Role renamed."},
		{"not needed", map[string]any{"rename": map[string]any{"node_state": "not_needed"}}, "Role renamed."},
		{"no rename record", map[string]any{"name": "x"}, "Role renamed."},
	} {
		if got := renameNote(c.m); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
