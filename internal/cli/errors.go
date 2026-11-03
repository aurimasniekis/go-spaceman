package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Exit codes of the remote commands (documented in site/operators/cli.md).
const (
	ExitOK       = 0
	ExitUsage    = 1 // bad flags or arguments, unreadable input files, local failures
	ExitAPI      = 2 // the API refused or failed the request, or the server could not be reached
	ExitConflict = 3 // 409, 412 or 428: the object changed, already exists, or the operation's precondition failed
	ExitNotFound = 4 // 404
	ExitAuth     = 5 // 401 or 403, or no usable credentials
)

// FieldError is one invalid field of a problem.
type FieldError struct {
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// APIError is an RFC 9457 problem returned by SpaceMan (or a bare error status).
type APIError struct {
	Type     string       `json:"type"`
	Title    string       `json:"title"`
	Status   int          `json:"status"`
	Code     string       `json:"code"`
	Detail   string       `json:"detail"`
	Instance string       `json:"instance"`
	Errors   []FieldError `json:"errors"`

	Method    string `json:"-"`
	Path      string `json:"-"`
	Op        string `json:"-"`
	RequestID string `json:"-"`
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d", e.Status)
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	}
	switch {
	case e.Detail != "":
		b.WriteString(": " + e.Detail)
	case e.Title != "":
		b.WriteString(": " + e.Title)
	}
	for _, f := range e.Errors {
		if f.Field != "" {
			fmt.Fprintf(&b, "; %s: %s", f.Field, f.Message)
		} else {
			fmt.Fprintf(&b, "; %s", f.Message)
		}
	}
	return b.String()
}

// NetError is a transport failure (connection refused, TLS, timeout).
type NetError struct {
	Op  string
	Err error
}

func (e *NetError) Error() string { return fmt.Sprintf("%s: %v", e.Op, e.Err) }
func (e *NetError) Unwrap() error { return e.Err }

// AuthError means no usable credentials were configured.
type AuthError struct{ Msg string }

func (e *AuthError) Error() string { return e.Msg }

// UsageError is a mistake in the command line or its inputs.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

func usageErrorf(format string, a ...any) error { return &UsageError{Msg: fmt.Sprintf(format, a...)} }

// StateError reports an operation that finished in an unwanted state (for example a relocation that failed).
type StateError struct{ Msg string }

func (e *StateError) Error() string { return e.Msg }

// ExitCode maps an error to the documented exit code.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var ae *APIError
	if errors.As(err, &ae) {
		switch ae.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return ExitAuth
		case http.StatusNotFound:
			return ExitNotFound
		case http.StatusConflict, http.StatusPreconditionFailed, http.StatusPreconditionRequired:
			return ExitConflict
		}
		return ExitAPI
	}
	var au *AuthError
	if errors.As(err, &au) {
		return ExitAuth
	}
	var ne *NetError
	var se *StateError
	if errors.As(err, &ne) || errors.As(err, &se) {
		return ExitAPI
	}
	return ExitUsage
}

// PrintError writes err for a person: problems with their code, fields, operation and request ID.
func PrintError(w io.Writer, err error) {
	var ae *APIError
	if !errors.As(err, &ae) {
		fmt.Fprintln(w, "spaceman:", err)
		return
	}
	msg := ae.Detail
	if msg == "" {
		msg = ae.Title
	}
	code := ae.Code
	if code == "" {
		code = strings.ReplaceAll(strings.ToLower(http.StatusText(ae.Status)), " ", "_")
	}
	fmt.Fprintf(w, "spaceman: %s (%d %s)\n", msg, ae.Status, code)
	for _, f := range ae.Errors {
		if f.Field != "" {
			fmt.Fprintf(w, "  %s: %s\n", f.Field, f.Message)
		} else {
			fmt.Fprintf(w, "  %s\n", f.Message)
		}
	}
	if ae.Op != "" {
		fmt.Fprintf(w, "  operation: %s (%s %s)\n", ae.Op, ae.Method, ae.Path)
	} else if ae.Method != "" {
		fmt.Fprintf(w, "  request: %s %s\n", ae.Method, ae.Path)
	}
	if ae.RequestID != "" {
		fmt.Fprintf(w, "  request id: %s\n", ae.RequestID)
	}
	if hint := hintFor(ae); hint != "" {
		fmt.Fprintf(w, "  hint: %s\n", hint)
	}
}

func hintFor(ae *APIError) string {
	switch {
	case ae.Status == http.StatusUnauthorized:
		return "the token is missing, expired or revoked"
	case ae.Status == http.StatusPreconditionRequired:
		return "this update needs the object's version; pass --if-match or let the command read it first"
	case ae.Status == http.StatusConflict && strings.Contains(strings.ToLower(ae.Detail), "changed"):
		return "the object changed since it was read; read it again and retry"
	}
	return ""
}
