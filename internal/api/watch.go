package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// globalReadPerm maps the resource type of a global change (no space) to the permission needed to see it.
var globalReadPerm = map[string]authz.Permission{
	"user":     authz.UserRead,
	"node":     authz.NodeRead,
	"token":    authz.TokenAdmin,
	"template": authz.TemplateRead,
	"account":  authz.AccountRead,
}

const (
	watchBatch     = 200
	watchHeartbeat = 15 * time.Second
	watchMaxQueue  = 64
)

// watchHandler streams the durable change feed as Server-Sent Events (SM-API-002, SM-UI-006).
//
//	GET /api/v1/watch?after=<seq>   (or the Last-Event-ID header on reconnect)
//
// Events: "change" (data: Change JSON, id: seq), "gap" (retention pruned changes after the requested position: data
// {"after":…, "oldest":…}; the client must re-list), "heartbeat", and "end" (data {"reason":"unauthenticated"}) when the
// stream's session or token stops being valid. Each change is filtered by the subscriber's current permissions, so a
// removed grant hides the space's changes from the next one on. The stream guard re-validates the principal at once
// after a committed revocation and every StreamRevalidate otherwise (SM-AUTH-007, ADR-0033).
func (d *Deps) watchHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		p := PrincipalFrom(ctx)
		if p == nil {
			writeProblem(w, errUnauthenticated())
			return
		}
		if p.Limited {
			writeProblem(w, errLimited())
			return
		}
		after, err := parseAfter(r)
		if err != nil {
			writeProblem(w, newProblem(http.StatusUnprocessableEntity, "validation_failed", "after must be a non-negative integer", nil))
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			writeProblem(w, newProblem(http.StatusInternalServerError, "internal_error", "streaming unsupported", nil))
			return
		}
		var wake <-chan eventbus.Event
		if d.Bus != nil {
			sub := d.Bus.Subscribe(watchMaxQueue, eventbus.TopicChange, eventbus.TopicGrantsChanged, eventbus.TopicTokenRevoked, eventbus.TopicUserChanged)
			defer sub.Close()
			wake = sub.C
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		d.Metrics.SSEClientConnected()
		defer d.Metrics.SSEClientDisconnected()

		guard := d.guardStream(r, p, nil)
		defer guard.close()
		heartbeat := time.NewTicker(watchHeartbeat)
		defer heartbeat.Stop()

		// A position beyond the newest change comes from before a restore from backup (SM-STORE-006): the feed was
		// rewound, so the client must re-list instead of silently skipping the changes that reuse those positions.
		if latest, err := d.Store.R().LatestChangeSeq(ctx); err == nil && after > latest {
			gap, _ := json.Marshal(map[string]int64{"after": after, "oldest": latest + 1})
			if _, err := fmt.Fprintf(w, "event: gap\ndata: %s\n\n", gap); err != nil {
				return
			}
			after = latest
		}
		if after, err = d.watchCatchUp(ctx, w, p, after); err != nil {
			return
		}
		fl.Flush()
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
				if after, err = d.watchCatchUp(ctx, w, p, after); err != nil {
					return
				}
				fl.Flush()
			case <-heartbeat.C:
				if _, err := fmt.Fprint(w, "event: heartbeat\ndata: {}\n\n"); err != nil {
					return
				}
				fl.Flush()
			case <-guard.revoked():
				if reason := guard.check(ctx); reason != "" {
					endStream(w, fl, reason)
					return
				}
				p = guard.principal()
			case <-guard.due():
				if reason := guard.check(ctx); reason != "" {
					endStream(w, fl, reason)
					return
				}
				p = guard.principal()
			}
		}
	})
}

func parseAfter(r *http.Request) (int64, error) {
	v := r.URL.Query().Get("after")
	if v == "" {
		v = r.Header.Get("Last-Event-ID")
	}
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad after")
	}
	return n, nil
}

// revalidate re-checks the stream's principal without marking the session active (it must not defeat the idle timeout).
func (d *Deps) revalidate(ctx context.Context, r *http.Request, p *auth.Principal) (*auth.Principal, bool) {
	switch {
	case p.RootTokenID != "", p.TokenID != "":
		tok := r.Header.Get("Authorization")
		if len(tok) <= len(bearerPrefix) {
			return nil, false
		}
		var np *auth.Principal
		var err error
		if p.RootTokenID != "" {
			np, err = d.Auth.AuthenticateRootToken(ctx, tok[len(bearerPrefix):], ClientIP(ctx))
		} else {
			np, err = d.Auth.AuthenticateAPIToken(ctx, tok[len(bearerPrefix):], ClientIP(ctx))
		}
		return np, err == nil
	default:
		np, err := d.Auth.Validate(ctx, sessionTokenFrom(ctx))
		return np, err == nil && !np.Limited
	}
}

// watchCatchUp writes every retained change after `after` that p may see, and returns the new position. A position
// older than the retained window produces a gap event first.
func (d *Deps) watchCatchUp(ctx context.Context, w http.ResponseWriter, p *auth.Principal, after int64) (int64, error) {
	oldest, err := d.Store.R().OldestChangeSeq(ctx)
	if err != nil {
		return after, err
	}
	if oldest > 0 && after < oldest-1 && after > 0 {
		gap, _ := json.Marshal(map[string]int64{"after": after, "oldest": oldest})
		if _, err := fmt.Fprintf(w, "event: gap\ndata: %s\n\n", gap); err != nil {
			return after, err
		}
	}
	for {
		rows, err := d.Store.R().ChangesAfter(ctx, db.ChangesAfterParams{After: after, Lim: watchBatch})
		if err != nil {
			return after, err
		}
		for _, row := range rows {
			after = row.Seq
			c := changefeed.FromRow(row)
			if !d.canSee(p, c) {
				continue
			}
			b, _ := json.Marshal(c)
			if _, err := fmt.Fprintf(w, "event: change\nid: %d\ndata: %s\n\n", c.Seq, b); err != nil {
				return after, err
			}
		}
		if len(rows) < watchBatch {
			return after, nil
		}
	}
}

func (d *Deps) canSee(p *auth.Principal, c changefeed.Change) bool {
	if d.Authz == nil {
		return false
	}
	if c.SpaceID != "" {
		return d.Authz.AllowedAll(p.Subjects(), c.SpaceID, authz.SpaceRead)
	}
	perm, ok := globalReadPerm[c.ResourceType]
	if !ok {
		perm = authz.SystemRead
	}
	return d.Authz.AllowedAll(p.Subjects(), authz.Global, perm)
}
