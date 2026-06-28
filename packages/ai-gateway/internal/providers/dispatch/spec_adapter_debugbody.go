package dispatch

import (
	"bytes"
	"context"
	"io"
	"log/slog"
)

// debugBodyLimit caps how many bytes of the raw upstream body are captured
// and logged when slog DEBUG is enabled. Large enough to show a full SSE
// event from Gemini Flash (typically < 2 KiB) but small enough to avoid
// flooding the log with embedding / image responses.
const debugBodyLimit = 8192

// debugBody wraps an io.ReadCloser to log every Read call at DEBUG level.
// The first debugBodyLimit bytes read are accumulated and emitted as a
// single "upstream stream body" record on Close, giving a clear snapshot
// of what the provider actually sent over the wire. Used only when slog
// DEBUG is enabled; never in production (gated by Enabled check).
type debugBody struct {
	inner  io.ReadCloser
	log    *slog.Logger
	ctx    context.Context //nolint:containedctx
	format string
	buf    bytes.Buffer
	capped bool
}

func newDebugBody(rc io.ReadCloser, log *slog.Logger, ctx context.Context, format string) *debugBody {
	return &debugBody{inner: rc, log: log, ctx: ctx, format: format}
}

func (d *debugBody) Read(p []byte) (int, error) {
	n, err := d.inner.Read(p)
	if n > 0 && !d.capped {
		remaining := debugBodyLimit - d.buf.Len()
		if remaining > 0 {
			take := n
			if take > remaining {
				take = remaining
				d.capped = true
			}
			d.buf.Write(p[:take])
		}
	}
	return n, err
}

func (d *debugBody) Close() error {
	if d.buf.Len() > 0 || d.capped {
		suffix := ""
		if d.capped {
			suffix = " (truncated)"
		}
		d.log.LogAttrs(d.ctx, slog.LevelDebug, "upstream stream body",
			slog.String("format", d.format),
			slog.Int("bytes_captured", d.buf.Len()),
			slog.Bool("capped", d.capped),
			slog.String("body", d.buf.String()+suffix),
		)
	} else {
		d.log.LogAttrs(d.ctx, slog.LevelDebug, "upstream stream body",
			slog.String("format", d.format),
			slog.Int("bytes_captured", 0),
			slog.String("body", "(empty — no bytes read from stream body)"),
		)
	}
	return d.inner.Close()
}
