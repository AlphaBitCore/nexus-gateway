package shadow

import (
	"io"
	"log/slog"
)

// silentLogger returns a logger that discards every record. Used by
// offline_test.go to avoid spamming stdout during table-driven tests.
// Originally lived in manager_test.go before the dispatcher was retired
// (R3 refactor); moved here to keep the helper alive without resurrecting
// the removed Manager surface.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
