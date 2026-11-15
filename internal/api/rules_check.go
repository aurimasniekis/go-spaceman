package api

import (
	"sort"

	"github.com/danielgtaylor/huma/v2"
)

// MissingRules lists the operation IDs in spec that have no authorization rule (tests and startup checks).
func MissingRules(spec *huma.OpenAPI, d *Deps) []string {
	var missing []string
	for _, item := range spec.Paths {
		for _, op := range []*huma.Operation{item.Get, item.Post, item.Put, item.Patch, item.Delete} {
			if op == nil {
				continue
			}
			if _, ok := d.ruleFor(op.OperationID); !ok {
				missing = append(missing, op.OperationID)
			}
		}
	}
	sort.Strings(missing)
	return missing
}

// RuleFor returns the authorization rule of an operation (core or Extra).
func (d *Deps) RuleFor(op string) (Rule, bool) { return d.ruleFor(op) }
