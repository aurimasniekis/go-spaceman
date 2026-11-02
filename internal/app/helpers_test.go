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
