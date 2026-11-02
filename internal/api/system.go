package api

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/buildinfo"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// SystemInfo is the public bootstrap and status summary the UI needs before sign-in.
type SystemInfo struct {
	Version string `json:"version"`
	Ready   bool   `json:"ready"`
	// Bootstrapped is false until a root token was issued (run `spaceman admin bootstrap`).
	Bootstrapped bool `json:"bootstrapped"`
	// HasAdmin is false until the first administrator exists (the UI then shows /setup).
	HasAdmin           bool `json:"has_admin"`
	LocalLoginDisabled bool `json:"local_login_disabled"`
}

func registerSystem(api huma.API, d *Deps) {
	huma.Register(api, huma.Operation{OperationID: "get-system-info", Method: http.MethodGet, Path: "/system/info", Summary: "Public system status",
		Tags: []string{"system"}, Security: []map[string][]string{}},
		func(ctx context.Context, _ *struct{}) (*struct{ Body SystemInfo }, error) {
			out := &struct{ Body SystemInfo }{}
			out.Body.Version = buildinfo.Get().Version
			out.Body.Ready = d.Ready == nil || d.Ready()
			n, err := d.Store.R().CountRootTokens(ctx)
			if err != nil {
				return nil, d.fail(ctx, "system.info", err)
			}
			out.Body.Bootstrapped = n > 0
			admins, err := d.Store.R().CountEnabledGlobalAdmins(ctx)
			if err != nil {
				return nil, d.fail(ctx, "system.info", err)
			}
			out.Body.HasAdmin = admins > 0
			if out.Body.LocalLoginDisabled, err = d.Auth.LocalLoginDisabled(ctx); err != nil {
				return nil, d.fail(ctx, "system.info", err)
			}
			return out, nil
		})
}

// AuditRecord is one audit event as shown to administrators (sensitive details are never returned).
type AuditRecord = audit.Record

type auditPage struct {
	Items      []AuditRecord `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}

func registerAudit(api huma.API, d *Deps) {
	huma.Register(api, huma.Operation{OperationID: "list-audit", Method: http.MethodGet, Path: "/audit", Summary: "Audit events, newest first",
		Tags: []string{"audit"}},
		func(ctx context.Context, in *struct {
			Page
			Action   string `query:"action" maxLength:"128"`
			TargetID string `query:"target_id" maxLength:"64"`
			ActorID  string `query:"actor_id" maxLength:"64"`
		}) (*struct{ Body auditPage }, error) {
			_, after, err := DecodeCursor(in.Cursor)
			if err != nil {
				return nil, d.fail(ctx, "audit.list", err)
			}
			params := db.ListAuditPageParams{Lim: int64(in.limit()) + 1, Action: nullStr(in.Action), TargetID: nullStr(in.TargetID), ActorID: nullStr(in.ActorID)}
			if after != "" {
				seq, err := strconv.ParseInt(after, 10, 64)
				if err != nil {
					return nil, d.fail(ctx, "audit.list", apperr.New(apperr.CodeInvalid, "invalid cursor"))
				}
				params.BeforeSeq = sql.NullInt64{Int64: seq, Valid: true}
			}
			rows, err := d.Store.R().ListAuditPage(ctx, params)
			if err != nil {
				return nil, d.fail(ctx, "audit.list", err)
			}
			out := &struct{ Body auditPage }{Body: auditPage{Items: []AuditRecord{}}}
			for i, r := range rows {
				if i == in.limit() {
					c := EncodeCursor("", strconv.FormatInt(rows[i-1].Seq, 10))
					out.Body.NextCursor = &c
					break
				}
				out.Body.Items = append(out.Body.Items, audit.DecodeRecord(r))
			}
			return out, nil
		})

	huma.Register(api, huma.Operation{OperationID: "verify-audit-chain", Method: http.MethodPost, Path: "/audit/verify", Summary: "Verify the audit hash chain",
		Tags: []string{"audit"}},
		func(ctx context.Context, _ *struct{}) (*struct{ Body audit.VerifyResult }, error) {
			res, err := audit.Verify(ctx, d.Store.R())
			if err != nil {
				return nil, d.fail(ctx, "audit.verify", err)
			}
			return &struct{ Body audit.VerifyResult }{Body: res}, nil
		})
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
