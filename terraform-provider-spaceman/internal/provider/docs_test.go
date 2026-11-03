package provider

// The registry documentation in docs/ is rendered from the schemas and examples/ by this test (tfplugindocs is not
// available offline here). TestDocsUpToDate fails when docs/ is stale; regenerate with
//
//	SM_TF_DOCS=1 go test ./internal/provider -run TestDocsUpToDate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	pschema "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
)

// docAttr is the documented view of one attribute.
type docAttr struct {
	name                                        string
	typ                                         string
	desc                                        string
	required, optional, computed, sensitive, wo bool
	nested                                      []docAttr
}

func resourceAttrs(m map[string]rschema.Attribute) []docAttr {
	var out []docAttr
	for name, a := range m {
		d := docAttr{name: name, desc: a.GetMarkdownDescription(), required: a.IsRequired(), optional: a.IsOptional(), computed: a.IsComputed(),
			sensitive: a.IsSensitive(), wo: a.IsWriteOnly()}
		switch t := a.(type) {
		case rschema.StringAttribute:
			d.typ = "String"
		case rschema.Int64Attribute:
			d.typ = "Number"
		case rschema.BoolAttribute:
			d.typ = "Boolean"
		case rschema.ListAttribute:
			d.typ = "List of String"
		case rschema.SetAttribute:
			d.typ = "Set of String"
		case rschema.MapAttribute:
			d.typ = "Map of String"
		case rschema.ListNestedAttribute:
			d.typ, d.nested = "Attributes List", resourceAttrs(t.NestedObject.Attributes)
		case rschema.SetNestedAttribute:
			d.typ, d.nested = "Attributes Set", resourceAttrs(t.NestedObject.Attributes)
		case rschema.SingleNestedAttribute:
			d.typ, d.nested = "Attributes", resourceAttrs(t.Attributes)
		default:
			d.typ = fmt.Sprintf("%T", a)
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func dataAttrs(m map[string]dschema.Attribute) []docAttr {
	var out []docAttr
	for name, a := range m {
		d := docAttr{name: name, desc: a.GetMarkdownDescription(), required: a.IsRequired(), optional: a.IsOptional(), computed: a.IsComputed(),
			sensitive: a.IsSensitive()}
		switch t := a.(type) {
		case dschema.StringAttribute:
			d.typ = "String"
		case dschema.Int64Attribute:
			d.typ = "Number"
		case dschema.BoolAttribute:
			d.typ = "Boolean"
		case dschema.MapAttribute:
			d.typ = "Map of String"
		case dschema.ListNestedAttribute:
			d.typ, d.nested = "Attributes List", dataAttrs(t.NestedObject.Attributes)
		default:
			d.typ = fmt.Sprintf("%T", a)
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func writeAttrs(b *strings.Builder, attrs []docAttr, anchor string) {
	groups := []struct {
		title string
		match func(docAttr) bool
	}{
		{"Required", func(a docAttr) bool { return a.required }},
		{"Optional", func(a docAttr) bool { return a.optional }},
		{"Read-Only", func(a docAttr) bool { return !a.required && !a.optional }},
	}
	var nested []docAttr
	for _, g := range groups {
		var in []docAttr
		for _, a := range attrs {
			if g.match(a) {
				in = append(in, a)
			}
		}
		if len(in) == 0 {
			continue
		}
		fmt.Fprintf(b, "\n### %s\n\n", g.title)
		for _, a := range in {
			flags := []string{a.typ}
			if a.sensitive {
				flags = append(flags, "Sensitive")
			}
			if a.wo {
				flags = append(flags, "[Write-only](https://developer.hashicorp.com/terraform/language/resources/ephemeral#write-only-arguments)")
			}
			link := ""
			if len(a.nested) > 0 {
				link = fmt.Sprintf(" (see [below for nested schema](#%s--%s))", anchor, a.name)
				nested = append(nested, a)
			}
			fmt.Fprintf(b, "- `%s` (%s) %s%s\n", a.name, strings.Join(flags, ", "), a.desc, link)
		}
	}
	for _, n := range nested {
		fmt.Fprintf(b, "\n<a id=\"%s--%s\"></a>\n### Nested Schema for `%s`\n", anchor, n.name, n.name)
		writeAttrs(b, n.nested, anchor+"--"+n.name)
	}
}

func readExample(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", rel))
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\n")
}

func frontmatter(title, desc string) string {
	first := strings.SplitN(desc, ". ", 2)[0]
	first = strings.TrimSuffix(first, ".") + "."
	return fmt.Sprintf("---\npage_title: %q\nsubcategory: \"\"\ndescription: |-\n  %s\n---\n\n", title, first)
}

func renderResource(t *testing.T, name string, s rschema.Schema) string {
	var b strings.Builder
	b.WriteString(frontmatter(name+" Resource - spaceman", s.MarkdownDescription))
	fmt.Fprintf(&b, "# %s (Resource)\n\n%s\n", name, s.MarkdownDescription)
	if ex := readExample(t, filepath.Join("resources", name, "resource.tf")); ex != "" {
		fmt.Fprintf(&b, "\n## Example Usage\n\n```terraform\n%s\n```\n", ex)
	}
	b.WriteString("\n<!-- schema generated by internal/provider/docs_test.go -->\n## Schema\n")
	writeAttrs(&b, resourceAttrs(s.Attributes), "nestedatt")
	if ex := readExample(t, filepath.Join("resources", name, "import.sh")); ex != "" {
		fmt.Fprintf(&b, "\n## Import\n\nImport is supported using the following syntax:\n\n```shell\n%s\n```\n", ex)
	}
	return b.String()
}

func renderDataSource(t *testing.T, name string, s dschema.Schema) string {
	var b strings.Builder
	b.WriteString(frontmatter(name+" Data Source - spaceman", s.MarkdownDescription))
	fmt.Fprintf(&b, "# %s (Data Source)\n\n%s\n", name, s.MarkdownDescription)
	if ex := readExample(t, filepath.Join("data-sources", name, "data-source.tf")); ex != "" {
		fmt.Fprintf(&b, "\n## Example Usage\n\n```terraform\n%s\n```\n", ex)
	}
	b.WriteString("\n<!-- schema generated by internal/provider/docs_test.go -->\n## Schema\n")
	writeAttrs(&b, dataAttrs(s.Attributes), "nestedatt")
	return b.String()
}

const indexGuide = `
## Authentication

The provider sends ` + "`Authorization: Bearer <token>`" + ` with an API token (` + "`spaceman_at_…`" + `) or the bootstrap root token
(` + "`spaceman_rt_…`" + `). Prefer an API token of a dedicated automation user with the grants the configuration needs. Plain
` + "`http://`" + ` endpoints are accepted only on loopback addresses unless ` + "`allow_insecure_http`" + ` is set. Redirects are never followed,
so the token never leaves the configured origin.

## What Terraform manages

Durable configuration only: spaces and their roles, account bindings, accounts, harness profiles, role templates, prompts,
skills, MCP servers and grants, event connections, templates and subscriptions, human-action types and their enablement,
approval policies, knowledge bases, dream configurations, and OIDC providers with group mappings. Runtime state (runs,
tasks, inbox items, messages, human-action requests and responses, approvals, recordings) is never managed.

## Drift and ownership

The web UI uses the same API, so a UI edit of a field the configuration sets shows as ordinary drift and the next apply
restores the configured value. An optional field the configuration leaves out is not owned by Terraform: its current value
(edited in the UI, or SpaceMan's default) is kept and never shows as drift. Removing such a field from the configuration
therefore leaves the last value in place instead of clearing it.

Every update sends ` + "`If-Match`" + ` with the object version read during the last refresh (kept in private state), so an apply never
overwrites a change made after the plan was computed: it fails with a conflict and the next plan shows the change. Creates
send an ` + "`Idempotency-Key`" + ` where the API accepts one and are retried safely on transient errors.

## Secrets: write-only arguments

Credentials (account secrets, MCP request headers, Pulse ` + "`.creds`" + `, OIDC client secrets) are
[write-only arguments](https://developer.hashicorp.com/terraform/language/resources/ephemeral#write-only-arguments)
(Terraform 1.11 or later): they are sent to SpaceMan, sealed there and never returned, and Terraform never stores them in a plan
or state. Because Terraform cannot see a write-only value change, each has a companion ` + "`<name>_version`" + ` number: change it
to send (rotate) the value. Feed write-only arguments from ` + "`ephemeral`" + ` variables or ephemeral resources.

## Immutable versions

Prompts, skills, event templates and human-action types publish immutable versions. Their content attributes describe the
latest published version; a change publishes exactly one new version, and an unchanged configuration never publishes again.
An unpublished draft (for example started in the UI) is not drift; Terraform replaces it when it publishes. Subscriptions
that follow a template's latest version move with it without planning a change.

## What cannot be deleted

SpaceMan keeps catalog history: prompts, skills, event templates, human-action types, harness profiles and role templates
have no delete API, so destroying them only removes them from Terraform's state (with a warning). Knowledge bases and dream
configurations are disabled on destroy. Spaces are archived on destroy and purged only with ` + "`purge_on_destroy`" + `.
Identifying fields the API cannot change (slugs, kinds, a prompt's name) are refused at plan time instead of replacing the
object.

## Node pinning

A space is pinned to its node. A change of ` + "`spaceman_space.node_id`" + ` is planned as an in-place update and applied as a SpaceMan
relocation (` + "`POST /spaces/{id}/relocation`" + `): the source is checked, memory and uploads are synced, unpushed, changed or untracked
state is refused unless ` + "`relocation_dirty_state = \"export\"`" + `, the destination is prepared, and the assignment and execution lease
move in one step. The space must be paused with no active runs, or set ` + "`relocation_stop_runs = true`" + `; it stays paused on the
new node. A refused relocation fails the apply and stays blocked in SpaceMan; applying again resumes it. The provider never
destroys and recreates a space to move it, and it does not offer the unavailable-source flow (an explicit operator
acknowledgement in the UI or API). Nodes themselves are enrolled with a one-time token outside Terraform and are read with
the ` + "`spaceman_node`" + ` data sources.
`

func renderIndex(t *testing.T) string {
	ctx := context.Background()
	var sr pschema.SchemaResponse
	New("test")().Schema(ctx, pschema.SchemaRequest{}, &sr)
	var b strings.Builder
	b.WriteString(frontmatter("Provider: SpaceMan", "Manages the durable configuration of a SpaceMan central."))
	fmt.Fprintf(&b, "# SpaceMan Provider\n\n%s\n", sr.Schema.MarkdownDescription)
	if ex := readExample(t, filepath.Join("provider", "provider.tf")); ex != "" {
		fmt.Fprintf(&b, "\n## Example Usage\n\n```terraform\n%s\n```\n", ex)
	}
	b.WriteString(indexGuide)
	b.WriteString("\n<!-- schema generated by internal/provider/docs_test.go -->\n## Schema\n\n### Optional\n\n")
	names := make([]string, 0, len(sr.Schema.Attributes))
	for n := range sr.Schema.Attributes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		a := sr.Schema.Attributes[n]
		typ := "String"
		switch {
		case strings.HasPrefix(n, "allow_"):
			typ = "Boolean"
		case strings.HasSuffix(n, "_seconds"):
			typ = "Number"
		}
		if a.IsSensitive() {
			typ += ", Sensitive"
		}
		fmt.Fprintf(&b, "- `%s` (%s) %s\n", n, typ, a.GetMarkdownDescription())
	}
	return b.String()
}

func renderDocs(t *testing.T) map[string]string {
	t.Helper()
	ctx := context.Background()
	p := New("test")()
	out := map[string]string{"index.md": renderIndex(t)}
	for _, f := range p.Resources(ctx) {
		r := f()
		var md resource.MetadataResponse
		r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: "spaceman"}, &md)
		var sr resource.SchemaResponse
		r.Schema(ctx, resource.SchemaRequest{}, &sr)
		out[filepath.Join("resources", strings.TrimPrefix(md.TypeName, "spaceman_")+".md")] = renderResource(t, md.TypeName, sr.Schema)
	}
	for _, f := range p.DataSources(ctx) {
		d := f()
		var md datasource.MetadataResponse
		d.Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: "spaceman"}, &md)
		var sr datasource.SchemaResponse
		d.Schema(ctx, datasource.SchemaRequest{}, &sr)
		out[filepath.Join("data-sources", strings.TrimPrefix(md.TypeName, "spaceman_")+".md")] = renderDataSource(t, md.TypeName, sr.Schema)
	}
	return out
}

func TestDocsUpToDate(t *testing.T) {
	docs := renderDocs(t)
	root := filepath.Join("..", "..", "docs")
	if os.Getenv("SM_TF_DOCS") == "1" {
		for rel, content := range docs {
			p := filepath.Join(root, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	for rel, want := range docs {
		got, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil || string(got) != want {
			t.Errorf("docs/%s is stale: regenerate with SM_TF_DOCS=1 go test ./internal/provider -run TestDocsUpToDate", rel)
		}
	}
	// Every resource and data source has an example.
	for rel := range docs {
		if rel == "index.md" {
			continue
		}
		dir, name := filepath.Split(rel)
		name = "spaceman_" + strings.TrimSuffix(name, ".md")
		file := "resource.tf"
		if strings.HasPrefix(dir, "data-sources") {
			file = "data-source.tf"
		}
		if readExample(t, filepath.Join(filepath.Clean(dir), name, file)) == "" {
			t.Errorf("no example for %s", name)
		}
	}
}
