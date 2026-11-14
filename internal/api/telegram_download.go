package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/miniapp"
)

// Signed single-purpose downloads for the Mini App (ADR-0083 §3.4.3): the Mini App holds a header session and cannot
// put it in a URL, so it mints a link that names one attachment for one Telegram link, valid 60 s and 3 uses.

// TelegramDownloadPath is the route of a signed download on the Mini App host (the ID is the last segment).
const TelegramDownloadPath = "/tg-dl/"

// telegramDownloadRedacted is how a download's path appears in logs.
const telegramDownloadRedacted = TelegramDownloadPath + "<redacted>"

// telegramDownloadCORSOrigin is the one origin that may read a download cross-origin: Telegram Web's page calls
// WebApp.downloadFile from web.telegram.org.
const telegramDownloadCORSOrigin = "https://web.telegram.org"

// TelegramDownloadRules is the rule of the minting operation: a session rule that acts for the caller's own Telegram
// link (TelegramSelf), so no other principal kind (a SpaceMan MCP call, say) reaches it. The handler needs the Mini App
// channel and attachment:read in the named space.
var TelegramDownloadRules = map[string]Rule{
	"create-telegram-download-link": {Session: true, TelegramSelf: true},
}

// TelegramDownloads is the store of live download links (miniapp.Downloads).
type TelegramDownloads interface {
	Mint(g miniapp.DownloadGrant) (string, error)
	Redeem(id string) (miniapp.DownloadGrant, error)
}

// TelegramLinkPrincipals builds a link's principal with the current grants (miniapp.Service.LinkPrincipal).
type TelegramLinkPrincipals interface {
	LinkPrincipal(ctx context.Context, linkID string) (*auth.Principal, error)
}

type telegramDownloadIn struct {
	Body struct {
		SpaceID      string `json:"space_id" minLength:"1" maxLength:"64"`
		AttachmentID string `json:"attachment_id" minLength:"1" maxLength:"64"`
	}
}

// TelegramDownloadOut is a minted download link.
type TelegramDownloadOut struct {
	URL       string    `json:"url" doc:"The download link on the Mini App host. It is the capability: do not log or share it"`
	ExpiresAt time.Time `json:"expires_at" doc:"The link stops working at this time"`
	MaxUses   int       `json:"max_uses" doc:"How many requests the link serves"`
}

// RegisterTelegramDownloads registers the minting operation. Nil collaborators serve nothing (404).
func RegisterTelegramDownloads(dl TelegramDownloads, att *attachments.Service, origin func() string) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		huma.Register(api, huma.Operation{OperationID: "create-telegram-download-link", Method: http.MethodPost, Path: "/telegram/download-links",
			Summary: "Mint a signed download link for an attachment",
			Description: "Mini App host only (404 elsewhere). Returns a link on the Mini App host that serves one attachment of the " +
				"space to the caller's Telegram link, for 60 seconds and at most 3 requests. The caller needs attachment:read in " +
				"the space. The link is re-authorized when it is used: an unlinked or disabled caller gets nothing.",
			Tags: []string{"telegram"}, MaxBodyBytes: 1 << 10, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *telegramDownloadIn) (*struct{ Body TelegramDownloadOut }, error) {
				p := PrincipalFrom(ctx)
				notFound := d.fail(ctx, "telegram.download", apperr.New(apperr.CodeNotFound, "attachment not found"))
				if dl == nil || att == nil || origin == nil || telegramChannelFrom(ctx) != channelMiniApp || !p.ViaTelegram() {
					return nil, notFound
				}
				base := origin()
				if base == "" || d.Authz == nil || !d.Authz.AllowedAll(p.Subjects(), in.Body.SpaceID, authz.AttachmentRead) {
					return nil, notFound
				}
				if _, err := att.Get(ctx, in.Body.SpaceID, in.Body.AttachmentID); err != nil {
					return nil, notFound
				}
				now := d.now()
				id, err := dl.Mint(miniapp.DownloadGrant{LinkID: p.TelegramLinkID, SpaceID: in.Body.SpaceID, AttachmentID: in.Body.AttachmentID})
				if errors.Is(err, miniapp.ErrDownloadsFull) {
					return nil, d.fail(ctx, "telegram.download", apperr.New(apperr.CodeRateLimited, "too many live download links; retry shortly"))
				}
				if err != nil {
					return nil, d.fail(ctx, "telegram.download", err)
				}
				return &struct{ Body TelegramDownloadOut }{Body: TelegramDownloadOut{URL: base + TelegramDownloadPath + url.PathEscape(id),
					ExpiresAt: now.Add(miniapp.DownloadTTL), MaxUses: miniapp.DownloadMaxUses}}, nil
			})
	}
}

// TelegramDownloadHandler serves GET /tg-dl/{id} on the Mini App host. There is no session on this request: the ID is
// the capability. It is a lookup key for an in-memory grant (60 s, 3 uses) that names one attachment of one space and
// one Telegram link; every redemption re-authorizes that link (active, user enabled, attachment:read in the space) and
// finds the attachment in that space. An unknown, expired, used-up, revoked or denied link is the same plain 404.
// Responses are never cached and never send a Referer, and the ID appears in no log line.
func (d *Deps) TelegramDownloadHandler(dl TelegramDownloads, links TelegramLinkPrincipals, att *attachments.Service) http.Handler {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	serve := func(w http.ResponseWriter, r *http.Request) int {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		// The only CORS header of the Mini App host: Telegram Web's downloadFile reads the file from its own page. No
		// credentials, and only simple GETs.
		h.Set("Access-Control-Allow-Origin", telegramDownloadCORSOrigin)
		if dl == nil || links == nil || att == nil || d.Authz == nil {
			return notFoundPlain(w)
		}
		g, err := dl.Redeem(r.PathValue("id"))
		if err != nil {
			return notFoundPlain(w)
		}
		p, err := links.LinkPrincipal(r.Context(), g.LinkID)
		if err != nil {
			if !errors.Is(err, auth.ErrUnauthorized) {
				log.Warn("telegram: download re-authorization failed", slog.String("error", err.Error()))
			}
			return notFoundPlain(w)
		}
		if !d.Authz.AllowedAll(p.Subjects(), g.SpaceID, authz.AttachmentRead) {
			return notFoundPlain(w)
		}
		a, rc, err := att.Open(r.Context(), g.SpaceID, g.AttachmentID)
		if err != nil {
			return notFoundPlain(w)
		}
		defer func() { _ = rc.Close() }()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(a.Name))
		h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
		h.Set("ETag", `"`+a.SHA256+`"`)
		sw := &statusWriter{ResponseWriter: w}
		http.ServeContent(sw, r, "", a.CreatedAt, rc)
		return sw.status()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		status := serve(w, r)
		// The request log names the route, never the ID (the ID is the capability).
		log.Info("telegram: download", slog.String("method", r.Method), slog.String("path", telegramDownloadRedacted),
			slog.Int("status", status), slog.Duration("took", time.Since(start)))
	})
}

func notFoundPlain(w http.ResponseWriter) int {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("not found\n"))
	return http.StatusNotFound
}

// statusWriter records the status http.ServeContent wrote.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int) {
	if s.code == 0 {
		s.code = c
	}
	s.ResponseWriter.WriteHeader(c)
}

func (s *statusWriter) status() int {
	if s.code == 0 {
		return http.StatusOK
	}
	return s.code
}
