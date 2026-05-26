package wiring

import (
	"context"
	"database/sql"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/internal/config/loaders"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
)

// LoadOtelConfig reads OTEL configuration from the compliance DB.
// Falls back to a minimal default when db is nil or the query fails.
func LoadOtelConfig(ctx context.Context, db *sql.DB) telemetry.Config {
	result := telemetry.Config{ServiceName: "nexus-compliance-proxy"}
	if db == nil {
		return result
	}
	cfg, err := loaders.LoadObservabilityConfig(ctx, db)
	if err != nil || cfg == nil {
		return result
	}
	return *cfg
}
