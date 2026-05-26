package wiring

import (
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/exemption"
)

// InitExemptionStore creates the in-memory store for temporary compliance-hook
// exemptions. The cleanup goroutine is started separately by the caller once
// a context is available.
func InitExemptionStore(logger *slog.Logger) *exemption.Store {
	store := exemption.NewStore(logger)
	slog.Info("exemption store initialized")
	return store
}
