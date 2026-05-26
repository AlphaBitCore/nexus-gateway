package loaders

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/telemetry"
)

// observabilityServiceName is the OTEL service.name attribute stamped onto
// every compliance-proxy trace span. Kept as a package-level constant so the
// pure decode helper does not have to take it as a parameter.
const observabilityServiceName = "nexus-compliance-proxy"

// LoadObservabilityConfig reads observability config from system_metadata.
// The interesting decision logic (missing row → defaults, generic DB error
// surfaces, malformed JSON → telemetry-disabled defaults so a typo in the
// admin UI cannot block startup) lives in decodeObservabilityResult so it
// can be unit-tested without a live database.
func LoadObservabilityConfig(ctx context.Context, db *sql.DB) (*telemetry.Config, error) {
	var val []byte
	err := db.QueryRowContext(ctx, `SELECT value FROM system_metadata WHERE key = 'observability.config'`).Scan(&val)
	return decodeObservabilityResult(val, err)
}

// decodeObservabilityResult applies the three-way decision (missing row →
// telemetry-disabled defaults with the service name stamped; generic err →
// propagate; malformed JSON → telemetry-disabled defaults with a nil error
// because observability config is best-effort and must not block startup;
// success → decoded config) over a query outcome.
func decodeObservabilityResult(val []byte, queryErr error) (*telemetry.Config, error) {
	if queryErr != nil {
		if errors.Is(queryErr, sql.ErrNoRows) {
			return &telemetry.Config{ServiceName: observabilityServiceName}, nil
		}
		return nil, queryErr
	}
	var stored struct {
		OtelEnabled  bool    `json:"otelEnabled"`
		SamplingRate float64 `json:"samplingRate"`
	}
	if err := json.Unmarshal(val, &stored); err != nil {
		return &telemetry.Config{ServiceName: observabilityServiceName}, nil //nolint:nilerr // observability config is best-effort; surface telemetry-disabled defaults rather than block startup
	}
	return &telemetry.Config{
		Enabled:      stored.OtelEnabled,
		SamplingRate: stored.SamplingRate,
		ServiceName:  observabilityServiceName,
	}, nil
}
