package app_test

import "log/slog"

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }
