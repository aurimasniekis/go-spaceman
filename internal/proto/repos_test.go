package proto

import "testing"

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
