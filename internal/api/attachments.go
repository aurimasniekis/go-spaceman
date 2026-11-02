package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
)

// AttachmentRules are the authorization rules of the attachment operations (SM-ATT-004: authorization matches the
// owning space).
var AttachmentRules = map[string]Rule{
	"upload-attachment": InSpace(authz.AttachmentWrite),
	"get-attachment":    InSpace(authz.AttachmentRead),
}

// AttachmentPath identifies an attachment of a space.
type AttachmentPath struct {
	SpaceID      string `path:"space_id" maxLength:"64"`
	AttachmentID string `path:"attachment_id" maxLength:"64"`
}

// AttachmentOut is an attachment with the URL that serves its content.
type AttachmentOut struct {
	*attachments.Attachment
	URL string `json:"url" doc:"Content URL (same origin); Markdown bodies reference the attachment as spaceman://attachment/<id>"`
	URI string `json:"uri" doc:"The spaceman://attachment/<id> reference to put in Markdown bodies"`
}

func attachmentOut(a *attachments.Attachment) *AttachmentOut {
	return &AttachmentOut{Attachment: a, URL: fmt.Sprintf("%s/spaces/%s/attachments/%s/content", Prefix, a.SpaceID, a.ID),
		URI: attachments.URIScheme + a.ID}
}

// RegisterAttachments registers the upload and metadata operations.
func RegisterAttachments(svc *attachments.Service) func(api huma.API, d *Deps) {
	return func(api huma.API, d *Deps) {
		tags := []string{"attachments"}
		huma.Register(api, huma.Operation{OperationID: "upload-attachment", Method: http.MethodPost, Path: "/spaces/{space_id}/attachments",
			Summary: "Upload an attachment", Description: "A multipart form with one `file` field. The media type is sniffed from the content. " +
				"Reference the returned `uri` (or `id`) from the item that uses it; unreferenced uploads are removed after 24 hours.",
			Tags: tags, DefaultStatus: http.StatusCreated},
			func(ctx context.Context, in *struct {
				SpacePath
				IdempotencyKey
				RawBody huma.MultipartFormFiles[struct {
					File huma.FormFile `form:"file" required:"true"`
				}]
			}) (*struct{ Body *AttachmentOut }, error) {
				f := in.RawBody.Data().File
				defer func() { _ = f.Close() }()
				out, _, err := Idempotent(ctx, d, "upload-attachment", in.IdempotencyKey, map[string]any{"space": in.SpaceID, "name": f.Filename, "size": f.Size}, false,
					func() (*AttachmentOut, error) {
						a, err := svc.Upload(ctx, Actor(ctx), in.SpaceID, f.Filename, f)
						if err != nil {
							return nil, err
						}
						return attachmentOut(a), nil
					})
				if err != nil {
					return nil, d.fail(ctx, "attachments.upload", err)
				}
				return &struct{ Body *AttachmentOut }{Body: out}, nil
			})
		huma.Register(api, huma.Operation{OperationID: "get-attachment", Method: http.MethodGet, Path: "/spaces/{space_id}/attachments/{attachment_id}",
			Summary: "Attachment metadata", Tags: tags},
			func(ctx context.Context, in *AttachmentPath) (*struct{ Body *AttachmentOut }, error) {
				a, err := svc.Get(ctx, in.SpaceID, in.AttachmentID)
				if err != nil {
					return nil, d.fail(ctx, "attachments.get", err)
				}
				return &struct{ Body *AttachmentOut }{Body: attachmentOut(a)}, nil
			})
	}
}

// AttachmentContent serves an attachment's bytes (SM-ATT-004): raster images inline, everything else as a download
// with a neutral type; always nosniff and a sandboxing CSP, so nothing uploaded can run in SpaceMan's origin.
func AttachmentContent(svc *attachments.Service) RawRoute {
	return RawRoute{Pattern: "GET " + Prefix + "/spaces/{space_id}/attachments/{attachment_id}/content", Perm: authz.AttachmentRead,
		Handler: func(d *Deps) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				a, rc, err := svc.Open(r.Context(), r.PathValue("space_id"), r.PathValue("attachment_id"))
				if err != nil {
					writeProblem(w, newProblem(http.StatusNotFound, "not_found", "attachment not found", nil))
					return
				}
				defer func() { _ = rc.Close() }()
				h := w.Header()
				disposition := "attachment"
				ctype := "application/octet-stream"
				if a.Inline {
					disposition, ctype = "inline", a.MIME
				}
				h.Set("Content-Type", ctype)
				h.Set("Content-Disposition", fmt.Sprintf("%s; filename*=UTF-8''%s", disposition, url.PathEscape(a.Name)))
				h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
				h.Set("X-Content-Type-Options", "nosniff")
				h.Set("Cache-Control", "private, max-age=3600")
				h.Set("ETag", `"`+a.SHA256+`"`)
				http.ServeContent(w, r, "", a.CreatedAt, rc)
			}
		}}
}
