package traffic

import (
	"io"
	"log/slog"
)

// silentLogger returns a logger that discards every record. Used by
// tests to avoid spamming stderr.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
