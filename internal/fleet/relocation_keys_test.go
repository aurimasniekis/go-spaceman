package fleet

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// SM-SPACE-005, ADR-0057: a relocated repository is matched to the space's configured one by path, else by a remote's
// URL; only a repository with a SpaceMan SSH key (its own or a shared one) gets a source, which carries no key.
func TestRelocationKeyedSource(t *testing.T) {
	url := "git@github.com:acme/api.git"
	configured := []db.SpaceRepository{
		{ID: "rep_api", Name: "api", Path: "api", Url: url, DefaultBranch: "main", SshKeyEnc: []byte("sealed"), KnownHosts: "github.com ssh-ed25519 AAAA"},
		{ID: "rep_lib", Name: "lib", Path: "lib", Url: "git@github.com:acme/lib.git", GitKeyID: "gk_1"},
		{ID: "rep_web", Name: "web", Path: "web", Url: "git@github.com:acme/web.git"},
		{ID: "rep_doc", Name: "doc", Path: "doc", Url: "https://example.com/doc.git", SshKeyEnc: []byte("sealed")},
	}
	remotes := func(u string) []proto.GitRemote { return []proto.GitRemote{{Name: "origin", URL: u}} }
	for _, tc := range []struct {
		rr   proto.RelocateRepo
		want string
	}{
		{proto.RelocateRepo{Path: "api", Remotes: remotes("https://example.com/other.git")}, "rep_api"}, // by path
		{proto.RelocateRepo{Path: "moved/api", Remotes: remotes(url)}, "rep_api"},                       // by URL
		{proto.RelocateRepo{Path: "lib", Remotes: remotes("x")}, "rep_lib"},                             // a shared key
		{proto.RelocateRepo{Path: "web", Remotes: remotes(url)}, ""},                                    // the path's repository has no key
		{proto.RelocateRepo{Path: "doc", Remotes: remotes("x")}, ""},                                    // a key, but not an SSH URL
		{proto.RelocateRepo{Path: "other", Remotes: remotes("x")}, ""},                                  // not configured
	} {
		got := keyedSource(tc.rr, configured)
		switch {
		case tc.want == "" && got != nil, tc.want != "" && (got == nil || got.ID != tc.want):
			t.Errorf("%s: got %+v, want %q", tc.rr.Path, got, tc.want)
		case got != nil && got.SSHKey != "":
			t.Errorf("%s: the source carries a key", tc.rr.Path)
		}
	}
	if s := keyedSource(proto.RelocateRepo{Path: "api"}, configured); s.URL != url || s.KnownHosts == "" || s.DefaultBranch != "main" {
		t.Errorf("source %+v", s)
	}
}

// The prepare command's repository keys go only onto the wire message, for a node that speaks repository keys; the
// stored payload stays without them.
func TestRelocationPrepareKeyOnWireOnly(t *testing.T) {
	const pem = "-----BEGIN OPENSSH PRIVATE KEY-----\nx\n-----END OPENSSH PRIVATE KEY-----\n"
	stored := proto.RelocatePreparePayload{RelocationID: "rel_1", Workdir: "w", Repos: []proto.RelocateRepo{
		{Path: "api", Source: &proto.RepoSource{ID: "rep_api", URL: "git@github.com:acme/api.git", SSHKey: pem}},
		{Path: "lib"},
	}}.Redacted()
	b, _ := json.Marshal(stored)
	if strings.Contains(string(b), "PRIVATE KEY") {
		t.Fatalf("stored payload holds the key: %s", b)
	}
	c := db.NodeCommand{ID: "cmd_1", Kind: proto.CmdRelocatePrepare, SpaceID: "spc_a", PayloadJson: string(b)}
	asked := []string{}
	h := &Hub{RepoKey: func(_ context.Context, spaceID, repoID string) (string, error) {
		asked = append(asked, spaceID+"/"+repoID)
		return pem, nil
	}}
	msg, err := h.materialize(context.Background(), c, "nod_1", proto.MinorRepoKeys)
	if err != nil {
		t.Fatal(err)
	}
	var wire proto.RelocatePreparePayload
	if err := json.Unmarshal(msg.Payload, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Repos[0].Source.SSHKey != pem || wire.Repos[1].Source != nil || len(asked) != 1 || asked[0] != "spc_a/rep_api" {
		t.Fatalf("wire payload %+v, asked %v", wire.Repos, asked)
	}
	if strings.Contains(c.PayloadJson, "PRIVATE KEY") {
		t.Fatal("materialize changed the stored payload")
	}
	// A node older than repository keys gets the payload as stored.
	if msg, err = h.materialize(context.Background(), c, "nod_1", proto.MinorRepoKeys-1); err != nil || strings.Contains(string(msg.Payload), "PRIVATE KEY") {
		t.Fatalf("old node: %s %v", msg.Payload, err)
	}
}
