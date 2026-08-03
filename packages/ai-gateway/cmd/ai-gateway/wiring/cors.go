// cors.go — assembly of the gateway's CORS middleware configuration.
//
// The request-header allowlist is composed, not configured: what the
// gateway itself reads (traffic.AcceptHeaders) ∪ what it forwards to
// providers (the resolved forward-header allowlist — derived at runtime so
// the two can never drift) ∪ whatever extra names the operator's browser
// clients send. Config extends the set; it cannot shrink it below what the
// gateway needs, which is the failure mode hand-maintained lists kept
// producing. The response-side expose list is traffic.ExposeHeaders,
// never operator-authored.
package wiring

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// corsMiddleware builds the CORS wrapper from the operator config and the
// resolved forward-header allowlist. Callers gate on cfg.CORS.Enabled.
func corsMiddleware(deps RouteDeps) func(http.Handler) http.Handler {
	allowedMethods := deps.Config.CORS.AllowedMethods
	if len(allowedMethods) == 0 {
		allowedMethods = []string{"GET", "POST", "OPTIONS"}
	}
	fwd := deps.Allowlist
	if fwd == nil {
		fwd = forwardheader.Default()
	}
	builtin := middleware.UnionHeaderNames(traffic.AcceptHeaders, fwd.AllRequestHeaders())
	allowedHeaders := middleware.UnionHeaderNames(builtin, deps.Config.CORS.AllowedHeaders)
	logRedundantCORSHeaders(builtin, deps.Config.CORS.AllowedHeaders)
	slog.Info("CORS enabled", "origins", deps.Config.CORS.AllowedOrigins)
	return middleware.CORS(middleware.CORSConfig{
		AllowedOrigins: deps.Config.CORS.AllowedOrigins,
		AllowedMethods: allowedMethods,
		AllowedHeaders: allowedHeaders,
		ExposeHeaders:  traffic.ExposeHeaders,
		MaxAge:         deps.Config.CORS.MaxAgeSec,
	})
}

// logRedundantCORSHeaders points out yaml entries the built-in set already
// covers, so a config that predates the extend-only semantics sheds its
// fossils instead of carrying them forever. Informational: redundancy is
// harmless.
func logRedundantCORSHeaders(builtin, extras []string) {
	builtinSet := make(map[string]bool, len(builtin))
	for _, h := range builtin {
		builtinSet[strings.ToLower(h)] = true
	}
	var redundant []string
	for _, h := range extras {
		if builtinSet[strings.ToLower(strings.TrimSpace(h))] {
			redundant = append(redundant, h)
		}
	}
	if len(redundant) > 0 {
		slog.Info("cors.allowedHeaders entries already covered by the built-in allowlist; they can be deleted from the yaml",
			"redundant", strings.Join(redundant, ", "))
	}
}
