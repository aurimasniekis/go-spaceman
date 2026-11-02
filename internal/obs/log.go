// Package obs provides logging, metrics, tracing and health primitives.
package obs

import (
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/buildinfo"
	"github.com/aurimasniekis/go-spaceman/internal/obs/redact"
)

// ECS-aligned field names used across SpaceMan logs (SM-OBS-001). See docs/operator/logging.md.
const (
	FieldTimestamp     = "@timestamp"
	FieldLevel         = "log.level"
	FieldMessage       = "message"
	FieldLogger        = "log.logger"
	FieldServiceName   = "service.name"
	FieldServiceVer    = "service.version"
	FieldEventAction   = "event.action"
	FieldEventOutcome  = "event.outcome"
	FieldEventDuration = "event.duration"
	FieldError         = "error.message"
	FieldTraceID       = "trace.id"
	FieldSpanID        = "span.id"
	FieldHTTPRequestID = "http.request.id"
	FieldSourceIP      = "source.ip"
	FieldUserID        = "user.id"
	FieldSpaceID       = "spaceman.space_id"
	FieldNodeID        = "spaceman.node_id"
	FieldRunID         = "spaceman.run_id"
	FieldTaskID        = "spaceman.task_id"
	FieldTokenID       = "spaceman.token_id"
	FieldCommandID     = "spaceman.command_id"
	FieldSubscription  = "spaceman.subscription_id"
	FieldConnectionID  = "spaceman.connection_id"
	FieldComponent     = "spaceman.component"
)

// LogConfig selects the log level (debug, info, warn, error) and format (json, text).
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// NewLogger builds the process logger. JSON output uses ECS-aligned top-level keys and every handler
// redacts sensitive attribute keys. Records logged with a context carry its trace and correlation IDs
// (ContextHandler). The returned LevelVar allows changing the level at runtime.
func NewLogger(w io.Writer, cfg LogConfig) (*slog.Logger, *slog.LevelVar) {
	lv := new(slog.LevelVar)
	lv.Set(ParseLevel(cfg.Level))
	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	if cfg.Format == "text" {
		opts.ReplaceAttr = redact.ReplaceAttr
		h = slog.NewTextHandler(w, opts)
	} else {
		opts.ReplaceAttr = ecsReplace
		h = slog.NewJSONHandler(w, opts)
	}
	l := slog.New(NewContextHandler(h)).With(
		slog.String(FieldServiceName, "spaceman"),
		slog.String(FieldServiceVer, buildinfo.Get().Version),
	)
	return l, lv
}

// ParseLevel converts a config level name; unknown values map to info.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func ecsReplace(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 {
		switch a.Key {
		case slog.TimeKey:
			if t, ok := a.Value.Any().(time.Time); ok {
				return slog.String(FieldTimestamp, t.UTC().Format(time.RFC3339Nano))
			}
			return slog.Attr{Key: FieldTimestamp, Value: a.Value}
		case slog.LevelKey:
			return slog.String(FieldLevel, strings.ToLower(a.Value.String()))
		case slog.MessageKey:
			return slog.Attr{Key: FieldMessage, Value: a.Value}
		}
	}
	return redact.ReplaceAttr(groups, a)
}

// Err returns the standard error attribute.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	return slog.String(FieldError, err.Error())
}

// Component returns a logger tagged with a component name.
func Component(l *slog.Logger, name string) *slog.Logger {
	return l.With(slog.String(FieldComponent, name))
}
