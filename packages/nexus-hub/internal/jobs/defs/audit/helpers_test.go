package audit

import (
	"io"
	"log/slog"
)

// testLogger returns a discard logger for use in unit tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
