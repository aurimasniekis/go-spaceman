package app_test

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Stream revocation (SM-AUTH-007, ADR-0033) through the real HTTP stack.

// revocationBound is how quickly a stream must end after a revocation commits. The tests that use it configure the
// periodic re-check at its maximum (60 s), so only the event-driven path can meet it.
const revocationBound = 5 * time.Second

type sseEvent struct{ typ, data string }

// openSSE opens a Server-Sent Events stream and delivers its events; the channel closes when the stream ends.
func openSSE(t *testing.T, hc *http.Client, url string, hdr ...string) <-chan sseEvent {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		res.Body.Close()
		t.Fatalf("stream %s: status %d", url, res.StatusCode)
	}
	t.Cleanup(func() { res.Body.Close() })
	ch := make(chan sseEvent, 256)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(res.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.typ != "" {
					ch <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "event: "):
				ev.typ = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return ch
}

// waitEnd skips events until the stream's "end" event and returns its reason. The stream must then close.
func waitEnd(t *testing.T, ch <-chan sseEvent, within time.Duration) string {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("stream closed without an end event")
			}
			if ev.typ != "end" {
				continue
			}
			var body struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal([]byte(ev.data), &body)
			select {
			case _, ok := <-ch:
				if ok {
					t.Fatal("events after end")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("stream not closed after end")
			}
			return body.Reason
		case <-deadline:
			t.Fatalf("stream still open %s after the revocation", within)
		}
	}
}

// nextChange returns the next change event, failing if the stream ends first.
func nextChange(t *testing.T, ch <-chan sseEvent) map[string]any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok || ev.typ == "end" {
				t.Fatalf("stream ended: %+v", ev)
			}
			if ev.typ == "change" {
				var c map[string]any
				_ = json.Unmarshal([]byte(ev.data), &c)
				return c
			}
		case <-deadline:
			t.Fatal("no change event")
		}
	}
}

// still asserts that the stream stays open (no end event) for d.
func still(t *testing.T, ch <-chan sseEvent, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-ch:
			if !ok || ev.typ == "end" {
				t.Fatalf("stream ended unexpectedly: %+v", ev)
			}
		case <-deadline:
			return
		}
	}
}

// signIn signs a local user in through the API (TOTP enrollment on the first sign-in) and returns the browser client.
func signIn(t *testing.T, h *harness, email string, secret []byte) (*client, api.Me, []byte) {
	t.Helper()
	ui := h.client()
	r := ui.do("POST", "/auth/login", map[string]string{"email": email, "password": "correct horse battery staple"})
	expect(t, r, 200)
	var me api.Me
	r.json(t, &me)
	ui.csrf = me.CSRFToken
	if secret == nil {
		r = ui.do("POST", "/auth/mfa/enroll", nil)
		var enr api.TOTPEnrollment
		r.json(t, &enr)
		secret, _ = auth.DecodeTOTPSecret(enr.Secret)
		expect(t, ui.do("POST", "/auth/mfa/enroll/confirm", map[string]string{"code": auth.TOTPCode(secret, auth.TOTPStep(h.clk.Now()))}), 200)
	}
	h.clk.Advance(30 * time.Second)
	r = ui.do("POST", "/auth/mfa/verify", map[string]string{"code": auth.TOTPCode(secret, auth.TOTPStep(h.clk.Now()))})
	expect(t, r, 200)
	r.json(t, &me)
	ui.csrf = me.CSRFToken
	return ui, me, secret
}

func createUser(t *testing.T, root *client, email string, grants ...map[string]string) string {
	t.Helper()
	r := root.do("POST", "/users", map[string]any{"email": email, "display_name": strings.Split(email, "@")[0],
		"password": "correct horse battery staple", "grants": grants})
	expect(t, r, 201)
	var u auth.User
	r.json(t, &u)
	return u.ID
}

func ifMatch(t *testing.T, h *harness, userID string) string {
	t.Helper()
	u, err := h.c.Auth.Get(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	return `"` + strconv.FormatInt(u.Version, 10) + `"`
}

var globalViewer = map[string]string{"role": "viewer", "domain": "*"}

// SM-AUTH-007: every kind of revocation ends an open change-feed stream at once (well inside the 60 s periodic
// re-check), with reason "unauthenticated", and the credential is refused on the next request.
func TestWatchStreamEndsOnRevocation(t *testing.T) {
	t.Parallel()
	h := newHarnessWith(t, func(c *config.Config) { c.Auth.StreamRevalidate = config.Duration(time.Minute) })
	ctx := context.Background()
	rootSecret, _, err := h.c.Auth.IssueRootToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	root := h.client()
	root.bearer = rootSecret
	createUser(t, root, "adm@example.com", map[string]string{"role": "admin", "domain": "*"}) // disabling others is allowed
	watchURL := h.srv.URL + api.Prefix + "/watch"
	sessionStream := func(c *client) <-chan sseEvent { return openSSE(t, c.hc, watchURL) }
	ended := func(t *testing.T, ch <-chan sseEvent) {
		t.Helper()
		if reason := waitEnd(t, ch, revocationBound); reason != api.EndUnauthenticated {
			t.Fatalf("end reason %q", reason)
		}
	}

	t.Run("logout", func(t *testing.T) {
		createUser(t, root, "ben@example.com", globalViewer)
		ben, _, _ := signIn(t, h, "ben@example.com", nil)
		ch := sessionStream(ben)
		expect(t, ben.do("POST", "/auth/logout", nil), 200)
		ended(t, ch)
		expect(t, ben.do("GET", "/auth/me", nil), 401)
	})

	t.Run("one of your sessions revoked", func(t *testing.T) {
		createUser(t, root, "ana@example.com", globalViewer)
		first, me1, secret := signIn(t, h, "ana@example.com", nil)
		second, _, _ := signIn(t, h, "ana@example.com", secret)
		ch := sessionStream(first)
		other := sessionStream(second)
		expect(t, second.do("DELETE", "/auth/sessions/"+me1.SessionID, nil), 204)
		ended(t, ch)
		still(t, other, time.Second) // the revoking session keeps its stream
	})

	t.Run("user disabled", func(t *testing.T) {
		id := createUser(t, root, "cy@example.com", globalViewer)
		cy, _, _ := signIn(t, h, "cy@example.com", nil)
		ch := sessionStream(cy)
		expect(t, root.do("POST", "/users/"+id+"/disable", nil, "If-Match", ifMatch(t, h, id)), 200)
		ended(t, ch)
	})

	t.Run("password reset", func(t *testing.T) {
		id := createUser(t, root, "dee@example.com", globalViewer)
		dee, _, _ := signIn(t, h, "dee@example.com", nil)
		ch := sessionStream(dee)
		r := root.do("POST", "/users/"+id+"/reset-token", nil)
		expect(t, r, 200)
		var rt api.ResetToken
		r.json(t, &rt)
		expect(t, h.client().do("POST", "/auth/reset", map[string]string{"token": rt.Token, "password": "another correct horse battery"}), 204)
		ended(t, ch)
	})

	t.Run("grants changed", func(t *testing.T) {
		id := createUser(t, root, "eve@example.com", globalViewer)
		eve, _, _ := signIn(t, h, "eve@example.com", nil)
		ch := sessionStream(eve)
		expect(t, root.do("PUT", "/users/"+id+"/grants", map[string]any{"grants": []map[string]string{{"role": "viewer", "domain": ids.New(ids.Space)}}}, "If-Match", ifMatch(t, h, id)), 200)
		ended(t, ch)
	})

	t.Run("SSO session", func(t *testing.T) {
		id := createUser(t, root, "fay@example.com", globalViewer)
		var tok string
		if err := h.c.Store.W().Tx(ctx, func(tx *store.Tx) error {
			var err error
			tok, err = h.c.Auth.CreateSessionTx(ctx, tx, id, auth.MethodOIDC, auth.SessionMeta{Provider: "porch"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		ch := openSSE(t, http.DefaultClient, watchURL, "Cookie", api.SessionCookieName(false)+"="+tok)
		expect(t, root.do("POST", "/users/"+id+"/disable", nil, "If-Match", ifMatch(t, h, id)), 200)
		ended(t, ch)
	})

	t.Run("API token revoked", func(t *testing.T) {
		createUser(t, root, "gus@example.com", map[string]string{"role": "admin", "domain": "*"})
		secret := loginAndIssueToken(t, h, "gus@example.com", []map[string]string{globalViewer})
		ch := openSSE(t, http.DefaultClient, watchURL, "Authorization", "Bearer "+secret)
		tok := h.client()
		tok.bearer = secret
		var mine struct {
			Items []auth.APIToken `json:"items"`
		}
		r := root.do("GET", "/admin/tokens", nil)
		expect(t, r, 200)
		r.json(t, &mine)
		if len(mine.Items) == 0 {
			t.Fatalf("no tokens: %s", r.body)
		}
		expect(t, root.do("DELETE", "/admin/tokens/"+mine.Items[0].ID, nil), 200)
		ended(t, ch)
		expect(t, tok.do("GET", "/auth/me", nil), 401)
	})

	t.Run("root token rotated", func(t *testing.T) {
		ch := openSSE(t, http.DefaultClient, watchURL, "Authorization", "Bearer "+root.bearer)
		r := root.do("POST", "/admin/root-token/rotate", nil)
		expect(t, r, 200)
		ended(t, ch)
		expect(t, root.do("GET", "/auth/me", nil), 401)
		var created api.CreatedRootToken
		r.json(t, &created)
		root.bearer = created.Secret
	})
}

// SM-AUTH-007: a removed grant hides a space's changes from an open stream at once, without closing a stream whose
// credential is still valid; and the periodic re-check (the backstop) ends streams whose session or token expired.
func TestWatchStreamFiltersAndExpires(t *testing.T) {
	t.Parallel()
	h := newHarnessWith(t, func(c *config.Config) { c.Auth.StreamRevalidate = config.Duration(time.Second) })
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	createUser(t, root, "adm@example.com", map[string]string{"role": "admin", "domain": "*"})
	spaceA, spaceB := ids.New(ids.Space), ids.New(ids.Space)
	id := createUser(t, root, "uma@example.com", map[string]string{"role": "viewer", "domain": spaceA}, map[string]string{"role": "viewer", "domain": spaceB})
	u, err := h.c.Auth.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := h.c.Auth.CreateAPIToken(ctx, &auth.Principal{User: u}, "watch", []auth.GrantInput{{Role: authz.RoleViewer, Domain: spaceA},
		{Role: authz.RoleViewer, Domain: spaceB}}, 24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	change := func(space string) {
		if err := h.c.Store.W().Tx(ctx, func(tx *store.Tx) error { return h.c.Changes.Record(ctx, tx, space, "task", "tsk_x", "updated", 1) }); err != nil {
			t.Fatal(err)
		}
	}
	ch := openSSE(t, http.DefaultClient, h.srv.URL+api.Prefix+"/watch", "Authorization", "Bearer "+secret)
	change(spaceA)
	if c := nextChange(t, ch); c["space_id"] != spaceA {
		t.Fatalf("change %v", c)
	}
	expect(t, root.do("PUT", "/users/"+id+"/grants", map[string]any{"grants": []map[string]string{{"role": "viewer", "domain": spaceB}}}, "If-Match", ifMatch(t, h, id)), 200)
	change(spaceA)
	change(spaceB)
	if c := nextChange(t, ch); c["space_id"] != spaceB {
		t.Fatalf("a change of a space whose grant was removed was delivered: %v", c)
	}

	// Expiry is not an event: the periodic re-check ends the stream within one interval.
	h.clk.Advance(25 * time.Hour)
	if reason := waitEnd(t, ch, 3*time.Second); reason != api.EndUnauthenticated {
		t.Fatalf("end reason %q", reason)
	}

	// A browser session that idles out ends its stream too; the stream itself never counts as activity.
	createUser(t, root, "ike@example.com", globalViewer)
	ike, _, _ := signIn(t, h, "ike@example.com", nil)
	sch := openSSE(t, ike.hc, h.srv.URL+api.Prefix+"/watch")
	still(t, sch, 1500*time.Millisecond) // several re-checks pass while the session is valid
	h.clk.Advance(31 * time.Minute)
	if reason := waitEnd(t, sch, 3*time.Second); reason != api.EndUnauthenticated {
		t.Fatalf("end reason %q", reason)
	}
}

// SM-AUTH-007, brief §40: revocation never undoes completed actions. Work done with a credential stays done, and stays
// attributed to it in the audit log, after the credential is revoked; only later requests are refused.
func TestRevocationDoesNotUndoCompletedActions(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, rt, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	kim := createUser(t, root, "kim@example.com", map[string]string{"role": "admin", "domain": "*"})
	tokSecret := loginAndIssueToken(t, h, "kim@example.com", []map[string]string{{"role": "admin", "domain": "*"}})
	tok := h.client()
	tok.bearer = tokSecret
	lou := createUser(t, tok, "lou@example.com", globalViewer) // done with the API token

	tokens, err := h.c.Auth.ListUserAPITokens(ctx, kim)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("tokens %v %v", tokens, err)
	}
	expect(t, root.do("DELETE", "/admin/tokens/"+tokens[0].ID, nil), 200)
	expect(t, root.do("POST", "/admin/root-token/rotate", nil), 200)
	// Both credentials are refused from the next request on.
	expect(t, tok.do("GET", "/users", nil), 401)
	expect(t, root.do("GET", "/users", nil), 401)

	// What they did stays: the users and their grants exist, and the audit log still attributes the creations.
	for _, uid := range []string{kim, lou} {
		u, err := h.c.Auth.Get(ctx, uid)
		if err != nil || !u.Enabled || len(u.Grants) == 0 {
			t.Fatalf("user created before the revocation changed: %+v %v", u, err)
		}
	}
	created := map[string]string{}
	rows, err := h.c.Store.R().ListAuditPage(ctx, db.ListAuditPageParams{Lim: 100, Action: sql.NullString{String: "user.created", Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		created[r.TargetID.String] = r.ActorType + ":" + r.ActorID.String
	}
	if created[kim] != "root:"+rt.ID || !strings.HasPrefix(created[lou], "user:") {
		t.Fatalf("audit attribution of completed actions: %v", created)
	}
	if res, err := audit.Verify(ctx, h.c.Store.R()); err != nil || !res.OK {
		t.Fatalf("audit chain after revocation: %+v %v", res, err)
	}
}
