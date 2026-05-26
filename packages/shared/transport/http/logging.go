package http

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// WrapOpts controls the logging RoundTripper. Logger nil → slog.Default();
// Caller "" → "unknown".
type WrapOpts struct {
	Logger         *slog.Logger
	Caller         string
	PropagateReqID bool
}

// WrapTransport returns a RoundTripper that instruments base with one
// debug-level slog record per outbound HTTP call. Errors emit at warn.
func WrapTransport(base http.RoundTripper, opts WrapOpts) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Caller == "" {
		opts.Caller = "unknown"
	}
	return &loggingTransport{base: base, opts: opts}
}

type loggingTransport struct {
	base http.RoundTripper
	opts WrapOpts
}

// Unwrap returns the base RoundTripper this wrapper instruments.
// Callers that need to reach the underlying *http.Transport (e.g. to
// re-apply a custom TLSClientConfig) can walk through Unwrap.
func (t *loggingTransport) Unwrap() http.RoundTripper {
	return t.base
}

func (t *loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	reqID := RequestIDFromContext(ctx)

	if t.opts.PropagateReqID && reqID != "" && req.Header.Get("X-Nexus-Request-Id") == "" {
		req = req.Clone(ctx)
		req.Header.Set("X-Nexus-Request-Id", reqID)
	}

	debug := t.opts.Logger.Enabled(ctx, slog.LevelDebug)
	start := time.Now()

	if !debug {
		// Fast path: no body counting, no httptrace. Errors still log at warn.
		resp, err := t.base.RoundTrip(req)
		if err != nil {
			t.logError(ctx, req, reqID, time.Since(start).Milliseconds(), err)
		}
		return resp, err
	}

	var reqBody *countingReadCloser
	if req.Body != nil && req.ContentLength <= 0 {
		reqBody = &countingReadCloser{rc: req.Body}
		req = req.Clone(ctx)
		req.Body = reqBody
	}

	var reused bool
	traceCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
	})
	req = req.WithContext(traceCtx)

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		t.logError(traceCtx, req, reqID, time.Since(start).Milliseconds(), err)
		return nil, err
	}

	reqBytes := req.ContentLength
	if reqBytes <= 0 && reqBody != nil {
		reqBytes = reqBody.count()
	}
	if reqBytes < 0 {
		reqBytes = 0
	}

	// Some transports (HEAD, 204, certain test fakes) legally return a nil
	// Body. Wrapping nil would panic and would also break callers that check
	// resp.Body == nil; emit the success record synchronously instead.
	if resp.Body == nil {
		t.emitSuccess(traceCtx, req, reqID, resp.StatusCode, resp.Proto, reused, reqBytes, 0, time.Since(start))
		return resp, nil
	}

	// HTTP 101 Switching Protocols hijacks the underlying connection — the
	// caller (e.g. coder/websocket) downcasts resp.Body to io.ReadWriteCloser
	// to drive the duplex frame stream. Wrapping it in loggingBody (which
	// only embeds io.ReadCloser) breaks that cast. Emit the success record
	// up front and return the original body untouched.
	if resp.StatusCode == http.StatusSwitchingProtocols {
		t.emitSuccess(traceCtx, req, reqID, resp.StatusCode, resp.Proto, reused, reqBytes, 0, time.Since(start))
		return resp, nil
	}

	resp.Body = &loggingBody{
		ReadCloser: resp.Body,
		emit: func(respBytes int64, dur time.Duration) {
			t.emitSuccess(traceCtx, req, reqID, resp.StatusCode, resp.Proto, reused, reqBytes, respBytes, dur)
		},
		start: start,
	}
	return resp, nil
}

type loggingBody struct {
	io.ReadCloser
	emit   func(respBytes int64, dur time.Duration)
	start  time.Time
	bytes  atomic.Int64
	closed atomic.Bool
}

func (b *loggingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.bytes.Add(int64(n))
	}
	return n, err
}

func (b *loggingBody) Close() error {
	if !b.closed.CompareAndSwap(false, true) {
		// Duplicate Close: do not re-close the underlying body and do not
		// re-emit the log record.
		return nil
	}
	err := b.ReadCloser.Close()
	b.emit(b.bytes.Load(), time.Since(b.start))
	return err
}

type countingReadCloser struct {
	rc io.ReadCloser
	n  atomic.Int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		c.n.Add(int64(n))
	}
	return n, err
}

func (c *countingReadCloser) Close() error { return c.rc.Close() }
func (c *countingReadCloser) count() int64 { return c.n.Load() }

func (t *loggingTransport) emitSuccess(ctx context.Context, req *http.Request, reqID string, status int, proto string, reused bool, reqBytes, respBytes int64, dur time.Duration) {
	t.opts.Logger.LogAttrs(ctx, slog.LevelDebug, "outbound http",
		slog.String("caller", t.opts.Caller),
		slog.String("method", req.Method),
		slog.String("url", redactURLQuery(req.URL)),
		slog.String("host", req.URL.Host),
		slog.Int("status", status),
		slog.Int64("req_bytes", reqBytes),
		slog.Int64("resp_bytes", respBytes),
		slog.Int64("duration_ms", dur.Milliseconds()),
		slog.String("nexus_request_id", reqID),
		slog.Int("attempt", AttemptFromContext(ctx)),
		slog.String("proto", proto),
		slog.Bool("reused", reused),
	)
}

func (t *loggingTransport) logError(ctx context.Context, req *http.Request, reqID string, durMs int64, err error) {
	t.opts.Logger.LogAttrs(ctx, slog.LevelWarn, "outbound http",
		slog.String("caller", t.opts.Caller),
		slog.String("method", req.Method),
		slog.String("url", redactURLQuery(req.URL)),
		slog.String("host", req.URL.Host),
		slog.Int("status", 0),
		slog.Int64("req_bytes", 0),
		slog.Int64("resp_bytes", 0),
		slog.Int64("duration_ms", durMs),
		slog.String("nexus_request_id", reqID),
		slog.Int("attempt", AttemptFromContext(ctx)),
		slog.String("err", err.Error()),
	)
}

// sensitiveQueryParams is the set of query parameter names whose VALUES
// we redact in logged URLs. Names are matched case-insensitively against
// each entry. The list is conservative — only widely-known credential
// carriers — so we don't break debugging by over-redacting.
//
// Notable inclusions:
//   - "key"           — Gemini API uses ?key=... for the API key
//   - "api_key"       — most generic SDKs
//   - "access_token"  — OAuth-style query auth (rare but exists)
//   - "signature"     — HMAC-signed request URLs (e.g. some Bedrock paths)
var sensitiveQueryParams = []string{
	"api_key", "apikey", "api-key",
	"key",
	"access_token", "accesstoken", "access-token",
	"token",
	"auth", "authorization",
	"signature", "sig",
	"password", "passwd", "pwd",
	"secret",
}

// redactURLQuery returns u.String() with the values of any sensitive
// query parameters replaced by "***". Non-sensitive parameters and the
// path are unchanged. Returns an empty string if u is nil.
func redactURLQuery(u *url.URL) string {
	if u == nil {
		return ""
	}
	if u.RawQuery == "" {
		return u.String()
	}
	q := u.Query()
	redacted := false
	for name, values := range q {
		if !isSensitiveParamName(name) {
			continue
		}
		for i := range values {
			values[i] = "***"
		}
		q[name] = values
		redacted = true
	}
	if !redacted {
		return u.String()
	}
	out := *u
	out.RawQuery = q.Encode()
	return out.String()
}

func isSensitiveParamName(name string) bool {
	for _, s := range sensitiveQueryParams {
		if strings.EqualFold(name, s) {
			return true
		}
	}
	return false
}
