package runtimeintrospect

import (
	"context"
	"crypto/subtle"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type HandlerOptions struct {
	// Token is the bearer token required in the Authorization header.
	// Empty token means the endpoint is administratively disabled and
	// returns 503 instead of serving — refuse rather than expose state.
	Token string

	Logger  *slog.Logger
	Timeout time.Duration
}

func (r *Registry) Handler(opts HandlerOptions) http.Handler {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	expected := []byte(strings.TrimSpace(opts.Token))

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if len(expected) == 0 {
			http.Error(w, "introspection disabled", http.StatusServiceUnavailable)
			return
		}
		auth := req.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		provided := []byte(strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")))
		if subtle.ConstantTimeCompare(provided, expected) != 1 {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		ctx, cancel := context.WithTimeout(req.Context(), opts.Timeout)
		defer cancel()

		resp := r.Snapshot(ctx)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			opts.Logger.Warn("runtimeintrospect: encode failed", "error", err)
		}
	})
}
