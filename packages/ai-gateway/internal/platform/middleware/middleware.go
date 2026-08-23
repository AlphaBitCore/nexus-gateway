// Package middleware provides HTTP middleware for the ai-gateway.
package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// RequestID honors an inbound X-Nexus-Request-Id (set by an upstream Nexus
// service) and assigns a fresh UUID only when none is present, so trace_id
// correlation survives the hop instead of being severed at the gateway. Any
// client-supplied x-request-id is preserved separately for audit correlation.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Nexus-Request-Id")
		if id == "" {
			id = uuid.New().String()
		}
		w.Header().Set("X-Nexus-Request-Id", id)
		r.Header.Set("X-Nexus-Request-Id", id)
		// Preserve client's x-request-id if present (read by handler as ClientRequestID).
		if clientID := r.Header.Get("X-Request-Id"); clientID != "" {
			w.Header().Set("X-Request-Id", clientID)
		}
		r = r.WithContext(nexushttp.WithRequestID(r.Context(), id))
		next.ServeHTTP(w, r)
	})
}

// Logger logs each request with method, path, status, and duration.
// Successful (2xx/3xx) requests and health/metrics probes log at Debug — the
// per-request data is already captured in the traffic_event audit row and the
// Prometheus request/latency series, so an Info access line per request is
// redundant hot-path I/O. 4xx responses log at Warn, 5xx at Error.
func Logger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(sw, r)

			path := r.URL.Path
			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", path),
				slog.Int("status", sw.status),
				slog.Duration("duration", time.Since(start)),
				slog.String("requestId", w.Header().Get("X-Nexus-Request-Id")),
			}

			// Reduce noise: health/metrics probes go to Debug.
			if path == "/healthz" || path == "/metrics" {
				logger.LogAttrs(r.Context(), slog.LevelDebug, "http request", attrs...)
				return
			}

			// Successful (2xx/3xx) requests log at Debug, not Info: on the proxy
			// hot path this access line writes per request (to stdout AND the log
			// file via the MultiWriter) and fully duplicates data already recorded
			// elsewhere — every request becomes a traffic_event audit row
			// (method/path/status/latency) and the Prometheus request/latency
			// series. At load the synchronous per-request file write was a
			// measurable hot-path cost (~2.8% CPU) for a triply-redundant line.
			// 4xx/5xx still surface at Warn/Error so failures stay visible at the
			// default Info level without the audit/metrics round-trip.
			level := slog.LevelDebug
			if sw.status >= 500 {
				level = slog.LevelError
			} else if sw.status >= 400 {
				level = slog.LevelWarn
			}
			logger.LogAttrs(r.Context(), level, "http request", attrs...)
		})
	}
}

// Recovery recovers from panics and returns 500.
func Recovery(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic recovered", "panic", rec, "path", r.URL.Path)
					// Not http.Error: it answers text/plain, and the body it
					// carried made `error` a STRING, so err.error.message threw
					// in every SDK that reached for it. A panic is the response
					// a caller is least equipped to interpret, so it gets the
					// same envelope as every other gateway error.
					envelope.WriteGatewayError(w, r, http.StatusInternalServerError,
						"INTERNAL_ERROR", "internal server error", "")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// CORSConfig holds CORS middleware settings.
//
// The middleware joins what it is given — it owns no header lists of its
// own. The wiring layer composes AllowedHeaders as
//
//	gateway-required ∪ provider-forwardable ∪ operator extras
//
// (traffic.AcceptHeaders ∪ forwardheader allowlist ∪ yaml cors.allowedHeaders)
// so an operator can only EXTEND the set, never shrink it below what the
// gateway's own read sites and the forward path need. Over-listing is
// harmless — the gateway ignores names it does not read, and the
// forward-header gate still default-denies them toward providers.
type CORSConfig struct {
	AllowedOrigins []string
	AllowedMethods []string
	AllowedHeaders []string
	// ExposeHeaders lists response headers that browser JS may read via fetch/XHR.
	// Maps to the Access-Control-Expose-Headers response header.
	ExposeHeaders []string
	MaxAge        int
}

// UnionHeaderNames merges header-name lists into one deduplicated slice.
// Names are compared case-insensitively (HTTP header names are), the
// first-seen spelling wins, and the result is sorted so the emitted CORS
// value is deterministic across restarts.
func UnionHeaderNames(lists ...[]string) []string {
	seen := map[string]string{}
	for _, l := range lists {
		for _, n := range l {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			k := strings.ToLower(n)
			if _, ok := seen[k]; !ok {
				seen[k] = n
			}
		}
	}
	out := make([]string, 0, len(seen))
	for _, n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// CORS returns middleware that handles Cross-Origin Resource Sharing.
//
// Every response on a request that carries an Origin gets `Vary: Origin`
// (via Add, preserving any existing Vary): the response differs by Origin,
// so a shared cache must not serve one origin's copy to another —
// including the "disallowed origin" copy, which carries no CORS headers at
// all. Preflight responses grant Allow-Methods/-Headers only to allowed
// origins; a disallowed origin gets a bare 204 that the browser then
// rejects, rather than a readout of what the gateway would accept.
func CORS(cfg CORSConfig) func(http.Handler) http.Handler {
	origins := make(map[string]bool, len(cfg.AllowedOrigins))
	allowAll := false
	for _, o := range cfg.AllowedOrigins {
		if o == "*" {
			allowAll = true
		}
		origins[o] = true
	}

	methods := strings.Join(cfg.AllowedMethods, ", ")
	headers := strings.Join(cfg.AllowedHeaders, ", ")
	exposeHeaders := strings.Join(cfg.ExposeHeaders, ", ")

	maxAge := "86400"
	if cfg.MaxAge > 0 {
		maxAge = fmt.Sprintf("%d", cfg.MaxAge)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Add("Vary", "Origin")
			allowed := allowAll || origins[origin]
			if allowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				if exposeHeaders != "" {
					w.Header().Set("Access-Control-Expose-Headers", exposeHeaders)
				}
			}

			if r.Method == http.MethodOptions {
				if allowed {
					if methods != "" {
						w.Header().Set("Access-Control-Allow-Methods", methods)
					}
					if headers != "" {
						w.Header().Set("Access-Control-Allow-Headers", headers)
					}
					w.Header().Set("Access-Control-Max-Age", maxAge)
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying ResponseWriter so SSE streaming works.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter so http.ResponseController
// can reach it for SetWriteDeadline and other per-connection operations.
func (sw *statusWriter) Unwrap() http.ResponseWriter {
	return sw.ResponseWriter
}

// Compile-time assertion that the wrapper stays unwrappable.
// http.ResponseController walks Unwrap() until it finds a writer that
// implements the capability it wants; this wrapper sits between the proxy
// handlers and the real connection, so dropping Unwrap silently severs
// SetWriteDeadline (and Hijack) from the connection. Nothing else would fail:
// the wrapper still satisfies http.ResponseWriter, the build stays green, and
// every long non-stream inference starts dying against the flat
// server.writeTimeout at runtime instead.
var _ interface{ Unwrap() http.ResponseWriter } = (*statusWriter)(nil)
