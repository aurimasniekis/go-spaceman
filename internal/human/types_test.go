package human

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
)

const deployReq = `{"type":"object","required":["environment","window"],"properties":{"environment":{"type":"string","enum":["staging","production"]},` +
	`"window":{"type":"string"},"rollback":{"type":"string"}},"additionalProperties":false}`
const deployResp = `{"type":"object","required":["start"],"properties":{"start":{"type":"string"},"conditions":{"type":"string"}},"additionalProperties":false}`

func (f *fx) deployType(t *testing.T) *HumanType {
	t.Helper()
	ht, err := f.svc.CreateType(f.ctx, op, "deploy-window", "Deploy window", "Agree on a deployment window", TypeDraft{RequestSchema: json.RawMessage(deployReq),
		ResponseSchema: json.RawMessage(deployResp), UI: json.RawMessage(`{"order":["environment","window"],"fields":{"window":{"label":"Window","widget":"text"}}}`),
		Outcomes: []string{"scheduled", "declined"}, DefaultPriority: "high", DefaultExpiryMinutes: 120})
	must(t, err)
	ht, err = f.svc.PublishType(f.ctx, op, ht.ID, ht.Versions[0].Version)
	must(t, err)
	return ht
}

// SM-HTYPE-001/002/003: a central catalog with immutable versions, strict schemas and safe UI metadata only.
func TestTypeCatalog(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	for name, d := range map[string]TypeDraft{
		"not object":     {RequestSchema: json.RawMessage(`{"type":"string"}`)},
		"bad schema":     {RequestSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"nope"}}}`)},
		"remote ref":     {ResponseSchema: json.RawMessage(`{"type":"object","properties":{"a":{"$ref":"https://example.test/s.json"}}}`)},
		"script in ui":   {UI: json.RawMessage(`{"fields":{"a":{"label":"x","onClick":"alert(1)"}}}`)},
		"unknown widget": {UI: json.RawMessage(`{"fields":{"a":{"widget":"iframe"}}}`)},
		"component":      {UI: json.RawMessage(`{"component":"<script>alert(1)</script>"}`)},
		"bad outcome":    {Outcomes: []string{"Approved!"}},
		"bad priority":   {DefaultPriority: "asap"},
	} {
		_, err := f.svc.CreateType(f.ctx, op, "t-"+strings.ReplaceAll(name, " ", "-"), "T", "", d)
		if apperr.CodeOf(err) != apperr.CodeInvalid {
			t.Fatalf("%s: %v", name, err)
		}
	}
	ht := f.deployType(t)
	if ht.Latest != 1 || len(ht.Versions) != 1 || ht.Versions[0].DefaultPriority != "high" {
		t.Fatalf("type %+v", ht)
	}
	_, err := f.svc.CreateType(f.ctx, op, "deploy-window", "Dup", "", TypeDraft{})
	code(t, err, apperr.CodeAlreadyExists)
	_, err = f.svc.PublishType(f.ctx, op, ht.ID, 1)
	code(t, err, apperr.CodePrecondition) // no draft
	// A new draft and version; the published one is unchanged.
	ht, err = f.svc.SaveTypeDraft(f.ctx, op, ht.ID, 0, TypeDraft{RequestSchema: json.RawMessage(`{"type":"object","required":["ticket"]}`), Outcomes: []string{"done"}})
	must(t, err)
	var draft *TypeVersion
	for _, v := range ht.Versions {
		if v.State == "draft" {
			draft = v
		}
	}
	_, err = f.svc.SaveTypeDraft(f.ctx, op, ht.ID, 0, TypeDraft{})
	code(t, err, apperr.CodePrecondition)
	ht, err = f.svc.PublishType(f.ctx, op, ht.ID, draft.Version)
	must(t, err)
	if ht.Latest != 2 {
		t.Fatalf("latest %d", ht.Latest)
	}
	l, err := f.svc.ListTypes(f.ctx)
	must(t, err)
	if len(l) != 1 || len(l[0].Versions) != 2 {
		t.Fatalf("list %+v", l)
	}
	if got, err := f.svc.GetType(f.ctx, "deploy-window"); err != nil || got.ID != ht.ID {
		t.Fatalf("by slug %v", err)
	}
}

// SM-HTYPE-002/004/005/006: spaces enable types (pinned or following, as variants); requests and responses are
// validated server-side; actions keep their type version; custom outcomes grant nothing.
func TestCustomActions(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	ht := f.deployType(t)
	v1 := ht.Versions[0]
	_, _, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeCustom, TypeSlug: "deploy-window", Title: "Deploy v2.14", Requester: agent,
		Request: json.RawMessage(`{"environment":"production","window":"Sunday 06:00"}`)})
	code(t, err, apperr.CodeInvalid) // not enabled in the space
	code(t, f.svc.EnableType(f.ctx, op, "spc_a", ht.ID, "prv_nope", "", "", 0), apperr.CodeInvalid)
	must(t, f.svc.EnableType(f.ctx, op, "spc_a", ht.ID, "", "Production deploy window", "urgent", 0))
	sts, err := f.svc.SpaceTypes(f.ctx, "spc_a")
	must(t, err)
	if len(sts) != 1 || sts[0].Label != "Production deploy window" || sts[0].DefaultPriority != "urgent" || sts[0].Number != 1 || sts[0].Pinned {
		t.Fatalf("space types %+v", sts)
	}
	_, _, err = f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeCustom, TypeSlug: "deploy-window", Title: "x", Requester: agent,
		Request: json.RawMessage(`{"environment":"moon","window":"now"}`)})
	if apperr.CodeOf(err) != apperr.CodeInvalid || !strings.Contains(err.Error(), "/environment") {
		t.Fatalf("schema violation: %v", err)
	}
	a, _, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeCustom, TypeSlug: "deploy-window", Title: "Deploy v2.14", Requester: agent,
		Request: json.RawMessage(`{"environment":"production","window":"Sunday 06:00"}`)})
	must(t, err)
	if a.TypeVersion != 1 || a.TypeVersionID != v1.ID || a.Priority != "urgent" || a.ExpiresAt == nil || !a.ExpiresAt.Equal(f.clk.Now().Add(120*time.Minute)) {
		t.Fatalf("custom action %+v", a)
	}
	// A breaking new version does not affect the outstanding action.
	ht, err = f.svc.SaveTypeDraft(f.ctx, op, ht.ID, 0, TypeDraft{RequestSchema: json.RawMessage(`{"type":"object","required":["ticket"]}`),
		ResponseSchema: json.RawMessage(`{"type":"object","required":["ticket"]}`), Outcomes: []string{"done"}})
	must(t, err)
	var d *TypeVersion
	for _, v := range ht.Versions {
		if v.State == "draft" {
			d = v
		}
	}
	_, err = f.svc.PublishType(f.ctx, op, ht.ID, d.Version)
	must(t, err)
	_, err = f.svc.Respond(f.ctx, op, "spc_a", a.ID, 0, RespondInput{Outcome: "done"})
	code(t, err, apperr.CodeInvalid) // v1 outcomes are scheduled or declined
	_, err = f.svc.Respond(f.ctx, op, "spc_a", a.ID, 0, RespondInput{Outcome: "scheduled", Response: Response{Data: json.RawMessage(`{"conditions":"x"}`)}})
	code(t, err, apperr.CodeInvalid) // start is required by v1's response schema
	got, err := f.svc.Respond(f.ctx, op, "spc_a", a.ID, 0, RespondInput{Outcome: "scheduled", Response: Response{Data: json.RawMessage(`{"start":"Sunday 06:00"}`)}})
	must(t, err)
	var resp Response
	_ = json.Unmarshal(got.Response, &resp)
	if got.Outcome != "scheduled" || string(resp.Data) != `{"start":"Sunday 06:00"}` {
		t.Fatalf("response %+v", got)
	}
	// New actions follow the latest version; a pin keeps v1.
	b, _, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeCustom, TypeSlug: "deploy-window", Title: "Next", Requester: agent, Request: json.RawMessage(`{"ticket":"OPS-1"}`)})
	must(t, err)
	if b.TypeVersion != 2 {
		t.Fatalf("follow %+v", b)
	}
	must(t, f.svc.EnableType(f.ctx, op, "spc_a", ht.ID, v1.ID, "", "", 0))
	c, _, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeCustom, TypeSlug: "deploy-window", Title: "Pinned", Requester: agent,
		Request: json.RawMessage(`{"environment":"staging","window":"now"}`)})
	must(t, err)
	if c.TypeVersion != 1 {
		t.Fatalf("pin %+v", c)
	}
	// Built-in types never take response data; disabling a type stops new actions but not outstanding ones.
	q, _, _ := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeQuestion, Title: "Q", Requester: agent})
	_, err = f.svc.Respond(f.ctx, op, "spc_a", q.ID, 0, RespondInput{Outcome: "answered", Response: Response{Answer: "y", Data: json.RawMessage(`{"a":1}`)}})
	code(t, err, apperr.CodeInvalid)
	must(t, f.svc.DisableType(f.ctx, op, "spc_a", ht.ID))
	code(t, f.svc.DisableType(f.ctx, op, "spc_a", ht.ID), apperr.CodeNotFound)
	_, _, err = f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeCustom, TypeSlug: "deploy-window", Title: "x", Requester: agent, Request: json.RawMessage(`{"ticket":"t"}`)})
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.Respond(f.ctx, op, "spc_a", c.ID, 0, RespondInput{Outcome: "declined", Response: Response{Data: json.RawMessage(`{"start":"never"}`)}})
	must(t, err)
	_, _, err = f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeCustom, Title: "x", Requester: agent})
	code(t, err, apperr.CodeInvalid) // no type slug
}
