package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/miniapp"
)

type fakeDownloads struct {
	minted []miniapp.DownloadGrant
	err    error
}

func (f *fakeDownloads) Mint(g miniapp.DownloadGrant) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.minted = append(f.minted, g)
	return strings.Repeat("A", 43), nil
}

func (f *fakeDownloads) Redeem(string) (miniapp.DownloadGrant, error) {
	return miniapp.DownloadGrant{}, miniapp.ErrDownloadGone
}

// The mint handler's own guards (ADR-0083 §3.4.3), reached without the middleware in front of it: a request that is
// not a Telegram principal on the Mini App channel gets the same 404 as a missing attachment, and a full store is 429.
func TestTelegramDownloadMintGuards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)
	aw := audit.NewWriter(keyringtest.Load(t, s))
	att := attachments.New(s, attachments.NewLocal(t.TempDir()), aw, attachments.Options{})
	err := s.W().TxNamed(ctx, "test.seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('` + tgSpace + `', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		} {
			if _, err := tx.SQL.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := att.Upload(ctx, audit.System, tgSpace, "r.txt", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}

	user := &auth.Principal{User: &auth.User{ID: "usr_tg"}, Method: auth.MethodPassword} // granted in tgSpace, but no Telegram link
	for name, c := range map[string]struct {
		p      *auth.Principal
		ch     telegramChannel
		mintEr error
		want   int
	}{
		"telegram principal on the Mini App": {tgPrincipal(), channelMiniApp, nil, 201},
		"non-Telegram principal":             {user, channelMiniApp, nil, 404},
		"chat channel":                       {tgPrincipal(), channelChat, nil, 404},
		"no channel":                         {tgPrincipal(), channelNone, nil, 404},
		"store full":                         {tgPrincipal(), channelMiniApp, miniapp.ErrDownloadsFull, 429},
	} {
		dl := &fakeDownloads{err: c.mintEr}
		d := &Deps{Authz: tgAuthz(t)}
		mux := http.NewServeMux()
		RegisterTelegramDownloads(dl, att, func() string { return "https://tg.example.com" })(humago.NewWithPrefix(mux, Prefix, Config()), d)
		req := httptest.NewRequest(http.MethodPost, Prefix+"/telegram/download-links",
			strings.NewReader(`{"space_id":"`+tgSpace+`","attachment_id":"`+a.ID+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(context.WithValue(context.WithValue(req.Context(), principalKey{}, c.p), telegramChannelKey{}, c.ch))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, c.want)
		}
		if want := c.want == 201; (len(dl.minted) == 1) != want {
			t.Errorf("%s: minted %d grants", name, len(dl.minted))
		}
		if c.want == 429 && rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s: no Retry-After", name)
		}
	}
}
