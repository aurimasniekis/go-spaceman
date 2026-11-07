package proto

import (
	"strings"
	"testing"
)

// ADR-0044: repository URLs never carry credentials or a remote helper; local URLs only in development.
func TestValidateRepoURL(t *testing.T) {
	for raw, want := range map[string][2]bool{ // {production ok, development ok}
		"https://github.com/acme/api.git":    {true, true},
		"ssh://git@github.com/acme/api.git":  {true, true},
		"git@github.com:acme/api.git":        {true, true},
		"github.com:acme/api.git":            {true, true},
		"https://[::1]/repo.git":             {true, true},
		"http://git.example.com/a.git":       {false, true},
		"git://git.example.com/a.git":        {false, true},
		"file:///srv/a.git":                  {false, true},
		"/srv/a.git":                         {false, true},
		"https://user:pw@github.com/a.git":   {false, false},
		"https://ghp_token@github.com/a.git": {false, false},
		"ssh://git:pw@github.com/a.git":      {false, false},
		"ext::sh -c touch% /tmp/x":           {false, false},
		"ext::git-remote-x":                  {false, false},
		"fd::3":                              {false, false},
		"--upload-pack=touch /tmp/x":         {false, false},
		"https://github.com/a b.git":         {false, false},
		"https://github.com/a.git\n":         {false, false},
		"":                                   {false, false},
		"ftp://example.com/a.git":            {false, false},
		"https:///a.git":                     {false, false},
		"https://github.com/a.git?x=1":       {false, false},
		"relative/path":                      {false, false},
		"./a.git":                            {false, false},
	} {
		if got := ValidateRepoURL(raw, false) == nil; got != want[0] {
			t.Errorf("production %q: ok=%v, want %v", raw, got, want[0])
		}
		if got := ValidateRepoURL(raw, true) == nil; got != want[1] {
			t.Errorf("development %q: ok=%v, want %v", raw, got, want[1])
		}
	}
	if RedactRepoURL("https://u:p@h/x") != "https://h/x" || RedactRepoURL("git@h:x") != "git@h:x" {
		t.Fatal("redaction")
	}
}

// ADR-0057: an SSH key applies only to SSH URLs, and redaction removes keys from launch and sync payloads (stored and
// logged forms) while keeping everything else.
func TestRepoKeysOnlySSHAndRedacted(t *testing.T) {
	for raw, want := range map[string]bool{
		"git@github.com:acme/api.git": true, "ssh://git@host:2222/acme/api.git": true, "git+ssh://host/a.git": true, "ssh+git://host/a.git": true,
		"https://github.com/acme/api.git": false, "http://host/a.git": false, "file:///srv/a.git": false, "/srv/a.git": false,
	} {
		if IsSSHURL(raw) != want {
			t.Errorf("IsSSHURL(%q) = %v, want %v", raw, !want, want)
		}
	}
	src := RepoSource{ID: "rep_1", URL: "git@github.com:acme/api.git", SSHKey: "-----BEGIN OPENSSH PRIVATE KEY-----", KnownHosts: "github.com ssh-ed25519 AAAA"}
	l := LaunchPayload{Harness: "claude-code", RunToken: "tok", Secrets: map[string]string{"K": "v"}, Repository: &src}
	red := l.Redacted()
	if red.Repository.SSHKey != "" || red.Repository.KnownHosts == "" || red.RunToken != "" || red.Secrets != nil || l.Repository.SSHKey == "" {
		t.Fatalf("launch redaction %+v (original %+v)", red.Repository, l.Repository)
	}
	if r := (LaunchPayload{}).Redacted(); r.Repository != nil {
		t.Fatal("a launch without a repository gained one")
	}
	p := ReposSyncPayload{Repositories: []RepoSource{src, {ID: "rep_2"}}, Clone: []string{"rep_1"}}
	rp := p.Redacted()
	if rp.Repositories[0].SSHKey != "" || rp.Repositories[0].ID != "rep_1" || len(rp.Clone) != 1 || p.Repositories[0].SSHKey == "" {
		t.Fatalf("sync redaction %+v", rp)
	}
	pp := RelocatePreparePayload{RelocationID: "rel_1", Repos: []RelocateRepo{{Path: "api", Source: &src}, {Path: "lib"}}}
	rpp := pp.Redacted()
	if rpp.Repos[0].Source.SSHKey != "" || rpp.Repos[0].Source.KnownHosts == "" || rpp.Repos[1].Source != nil || pp.Repos[0].Source.SSHKey == "" {
		t.Fatalf("prepare redaction %+v (original %+v)", rpp.Repos[0].Source, pp.Repos[0].Source)
	}
	if !strings.Contains(BuiltinKnownHosts, "github.com ssh-ed25519 ") {
		t.Fatal("built-in pins lost GitHub's ed25519 key")
	}
}
