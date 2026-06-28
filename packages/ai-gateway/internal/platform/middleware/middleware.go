// Package middleware provides HTTP middleware for the ai-gateway.
package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

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
					http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// CORSConfig holds CORS middleware settings.
type CORSConfig struct {
	AllowedOrigins []string
	AllowedMethods []string
	AllowedHeaders []string
	// ExposeHeaders lists response headers that browser JS may read via fetch/XHR.
	// Maps to the Access-Control-Expose-Headers response header.
	ExposeHeaders []string
	MaxAge        int
}

// CORS returns middleware that handles Cross-Origin Resource Sharing.
func CORS(cfg CORSConfig) func(http.Handler) http.Handler {
	origins := make(map[string]bool, len(cfg.AllowedOrigins))
	allowAll := false
	for _, o := range cfg.AllowedOrigins {
		if o == "*" {
			allowAll = true
		}
		origins[o] = true
	}

	methods := "GET, POST, PUT, DELETE, OPTIONS"
	if len(cfg.AllowedMethods) > 0 {
		methods = strings.Join(cfg.AllowedMethods, ", ")
	}

	headers := "Content-Type, Authorization, x-nexus-virtual-key, x-request-id"
	if len(cfg.AllowedHeaders) > 0 {
		headers = strings.Join(cfg.AllowedHeaders, ", ")
	}

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

			if allowAll || origins[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				if exposeHeaders != "" {
					w.Header().Set("Access-Control-Expose-Headers", exposeHeaders)
				}
			}

			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", methods)
				w.Header().Set("Access-Control-Allow-Headers", headers)
				w.Header().Set("Access-Control-Max-Age", maxAge)
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
