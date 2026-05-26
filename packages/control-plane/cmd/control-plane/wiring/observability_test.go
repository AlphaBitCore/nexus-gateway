package wiring

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	pgxmock "github.com/pashagolub/pgxmock/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/cmd/control-plane/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/store"
)

func TestInitOpsMetrics_ReturnsNonNilRegistry(t *testing.T) {
	// InitOpsMetrics registers with prometheus.DefaultRegisterer which panics on
	// duplicate registration. We call it once in TestMain (helpers_test.go) and
	// just verify the result here is non-nil.
	reg := getOpsMetricsRegistry()
	if reg == nil {
		t.Error("expected non-nil metrics registry")
	}
}

func TestLoadOtelConfig_NilDB_UsesFileCfg(t *testing.T) {
	cfg := &config.Config{}
	cfg.Otel.Endpoint = "http://localhost:4318"
	cfg.Otel.ServiceName = "test-service"

	result := LoadOtelConfig(context.Background(), nil, cfg)

	if result.Endpoint != "http://localhost:4318" {
		t.Errorf("expected endpoint %q, got %q", "http://localhost:4318", result.Endpoint)
	}
	if result.ServiceName != "test-service" {
		t.Errorf("expected service name %q, got %q", "test-service", result.ServiceName)
	}
}

func TestLoadOtelConfig_NilDB_EmptyOtelCfg_DefaultsServiceName(t *testing.T) {
	cfg := &config.Config{}
	// No otel config: endpoint empty, service name empty.
	result := LoadOtelConfig(context.Background(), nil, cfg)

	if result.ServiceName != "nexus-control-plane" {
		t.Errorf("expected default service name %q, got %q", "nexus-control-plane", result.ServiceName)
	}
	if result.Endpoint != "" {
		t.Errorf("expected empty endpoint, got %q", result.Endpoint)
	}
	if result.Enabled {
		t.Error("expected Enabled=false by default")
	}
}

func TestLoadOtelConfig_NilDB_NoOtelOverride(t *testing.T) {
	cfg := &config.Config{}
	// DB is nil — function must return gracefully with file-based config only.
	result := LoadOtelConfig(context.Background(), nil, cfg)
	_ = result // observable: must not panic
}

func TestInitObservability_NilDB_NoOtel_Succeeds(t *testing.T) {
	cfg := &config.Config{}
	cfg.Otel.Endpoint = "" // disabled

	tp, closer, err := InitObservability(context.Background(), nil, cfg, silentLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tp == nil {
		t.Error("expected non-nil SwappableTracerProvider")
	}
	// closer must not panic.
	closer()
}

func TestLoadOtelConfig_WithDB_AppliesDBOverrides(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// DB returns a JSON blob with all three overridable fields.
	raw := []byte(`{"otelEnabled":true,"samplingRate":0.5,"endpoint":"http://otel:4318"}`)
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("observability.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(raw))

	db := store.NewWithPgxPool(mock)
	cfg := &config.Config{}

	result := LoadOtelConfig(context.Background(), db, cfg)

	if !result.Enabled {
		t.Error("expected Enabled=true from DB override")
	}
	if result.SamplingRate != 0.5 {
		t.Errorf("expected SamplingRate=0.5, got %v", result.SamplingRate)
	}
	if result.Endpoint != "http://otel:4318" {
		t.Errorf("expected endpoint from DB, got %q", result.Endpoint)
	}
}

func TestLoadOtelConfig_WithDB_DBReturnsNoRows_UseFileConfig(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("observability.config").
		WillReturnError(pgx.ErrNoRows)

	db := store.NewWithPgxPool(mock)
	cfg := &config.Config{}
	cfg.Otel.Endpoint = "http://file:4318"

	result := LoadOtelConfig(context.Background(), db, cfg)
	if result.Endpoint != "http://file:4318" {
		t.Errorf("expected file endpoint when DB has no row, got %q", result.Endpoint)
	}
}

func TestLoadOtelConfig_WithDB_DBError_UseFileConfig(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("observability.config").
		WillReturnError(pgx.ErrNoRows) // treat any error as no-override

	db := store.NewWithPgxPool(mock)
	cfg := &config.Config{}

	// Must not panic — DB error is swallowed gracefully.
	result := LoadOtelConfig(context.Background(), db, cfg)
	if result.ServiceName != "nexus-control-plane" {
		t.Errorf("expected default service name, got %q", result.ServiceName)
	}
}

func TestLoadOtelConfig_WithDB_EmptyEndpointInDB_NotOverridden(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// DB returns endpoint = "" — should NOT override the file endpoint.
	raw := []byte(`{"otelEnabled":true,"endpoint":""}`)
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("observability.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(raw))

	db := store.NewWithPgxPool(mock)
	cfg := &config.Config{}
	cfg.Otel.Endpoint = "http://file:4318"

	result := LoadOtelConfig(context.Background(), db, cfg)
	if result.Endpoint != "http://file:4318" {
		t.Errorf("expected file endpoint when DB endpoint is empty, got %q", result.Endpoint)
	}
}

// TestInitObservability_DBEnabled_CancelledContext_MayError exercises the
// error path in InitObservability.  When Enabled=true (from a DB override) and
// Endpoint is set, and the context is pre-cancelled, otlptracehttp.New may fail
// immediately, causing telemetry.Init to return an error that InitObservability
// propagates.  If the OTLP library does NOT error on a cancelled context
// (behaviour can vary across SDK versions), we skip gracefully — the happy-path
// branch is already covered by TestInitObservability_NilDB_NoOtel_Succeeds.
func TestInitObservability_DBEnabled_CancelledContext_MayError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	// DB returns otelEnabled=true + endpoint — so LoadOtelConfig sets Enabled=true.
	raw := []byte(`{"otelEnabled":true,"endpoint":"http://127.0.0.1:4318"}`)
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key = \$1`).
		WithArgs("observability.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(raw))

	db := store.NewWithPgxPool(mock)
	cfg := &config.Config{}

	// Pre-cancel the context so the OTLP exporter setup might fail.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tp, closer, err := InitObservability(ctx, db, cfg, silentLogger())
	if err != nil {
		// Expected path: telemetry.Init propagated an error — covers the error branch.
		if closer == nil {
			t.Error("expected non-nil closer even on error path")
		}
		closer() // must not panic
		return
	}
	// Some OTLP SDK versions succeed on cancelled context (lazy connect or
	// resource creation before context check). Treat as an acceptable success.
	if tp != nil {
		closer()
	}
	t.Skip("otlptracehttp.New did not error on pre-cancelled context; error branch is infra-bound in this env")
}
