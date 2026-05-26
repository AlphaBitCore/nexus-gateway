// executor.go — TargetExecutor + canonicalbridge wiring.
package wiring

import (
	"log/slog"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/canonicalbridge"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	credstats "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/stats"
)

// InitExecutor builds the canonical bridge and target executor.
// Returns (formatBridge, targetExecutor). Panics on bridge self-check failure
// (programmer error — codec misregistration detected at startup).
func InitExecutor(
	adapterReg *provcore.Registry,
	ptResolver *provtarget.PgResolver,
	healthTracker *store.HealthTracker,
	credStatsBuf *credstats.Buffer,
	logger *slog.Logger,
) (*canonicalbridge.Bridge, *executor.TargetExecutor) {
	formatBridge := canonicalbridge.New(provbuiltins.SchemaCodecs(logger))
	if err := formatBridge.SelfCheck(); err != nil {
		logger.Error("canonical bridge self-check failed", "error", err)
		panic(err)
	}

	targetExecutor := executor.New(adapterReg, ptResolver, healthTracker, formatBridge).WithStats(credStatsBuf)
	return formatBridge, targetExecutor
}
