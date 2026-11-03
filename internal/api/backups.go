package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/backup"
)

// BackupRules are the authorization rules of backups (SM-STORE-005, ADR-0031). Backups and restores are global
// administration. Restoring is never an API operation: it runs offline with `spaceman restore` on a stopped central.
// The restore notice is readable by every signed-in principal, because every operator must know that spaces were paused.
var BackupRules = map[string]Rule{
	"list-backups":        Global(authz.SystemRead),
	"create-backup":       Global(authz.BackupRun),
	"verify-backup":       Global(authz.BackupRun),
	"get-restore-state":   {Session: true},
	"acknowledge-restore": Global(authz.SystemWrite),
}

// BackupPath identifies a backup.
type BackupPath struct {
	BackupID string `path:"backup_id" pattern:"^bkp_[0-9a-z]{26}$"`
}

// RestoreNotice is the last restore from a backup, as shown to operators.
type RestoreNotice struct {
	Restored bool                 `json:"restored"`
	State    *backup.RestoreState `json:"state,omitempty"`
	// StillPaused counts the spaces the restore paused that are still paused (among those the caller may read).
	StillPaused int `json:"still_paused"`
}

// RegisterBackups registers the backup operations.
func RegisterBackups(svc *backup.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"backups"}
		huma.Register(api, huma.Operation{OperationID: "list-backups", Method: http.MethodGet, Path: "/system/backups", Summary: "Backups and the schedule",
			Description: "The configured target and schedule, what every backup includes and excludes, and the backups: runs made by central joined " +
				"with what the target holds (CLI-made backups are listed too).", Tags: tags},
			func(ctx context.Context, _ *struct{}) (*struct{ Body *backup.Status }, error) {
				st, err := svc.Status(ctx)
				if err != nil {
					return nil, d.fail(ctx, "backups.list", err)
				}
				return &struct{ Body *backup.Status }{Body: st}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "create-backup", Method: http.MethodPost, Path: "/system/backups", Summary: "Back up now",
			Description: "Starts a backup in the background and returns its run (state running). The change feed reports when it ends. One backup or " +
				"verification runs at a time.", Tags: tags, DefaultStatus: http.StatusAccepted},
			func(ctx context.Context, _ *struct{}) (*struct{ Body *backup.Item }, error) {
				it, err := svc.Start(ctx, Actor(ctx), backup.TriggerManual)
				if err != nil {
					return nil, d.fail(ctx, "backups.create", err)
				}
				return &struct{ Body *backup.Item }{Body: it}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "verify-backup", Method: http.MethodPost, Path: "/system/backups/{backup_id}/verify",
			Summary: "Verify a backup", Description: "Opens every sealed object with the key-encryption key, checks each file's size and SHA-256 against " +
				"the manifest and checks the database copy (integrity check, schema, keyring, audit chain), in the background.",
			Tags: tags, DefaultStatus: http.StatusAccepted},
			func(ctx context.Context, in *BackupPath) (*struct{ Body *backup.Item }, error) {
				it, err := svc.StartVerify(ctx, Actor(ctx), in.BackupID)
				if err != nil {
					return nil, d.fail(ctx, "backups.verify", err)
				}
				return &struct{ Body *backup.Item }{Body: it}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-restore-state", Method: http.MethodGet, Path: "/system/restore", Summary: "Restore notice",
			Description: "Whether this installation was restored from a backup, what the restore paused, stopped, reset and revoked, and how many of " +
				"the spaces it paused are still paused.", Tags: tags},
			func(ctx context.Context, _ *struct{}) (*struct{ Body RestoreNotice }, error) {
				out := &struct{ Body RestoreNotice }{}
				rs, err := backup.LoadRestoreState(ctx, d.Store.R())
				if err != nil {
					return nil, d.fail(ctx, "backups.restore_state", err)
				}
				if rs != nil {
					// Everyone learns that a restore paused spaces; only the spaces the caller may read are named.
					p := PrincipalFrom(ctx)
					global := p != nil && d.Authz != nil && d.Authz.AllowedAll(p.Subjects(), authz.Global, authz.SystemRead)
					cp := *rs
					cp.PausedSpaces = []string{}
					for _, id := range rs.PausedSpaces {
						if !global && (p == nil || d.Authz == nil || !d.Authz.AllowedAll(p.Subjects(), id, authz.SpaceRead)) {
							continue
						}
						cp.PausedSpaces = append(cp.PausedSpaces, id)
						if sp, err := d.Store.R().GetSpace(ctx, id); err == nil && sp.State == "paused" {
							out.Body.StillPaused++
						}
					}
					out.Body.Restored, out.Body.State = true, &cp
				}
				return out, nil
			})
		huma.Register(api, huma.Operation{OperationID: "acknowledge-restore", Method: http.MethodPost, Path: "/system/restore/acknowledge",
			Summary: "Dismiss the restore notice", Description: "Records that an administrator has seen the restore. Spaces stay paused until each is resumed.",
			Tags: tags},
			func(ctx context.Context, _ *struct{}) (*struct{ Body RestoreNotice }, error) {
				rs, err := svc.AcknowledgeRestore(ctx, Actor(ctx))
				if err != nil {
					return nil, d.fail(ctx, "backups.restore_ack", err)
				}
				return &struct{ Body RestoreNotice }{Body: RestoreNotice{Restored: rs != nil, State: rs}}, nil
			})
	}
}
