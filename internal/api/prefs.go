package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// NotificationPrefs are a user's notification preferences (SM-HUI-006). They decide which human-action changes the
// web UI announces to the user while it is open, and whether it also shows a desktop notification. SpaceMan sends no
// e-mail or chat notifications; external systems subscribe to the outbound lifecycle events instead.
type NotificationPrefs struct {
	Enabled        bool     `json:"enabled" doc:"Announce human-action changes in the web UI"`
	Desktop        bool     `json:"desktop" doc:"Also show a desktop notification (the browser asks for permission)"`
	NewActions     bool     `json:"new_actions" doc:"A new pending action in a space you can read"`
	AssignedToMe   bool     `json:"assigned_to_me" doc:"An action was assigned to you"`
	Escalations    bool     `json:"escalations" doc:"An action escalated"`
	Reminders      bool     `json:"reminders" doc:"A reminder for a pending action"`
	Clarifications bool     `json:"clarifications" doc:"Replies to clarification requests, and clarification requests on actions you asked"`
	MinPriority    string   `json:"min_priority" enum:"low,normal,high,urgent" doc:"Only actions of at least this priority (assignments to you always count)"`
	MutedSpaces    []string `json:"muted_spaces" maxItems:"200" doc:"Spaces whose actions are never announced"`
}

// NotificationPrefsOut is the stored preferences with their version (0: defaults, never saved).
type NotificationPrefsOut struct {
	NotificationPrefs
	Version   int64      `json:"version"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// DefaultNotificationPrefs are the preferences of a user who never saved any.
func DefaultNotificationPrefs() NotificationPrefs {
	return NotificationPrefs{Enabled: true, NewActions: true, AssignedToMe: true, Escalations: true, Clarifications: true, MinPriority: "low",
		MutedSpaces: []string{}}
}

func (d *Deps) notificationPrefs(ctx context.Context, userID string) (*NotificationPrefsOut, error) {
	r, err := d.Store.R().GetNotificationPrefs(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return &NotificationPrefsOut{NotificationPrefs: DefaultNotificationPrefs()}, nil
	} else if err != nil {
		return nil, err
	}
	out := &NotificationPrefsOut{NotificationPrefs: DefaultNotificationPrefs(), Version: r.Version}
	if err := json.Unmarshal([]byte(r.PrefsJson), &out.NotificationPrefs); err != nil {
		return nil, err
	}
	if out.MutedSpaces == nil {
		out.MutedSpaces = []string{}
	}
	t := time.UnixMicro(r.UpdatedAt).UTC()
	out.UpdatedAt = &t
	return out, nil
}

func registerPrefs(api huma.API, d *Deps) {
	tags := []string{"auth"}
	huma.Register(api, huma.Operation{OperationID: "get-notification-preferences", Method: http.MethodGet, Path: "/auth/me/notification-preferences",
		Summary: "Your notification preferences", Description: "Defaults (version 0) until you save them.", Tags: tags},
		func(ctx context.Context, _ *struct{}) (*struct {
			ETag string `header:"ETag"`
			Body *NotificationPrefsOut
		}, error) {
			p, err := d.notificationPrefs(ctx, PrincipalFrom(ctx).User.ID)
			if err != nil {
				return nil, d.fail(ctx, "prefs.get", err)
			}
			return &struct {
				ETag string `header:"ETag"`
				Body *NotificationPrefsOut
			}{ETag: ETag(p.Version), Body: p}, nil
		})
	huma.Register(api, huma.Operation{OperationID: "set-notification-preferences", Method: http.MethodPut, Path: "/auth/me/notification-preferences",
		Summary: "Save your notification preferences", Description: "Send If-Match with the version you read (\"0\" for the defaults).", Tags: tags},
		func(ctx context.Context, in *struct {
			IfMatch
			Body NotificationPrefs
		}) (*struct {
			ETag string `header:"ETag"`
			Body *NotificationPrefsOut
		}, error) {
			v := int64(0) // "0": the defaults, never saved
			if m := strings.TrimPrefix(strings.TrimSpace(in.IfMatch.IfMatch), "W/"); m != `"0"` {
				var err error
				if v, err = in.Version(); err != nil {
					return nil, d.fail(ctx, "prefs.set", err)
				}
			}
			b := in.Body
			if b.MinPriority == "" {
				b.MinPriority = "low"
			}
			if b.MutedSpaces == nil {
				b.MutedSpaces = []string{}
			}
			raw, _ := json.Marshal(b)
			uid := PrincipalFrom(ctx).User.ID
			err := d.Store.W().TxNamed(ctx, "prefs.set", func(tx *store.Tx) error {
				var n int64
				var err error
				if v == 0 {
					n, err = tx.Q.InsertNotificationPrefs(ctx, db.InsertNotificationPrefsParams{UserID: uid, PrefsJson: string(raw), UpdatedAt: tx.NowMicros()})
				} else {
					n, err = tx.Q.UpdateNotificationPrefs(ctx, db.UpdateNotificationPrefsParams{PrefsJson: string(raw), UpdatedAt: tx.NowMicros(), UserID: uid, Version: v})
				}
				if err != nil {
					return err
				}
				if n == 0 {
					return apperr.New(apperr.CodePrecondition, "your preferences changed since version %d; reload them", v)
				}
				_, err = d.Audit.Append(ctx, tx, audit.Event{Actor: Actor(ctx), Action: "user.notification_preferences_saved", Target: audit.Target{Type: "user", ID: uid},
					Details: map[string]any{"enabled": b.Enabled, "desktop": b.Desktop}})
				return err
			})
			if err != nil {
				return nil, d.fail(ctx, "prefs.set", err)
			}
			p, err := d.notificationPrefs(ctx, uid)
			if err != nil {
				return nil, d.fail(ctx, "prefs.set", err)
			}
			return &struct {
				ETag string `header:"ETag"`
				Body *NotificationPrefsOut
			}{ETag: ETag(p.Version), Body: p}, nil
		})
}
