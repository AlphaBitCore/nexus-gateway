package runtimeapi

import (
	"log/slog"
	"net/http"
)

// Config holds the injection points needed to stand up the AI Gateway
// runtimeapi. APIToken is read by the auth middleware; Thing exposes
// the shadow snapshot accessors used by the read-only /runtime/* endpoints.
type Config struct {
	APIToken string
	Thing    ThingSnapshotter
	Logger   *slog.Logger
}

// Server bundles the runtimeapi ServeMux with its auth and shadow
// dependencies. Callers either serve directly off s.mux (unit tests) or
// graft the routes onto an existing parent mux via Mount (production).
type Server struct {
	mux    *http.ServeMux
	auth   *auth
	thing  ThingSnapshotter
	logger *slog.Logger
}

// New constructs a Server and pre-registers the read routes on an
// internal mux. Callers can use s.mux directly or call Mount to graft
// the routes onto an existing ServeMux.
func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Thing == nil {
		panic("runtimeapi: Thing is required")
	}
	s := &Server{
		mux:    http.NewServeMux(),
		auth:   newAuth(cfg.APIToken),
		thing:  cfg.Thing,
		logger: cfg.Logger,
	}

	s.mux.HandleFunc("GET /runtime/config", s.auth.require(s.handleRuntimeConfig))
	s.mux.HandleFunc("GET /runtime/config/{key}", s.auth.require(s.handleRuntimeConfigKey))
	s.mux.HandleFunc("GET /runtime/sync-status", s.auth.require(s.handleRuntimeSyncStatus))
	s.mux.HandleFunc("GET /runtime/health", s.auth.require(s.handleRuntimeHealth))

	return s
}

// Mount attaches the runtimeapi routes to an existing mux, allowing
// main.go to reuse the same HTTP server for /v1 traffic and /runtime
// ops. Same auth rules as New apply.
func (s *Server) Mount(parent *http.ServeMux) {
	parent.HandleFunc("GET /runtime/config", s.auth.require(s.handleRuntimeConfig))
	parent.HandleFunc("GET /runtime/config/{key}", s.auth.require(s.handleRuntimeConfigKey))
	parent.HandleFunc("GET /runtime/sync-status", s.auth.require(s.handleRuntimeSyncStatus))
	parent.HandleFunc("GET /runtime/health", s.auth.require(s.handleRuntimeHealth))
}
