package logging

import (
	"log/slog"
	"net/http"
	"time"
)

// HTTPRequestLogger returns net/http middleware that logs every request with
// method, path, status code, duration, and remote address. Suitable for
// wrapping any http.Handler (e.g. compliance-proxy runtime API).
func HTTPRequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusCapture{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			logger.Info("http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", sw.status),
				slog.Duration("duration", time.Since(start)),
				slog.String("remoteAddr", r.RemoteAddr),
			)
		})
	}
}

type statusCapture struct {
	http.ResponseWriter
	status int
}

func (s *statusCapture) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying ResponseWriter for streaming support.
func (s *statusCapture) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter for http.ResponseController.
func (s *statusCapture) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
