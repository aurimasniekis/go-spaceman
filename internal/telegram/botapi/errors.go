package botapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Error is an unsuccessful Bot API answer (`"ok": false`, API#making-requests). Description is Telegram's text with
// the token redacted; Telegram says error codes may change, so callers branch on the helpers below.
type Error struct {
	Method      string
	Code        int
	Description string
	// RetryAfter is parameters.retry_after: how long to wait before repeating the request (429).
	RetryAfter time.Duration
	// MigrateToChatID is parameters.migrate_to_chat_id: the group became a supergroup with this ID.
	MigrateToChatID int64
}

func (e *Error) Error() string {
	if e.Description == "" {
		return fmt.Sprintf("telegram %s: %d", e.Method, e.Code)
	}
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

// TransportError is a failure below the Bot API (connection, TLS, timeout, cancellation). Its text never contains
// the request URL. errors.Is matches context.Canceled and context.DeadlineExceeded.
type TransportError struct {
	Method  string
	Msg     string
	timeout bool
	ctxErr  error
}

func (e *TransportError) Error() string { return fmt.Sprintf("telegram %s: %s", e.Method, e.Msg) }

// Timeout reports whether the failure was a timeout.
func (e *TransportError) Timeout() bool {
	return e.timeout || errors.Is(e.ctxErr, context.DeadlineExceeded)
}

// Is matches the context error that ended the call, if any.
func (e *TransportError) Is(target error) bool { return e.ctxErr != nil && target == e.ctxErr }

// RetryAfter returns how long Telegram asked to wait (429 with parameters.retry_after), if err is such an answer.
func RetryAfter(err error) (time.Duration, bool) {
	var e *Error
	if errors.As(err, &e) && e.Code == http.StatusTooManyRequests {
		return e.RetryAfter, true
	}
	return 0, false
}

// IsForbidden reports a 403 answer: the user blocked the bot, or the bot can't write to the chat (API#making-requests).
func IsForbidden(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == http.StatusForbidden
}

// IsUnauthorized reports a 401 answer: the token is wrong or was revoked in BotFather.
func IsUnauthorized(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == http.StatusUnauthorized
}
