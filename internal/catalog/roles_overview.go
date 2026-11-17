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
}

// RoleOverviews lists a space's roles sorted by name, each with its effective description, harness and model
// (resolved as a launch of that role would, so the policy applies). A role that cannot resolve is left out.
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
			continue
		}
		r, err := svc.Resolve(ctx, q, sp, Launch{Role: row.Name})
		if err != nil {
			continue
		}
		out = append(out, RoleOverview{Name: row.Name, Kind: r.Kind, Description: r.Description, DescriptionOrigin: r.DescriptionOrigin,
			Template: tpl.Slug, Harness: r.Harness, Model: r.Model})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
