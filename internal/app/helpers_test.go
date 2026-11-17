package app_test

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/accounts"
	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// cleanupReleased registers f with t.Cleanup and lets go of f once it has run. The testing package keeps a finished
// parallel test's *testing.T reachable until every parallel test of the run is done, and its cleanup list keeps the
// closures that already ran (runCleanup pops them by reslicing, which leaves them in the backing array). A cleanup
// holding a central therefore kept that test's whole central (store, services, the API's OpenAPI schemas) alive until
// the end of the parallel phase: about 5 GB of live heap across this package's parallel tests, and with the race
// detector's shadow memory 13 GB resident, which ran a 16 GB CI runner out of memory. Cleanups that reach a central
// or its HTTP server go through here, and drop the references they hold when they are done.
func cleanupReleased(t testing.TB, f func()) {
	t.Helper()
	t.Cleanup(func() {
		g := f
		f = nil
		g()
	})
}

func testLog() *slog.Logger {
	if os.Getenv("SM_TEST_LOG") == "" {
		return discard()
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// bindStubAccounts creates a Claude and a Codex account with stub credentials and binds them to the space as the
// defaults, so its runs can launch (SM-ACCT-003). The returned map is credential kind -> account ID.
func bindStubAccounts(t *testing.T, c *app.Central, spaceID string) map[string]string {
	t.Helper()
	ctx := context.Background()
	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	out := map[string]string{}
	var bs []accounts.Binding
	for _, a := range []accounts.Input{
		{Name: "stub-claude-" + spaceID[len(spaceID)-6:], Kind: "claude_oauth_token", Secret: "stub-claude-token", Enabled: true},
		{Name: "stub-codex-" + spaceID[len(spaceID)-6:], Kind: "openai_api_key", Secret: "stub-openai-key", Enabled: true},
	} {
		acc, err := c.Accounts.Create(ctx, actor, a)
		if err != nil {
			t.Fatal(err)
		}
		out[a.Kind] = acc.ID
		bs = append(bs, accounts.Binding{AccountID: acc.ID, Default: true})
	}
	if _, err := c.Accounts.SetBindings(ctx, actor, spaceID, bs); err != nil {
		t.Fatal(err)
	}
	return out
}
