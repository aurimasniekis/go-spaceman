package app_test

import (
	"log/slog"
	"os"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testLog() *slog.Logger {
	if os.Getenv("SM_TEST_LOG") == "" {
		return discard()
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
