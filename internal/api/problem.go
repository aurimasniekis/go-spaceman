package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// ProblemTypeBase prefixes problem type URIs; each code has a documentation anchor on the site.
const ProblemTypeBase = "https://aurimasniekis.github.io/go-spaceman/reference/errors#"

// Problem is an RFC 9457 problem document with a stable machine-readable code (SM-API-002). Every error the API
// returns, including Huma's own validation errors, uses this shape.
type Problem struct {
	Type     string              `json:"type" doc:"URI identifying the problem type"`
	Title    string              `json:"title"`
	Status   int                 `json:"status"`
	Code     string              `json:"code" doc:"Stable machine-readable error code"`
	Detail   string              `json:"detail,omitempty"`
	Instance string              `json:"instance,omitempty"`
	Errors   []apperr.FieldError `json:"errors,omitempty"`

	headers http.Header
}

func (p *Problem) Error() string                   { return p.Detail }
func (p *Problem) GetStatus() int                  { return p.Status }
func (p *Problem) ContentType(string) string       { return "application/problem+json" }
func (p *Problem) GetHeaders() http.Header         { return p.headers }
func (p *Problem) withHeader(k, v string) *Problem { p.headers = http.Header{k: {v}}; return p }

var codeStatus = map[apperr.Code]int{
	apperr.CodeInvalid:              http.StatusUnprocessableEntity,
	apperr.CodeNotFound:             http.StatusNotFound,
	apperr.CodeAlreadyExists:        http.StatusConflict,
	apperr.CodeConflict:             http.StatusConflict,
	apperr.CodeForbidden:            http.StatusForbidden,
	apperr.CodePrecondition:         http.StatusConflict,
	apperr.CodePreconditionRequired: http.StatusPreconditionRequired,
	apperr.CodeUnavailable:          http.StatusServiceUnavailable,
	apperr.CodeRateLimited:          http.StatusTooManyRequests,
	apperr.CodeReauth:               http.StatusUnauthorized,
	apperr.CodeMFARequired:          http.StatusUnauthorized,
	apperr.CodeUnauthorized:         http.StatusUnauthorized,
}

var statusCode = map[int]apperr.Code{
	http.StatusBadRequest:            apperr.CodeInvalid,
	http.StatusUnprocessableEntity:   apperr.CodeInvalid,
	http.StatusUnauthorized:          apperr.CodeUnauthorized,
	http.StatusForbidden:             apperr.CodeForbidden,
	http.StatusNotFound:              apperr.CodeNotFound,
	http.StatusConflict:              apperr.CodeConflict,
	http.StatusPreconditionFailed:    apperr.CodeConflict,
	http.StatusPreconditionRequired:  apperr.CodePreconditionRequired,
	http.StatusRequestEntityTooLarge: "payload_too_large",
	http.StatusUnsupportedMediaType:  "unsupported_media_type",
	http.StatusMethodNotAllowed:      "method_not_allowed",
	http.StatusTooManyRequests:       apperr.CodeRateLimited,
	http.StatusServiceUnavailable:    apperr.CodeUnavailable,
}

func newProblem(status int, code apperr.Code, detail string, fields []apperr.FieldError) *Problem {
	if code == "" {
		code = statusCode[status]
		if code == "" {
			code = "internal_error"
		}
	}
	return &Problem{Type: ProblemTypeBase + string(code), Title: http.StatusText(status), Status: status, Code: string(code),
		Detail: detail, Errors: fields}
}

func init() {
	// Huma's own errors (request validation, unknown fields, body too large…) become SpaceMan problems.
	huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
		var fields []apperr.FieldError
		for _, e := range errs {
			var d *huma.ErrorDetail
			if errors.As(e, &d) {
				fields = append(fields, apperr.FieldError{Field: d.Location, Message: d.Message})
			} else if e != nil {
				fields = append(fields, apperr.FieldError{Message: e.Error()})
			}
		}
		if status >= 500 {
			msg = "internal error"
			fields = nil
		}
		return newProblem(status, "", msg, fields)
	}
}

// fail converts a service error into a problem. Domain errors keep their code and message; anything else is logged
// with the operation name and becomes an opaque 500 so internals never leak (SM-OBS-001).
func (d *Deps) fail(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	var p *Problem
	if errors.As(err, &p) {
		return p
	}
	var ae *apperr.Error
	if errors.As(err, &ae) {
		st := codeStatus[ae.Code]
		if st == 0 {
			st = http.StatusBadRequest
		}
		pr := newProblem(st, ae.Code, ae.Message, ae.Fields)
		if ae.Code == apperr.CodeRateLimited {
			pr = pr.withHeader("Retry-After", "60")
		}
		return pr
	}
	switch {
	case errors.Is(err, context.Canceled):
		return newProblem(499, "client_closed_request", "client closed request", nil)
	case errors.Is(err, store.ErrBusy):
		// Contention outlived the store's bounded retries: nothing was written, so the client may retry (with the
		// same Idempotency-Key for creates).
		d.log().WarnContext(ctx, "api storage busy", slog.String("op", op), slog.String("error.message", err.Error()))
		return newProblem(http.StatusServiceUnavailable, apperr.CodeUnavailable, "storage busy; retry shortly", nil).withHeader("Retry-After", "1")
	case errors.Is(err, store.ErrFull), errors.Is(err, store.ErrIO):
		d.log().ErrorContext(ctx, "api storage error", slog.String("op", op), slog.String("error.message", err.Error()))
		return newProblem(http.StatusServiceUnavailable, apperr.CodeUnavailable, "storage temporarily unavailable", nil)
	}
	d.log().ErrorContext(ctx, "api error", slog.String("op", op), slog.String("error.message", err.Error()))
	return newProblem(http.StatusInternalServerError, "internal_error", "internal error", nil)
}

// writeProblem writes a problem from plain net/http middleware.
func writeProblem(w http.ResponseWriter, p *Problem) {
	for k, vs := range p.headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

func problemFor(err error) *Problem {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		st := codeStatus[ae.Code]
		if st == 0 {
			st = http.StatusBadRequest
		}
		return newProblem(st, ae.Code, ae.Message, ae.Fields)
	}
	return newProblem(http.StatusInternalServerError, "internal_error", "internal error", nil)
}

// Common problems.
var (
	errUnauthenticated = func() *Problem {
		return newProblem(http.StatusUnauthorized, apperr.CodeUnauthorized, "authentication required", nil)
	}
	errForbidden = func() *Problem { return newProblem(http.StatusForbidden, apperr.CodeForbidden, "not permitted", nil) }
	errCSRF      = func() *Problem {
		return newProblem(http.StatusForbidden, apperr.CodeForbidden, "missing or invalid CSRF token (X-CSRF-Token)", nil)
	}
	errCrossSite = func() *Problem {
		return newProblem(http.StatusForbidden, apperr.CodeForbidden, "cross-site request refused", nil)
	}
	errLimited = func() *Problem {
		return newProblem(http.StatusUnauthorized, apperr.CodeMFARequired, "complete multi-factor authentication first", nil)
	}
)
