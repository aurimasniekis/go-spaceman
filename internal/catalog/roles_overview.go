package catalog

import (
	"context"
	"sort"
)

// RoleOverview is what control may know about one of its space's roles: what it is for and what it runs on.
// It never carries prompt text, settings, skills or account references (ADR-0095).
type RoleOverview struct {
	Name              string `json:"name"`
	Kind              string `json:"kind"`
	Description       string `json:"description,omitempty"`
	DescriptionOrigin string `json:"description_origin,omitempty"`
	Template          string `json:"template"`
	Harness           string `json:"harness,omitempty"`
	Model             string `json:"model,omitempty"`
	// Error is set when the role cannot resolve (a missing profile, prompt or template); it names the problem, never content.
	Error string `json:"error,omitempty"`
}

// callerHarness stands in for the harness a launch of a profile-less role would get from its caller, so such a role
// still resolves; the overview then reports its harness as empty.
const callerHarness = "caller-default"

// RoleOverviews lists a space's roles sorted by name, each with its effective description, harness and model
// (resolved as a launch of that role would, so the policy applies). A profile-less role has an empty harness (a launch defaults it to the caller's); a role that cannot resolve is listed with an error.
func (svc *Service) RoleOverviews(ctx context.Context, spaceID string) ([]RoleOverview, error) {
	q := svc.s.R()
	sp, err := q.GetSpace(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	rows, err := q.ListSpaceRoles(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make([]RoleOverview, 0, len(rows))
	for _, row := range rows {
		tpl, err := q.GetRoleTemplate(ctx, row.TemplateID)
		if err != nil {
			out = append(out, RoleOverview{Name: row.Name, Error: "the role refers to a missing role template"})
			continue
		}
		launch := Launch{Role: row.Name}
		if row.ProfileID == "" && tpl.ProfileID == "" {
			launch.Harness = callerHarness
		}
		r, err := svc.Resolve(ctx, q, sp, launch)
		if err != nil {
			out = append(out, RoleOverview{Name: row.Name, Kind: tpl.Kind, Template: tpl.Slug, Error: err.Error()})
			continue
		}
		harness := r.Harness
		if harness == callerHarness {
			harness = ""
		}
		out = append(out, RoleOverview{Name: row.Name, Kind: r.Kind, Description: r.Description, DescriptionOrigin: r.DescriptionOrigin,
			Template: tpl.Slug, Harness: harness, Model: r.Model})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
