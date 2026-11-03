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
	"github.com/aurimasniekis/go-spaceman/internal/oidc"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// SystemInfo is the public bootstrap and status summary the UI needs before sign-in.
type SystemInfo struct {
	Version string `json:"version"`
	Ready   bool   `json:"ready"`
	// Bootstrapped is false until a root token was issued (run `spaceman admin bootstrap`).
	Bootstrapped bool `json:"bootstrapped"`
	// HasAdmin is false until the first administrator exists (the UI then shows /setup).
	HasAdmin bool `json:"has_admin"`
	// LocalLoginDisabled is SSO-only enforcement: the sign-in page offers only the SSO providers (ADR-0045).
	LocalLoginDisabled bool `json:"local_login_disabled"`
	// DirectProvider is the slug of the provider the sign-in page goes to directly ("": none).
	DirectProvider string `json:"direct_provider"`
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
			if out.Body.DirectProvider, err = oidc.DirectProvider(ctx, d.Store.R()); err != nil {
				return nil, d.fail(ctx, "system.info", err)
			}
			return out, nil
		})
}

// AuditRecord is one audit event as shown to administrators (sensitive details are never returned).
type AuditRecord = audit.Record

type auditPage struct {
	Items      []AuditRecord `json:"items"`
	NextCursor *string       `json:"next_cursor" doc:"Newest-first paging: the cursor of the next (older) page"`
	// NextAfter is the durable forward position after this page (with after=): store it and pass it as after= to resume.
	// It equals the requested after when the page is empty.
	NextAfter *int64 `json:"next_after,omitempty" doc:"Forward paging (after=): the position to store and resume from"`
	// Gap is set when retention deleted events after the requested position: the consumer missed them.
	Gap *AuditGap `json:"gap,omitempty"`
	// Pruned is the audit log's pruned prefix, when retention deleted events.
	Pruned *audit.Pruned `json:"pruned,omitempty"`
}

// AuditGap reports that audit retention deleted events after a reader's position, like the change feed's gap event:
// the events after After and before Oldest were deleted and cannot be read.
type AuditGap struct {
	After  int64 `json:"after"`
	Oldest int64 `json:"oldest" doc:"The first retained sequence"`
}

func registerAudit(api huma.API, d *Deps) {
	huma.Register(api, huma.Operation{OperationID: "list-audit", Method: http.MethodGet, Path: "/audit", Summary: "Audit events, newest first or forward from a durable position",
		Description: "Without after, events are returned newest first and paged with cursor. With after (a sequence number, 0 for the start), " +
			"events with a higher sequence are returned oldest first; next_after is the position to store and resume from. When audit " +
			"retention deleted events after that position, gap says so (after, oldest retained sequence).",
		Tags: []string{"audit"}},
		func(ctx context.Context, in *struct {
			Page
			After    string `query:"after" pattern:"^[0-9]{1,19}$" doc:"Forward paging: return events after this sequence, oldest first (0: from the start)"`
			Action   string `query:"action" maxLength:"128"`
			TargetID string `query:"target_id" maxLength:"64"`
			ActorID  string `query:"actor_id" maxLength:"64"`
		}) (*struct{ Body auditPage }, error) {
			pr, err := audit.PrunedPrefix(ctx, d.Store.R())
			if err != nil {
				return nil, d.fail(ctx, "audit.list", err)
			}
			out := &struct{ Body auditPage }{Body: auditPage{Items: []AuditRecord{}, Pruned: pr}}
			if in.After != "" {
				if in.Cursor != "" {
					return nil, d.fail(ctx, "audit.list", apperr.New(apperr.CodeInvalid, "use either cursor (newest first) or after (forward), not both"))
				}
				after, err := strconv.ParseInt(in.After, 10, 64)
				if err != nil {
					return nil, d.fail(ctx, "audit.list", apperr.New(apperr.CodeInvalid, "invalid after"))
				}
				if pr != nil && after < pr.Through {
					out.Body.Gap = &AuditGap{After: after, Oldest: pr.Through + 1}
				}
				rows, err := d.Store.R().ListAuditForward(ctx, db.ListAuditForwardParams{AfterSeq: after, Lim: int64(in.limit()), Action: nullStr(in.Action),
					TargetID: nullStr(in.TargetID), ActorID: nullStr(in.ActorID)})
				if err != nil {
					return nil, d.fail(ctx, "audit.list", err)
				}
				next := after
				for _, r := range rows {
					out.Body.Items = append(out.Body.Items, audit.DecodeRecord(r))
					next = r.Seq
				}
				out.Body.NextAfter = &next
				return out, nil
			}
			_, before, err := DecodeCursor(in.Cursor)
			if err != nil {
				return nil, d.fail(ctx, "audit.list", err)
			}
			params := db.ListAuditPageParams{Lim: int64(in.limit()) + 1, Action: nullStr(in.Action), TargetID: nullStr(in.TargetID), ActorID: nullStr(in.ActorID)}
			if before != "" {
				seq, err := strconv.ParseInt(before, 10, 64)
				if err != nil {
					return nil, d.fail(ctx, "audit.list", apperr.New(apperr.CodeInvalid, "invalid cursor"))
				}
				params.BeforeSeq = sql.NullInt64{Int64: seq, Valid: true}
			}
			rows, err := d.Store.R().ListAuditPage(ctx, params)
			if err != nil {
				return nil, d.fail(ctx, "audit.list", err)
			}
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
