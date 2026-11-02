package template

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/pulse-github-issues-opened.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func issueEvent(t *testing.T) Event {
	return Event{ID: "01JBEXAMPLE0ISSUESOPENED0", Subject: "evt.github.example-org.webhook.issues.opened", Type: "github.webhook.issues.opened.v1",
		Source: "pulse-github", ContentType: "application/json", Time: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC), Data: fixture(t)}
}

// SM-EVT-002/009: the GitHub issue seed renders the go-pulse-github issues_opened event deterministically; the repository
// variable is a payload filter, not a subject token.
func TestGitHubIssueSeed(t *testing.T) {
	d := GitHubIssue()
	b, err := d.Bind(map[string]string{"account_key": "example-org", "repository": "example-org/tool"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Subject != "evt.github.example-org.webhook.issues.*" {
		t.Fatalf("subject %q", b.Subject)
	}
	r, err := b.Apply(issueEvent(t))
	if err != nil || !r.Accepted {
		t.Fatalf("apply: %+v %v", r, err)
	}
	if r.Title != "example-org/tool#42 opened: Bug <script>" {
		t.Fatalf("title %q", r.Title)
	}
	for _, want := range []string{"**aurimasniekis** opened issue [example-org/tool#42](https://github.com/example-org/tool/issues/42)", "secret body"} {
		if !strings.Contains(r.Body, want) {
			t.Fatalf("body lacks %q:\n%s", want, r.Body)
		}
	}
	if r.Correlation != "github:example-org/tool#42" || !strings.Contains(r.Instructions, "example-org/tool") {
		t.Fatalf("correlation %q instructions %q", r.Correlation, r.Instructions)
	}
	again, _ := b.Apply(issueEvent(t))
	if *again != *r {
		t.Fatal("rendering is not deterministic")
	}
	// Another repository of the same account is filtered out (acknowledged, not an error).
	other, _ := d.Bind(map[string]string{"account_key": "example-org", "repository": "example-org/other"})
	if r, err := other.Apply(issueEvent(t)); err != nil || r.Accepted || !strings.Contains(r.SkipReason, "repository.full_name") {
		t.Fatalf("other repo: %+v %v", r, err)
	}
	if pr := GitHubPullRequest(); pr.Validate() != nil {
		err := pr.Validate()
		t.Fatal(err)
	}
}

// SM-EVT-010: variables cannot add wildcards, tokens or placeholders, and must match their pattern.
func TestBindRejectsWidening(t *testing.T) {
	d := GitHubIssue()
	for name, vals := range map[string]map[string]string{
		"wildcard":        {"account_key": "*", "repository": "a/b"},
		"tail wildcard":   {"account_key": ">", "repository": "a/b"},
		"extra token":     {"account_key": "org.webhook", "repository": "a/b"},
		"placeholder":     {"account_key": "{{vars.x}}", "repository": "a/b"},
		"pattern":         {"account_key": "Example", "repository": "a/b"},
		"repo pattern":    {"account_key": "org", "repository": "not a repo"},
		"missing":         {"account_key": "org"},
		"unknown":         {"account_key": "org", "repository": "a/b", "extra": "x"},
		"repo wildcard *": {"account_key": "org", "repository": "a/*"},
	} {
		if _, err := d.Bind(vals); err == nil {
			t.Errorf("%s: accepted %v", name, vals)
		}
	}
}

func TestValidateDefinition(t *testing.T) {
	base := func() Definition {
		return Definition{Subject: "evt.ops.alert.{{vars.env}}", Variables: []Variable{{Name: "env", Pattern: "[a-z]+", Required: true}}, Title: "{{data.name}}"}
	}
	for name, mut := range map[string]func(*Definition){
		"partial placeholder token": func(d *Definition) { d.Subject = "evt.ops.alert.x{{vars.env}}" },
		"undefined variable":        func(d *Definition) { d.Subject = "evt.ops.alert.{{vars.nope}}" },
		"not under evt":             func(d *Definition) { d.Subject = "ops.alert.x.y" },
		"bad path":                  func(d *Definition) { d.Title = "{{secrets.token}}" },
		"bad filter":                func(d *Definition) { d.Title = "{{data.x | exec \"rm\"}}" },
		"unbalanced":                func(d *Definition) { d.Body = "{{data.x" },
		"filter on vars":            func(d *Definition) { d.Filters = []Filter{{Path: "vars.env", Op: "eq", Value: "x"}} },
		"filter value template":     func(d *Definition) { d.Filters = []Filter{{Path: "data.x", Op: "eq", Value: "{{data.y}}"}} },
		"bad op":                    func(d *Definition) { d.Filters = []Filter{{Path: "data.x", Op: "regex", Value: "."}} },
		"bad pattern":               func(d *Definition) { d.Variables[0].Pattern = "(" },
		"no title":                  func(d *Definition) { d.Title = " " },
		"remote schema":             func(d *Definition) { d.InputSchema = json.RawMessage(`{"$ref":"https://example.com/s.json"}`) },
		"file schema":               func(d *Definition) { d.InputSchema = json.RawMessage(`{"$ref":"file:///etc/passwd"}`) },
	} {
		d := base()
		mut(&d)
		if err := d.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	d := base()
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
}

// SM-EVT-013: events that fail the input schema are invalid (quarantine), not silently dropped.
func TestInputSchemaAndFilters(t *testing.T) {
	d := Definition{Subject: "evt.ops.alert.*", Title: "{{data.name | upper}} {{event.type}}", Body: "{{data | json}}",
		InputSchema: json.RawMessage(`{"type":"object","required":["name","level"],"properties":{"level":{"type":"integer","minimum":1}}}`),
		Filters:     []Filter{{Path: "data.level", Op: "in", Values: []string{"2", "3"}}, {Path: "data.name", Op: "prefix", Value: "db"}, {Path: "data.muted", Op: "ne", Value: "true"}},
		Urgency:     "high"}
	b, err := d.Bind(nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := func(data string) Event {
		return Event{ID: "1", Subject: "evt.ops.alert.fired", Type: "ops.alert.v1", Data: []byte(data)}
	}
	if _, err := b.Apply(ev(`{"name":"db-lag"}`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("schema: %v", err)
	}
	if _, err := b.Apply(ev(`not json`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-JSON with a schema: %v", err)
	}
	r, err := b.Apply(ev(`{"name":"db-lag","level":2}`))
	if err != nil || !r.Accepted || r.Title != "DB-LAG ops.alert.v1" || r.Urgency != "high" || r.Body != `{"level":2,"name":"db-lag"}` {
		t.Fatalf("accepted: %+v %v", r, err)
	}
	for _, data := range []string{`{"name":"db-lag","level":1}`, `{"name":"api","level":2}`, `{"name":"db","level":3,"muted":true}`} {
		if r, err := b.Apply(ev(data)); err != nil || r.Accepted {
			t.Errorf("%s should be filtered: %+v %v", data, r, err)
		}
	}
}

func TestRenderFilters(t *testing.T) {
	ctx := Context{Event: map[string]any{"id": "e1"}, Data: map[string]any{"s": "a `b`  c\nd", "n": json.Number("3"), "list": []any{"x", map[string]any{"k": "v"}}},
		Vars: map[string]string{"repo": "o/r"}}
	for tmpl, want := range map[string]string{
		"{{data.missing | default \"none\"}}": "none",
		"{{data.s | oneline}}":                "a `b` c d",
		"{{data.s | oneline | code}}":         "`` a `b` c d ``",
		"{{data.s | truncate 3}}":             "a `…",
		"{{data.n}}/{{data.list.1.k}}":        "3/v",
		"{{data.list | json}}":                `["x",{"k":"v"}]`,
		"{{vars.repo | upper}} {{event.id}}":  "O/R e1",
		"{{ data.list.9 | default \"-\" }}":   "-",
	} {
		got, err := Render(tmpl, ctx, 0)
		if err != nil || got != want {
			t.Errorf("%s = %q (%v), want %q", tmpl, got, err, want)
		}
	}
	long, _ := Render("{{data.s}}{{data.s}}", ctx, 5)
	if !strings.HasSuffix(long, "(truncated)") {
		t.Fatalf("bound: %q", long)
	}
}
