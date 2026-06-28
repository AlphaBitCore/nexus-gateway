package wiring

import (
	"context"
	"github.com/goccy/go-json"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/config"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/spillfactory"
)

// errTest is a simple string-based error for test-only errors.
// Defined here so both observability_test.go and other test files can reference it.
type errTest string

func (e errTest) Error() string { return string(e) }

// Ensure Config is used to avoid lint — it is used by TestInitOtelConfig_overrideFromConfig
var _ = config.Config{}

// TestInitOtelConfig_nilDB returns config built from yaml only.
func TestInitOtelConfig_nilDB(t *testing.T) {
	cfg := &config.Config{}
	result := InitOtelConfig(context.Background(), nil, cfg)
	if result.ServiceName != "nexus-ai-gateway" {
		t.Errorf("expected default service name, got %q", result.ServiceName)
	}
}

// TestInitOtelConfig_overrideFromConfig verifies endpoint + serviceName from cfg.
func TestInitOtelConfig_overrideFromConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Otel.Endpoint = "http://otel:4317"
	cfg.Otel.ServiceName = "my-gateway"
	result := InitOtelConfig(context.Background(), nil, cfg)
	if result.Endpoint != "http://otel:4317" {
		t.Errorf("expected endpoint=http://otel:4317, got %q", result.Endpoint)
	}
	if result.ServiceName != "my-gateway" {
		t.Errorf("expected service name=my-gateway, got %q", result.ServiceName)
	}
}

// TestInitOtelConfig_withDB_noRow verifies default config when no row exists.
func TestInitOtelConfig_withDB_noRow(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("observability.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}))

	db := store.NewWithPgxPool(mock)
	cfg := &config.Config{}
	result := InitOtelConfig(context.Background(), db, cfg)
	if result.Enabled {
		t.Error("expected Enabled=false when no DB row")
	}
}

// TestInitOtelConfig_withDB_row verifies enabled flag from DB row.
func TestInitOtelConfig_withDB_row(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	payload, _ := json.Marshal(map[string]any{
		"otelEnabled":  true,
		"samplingRate": 0.5,
	})
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("observability.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(payload))

	db := store.NewWithPgxPool(mock)
	cfg := &config.Config{}
	result := InitOtelConfig(context.Background(), db, cfg)
	if !result.Enabled {
		t.Error("expected Enabled=true from DB row")
	}
	if result.SamplingRate != 0.5 {
		t.Errorf("expected SamplingRate=0.5, got %f", result.SamplingRate)
	}
}

// TestInitOtelConfig_withDB_malformedJSON returns defaults without error.
func TestInitOtelConfig_withDB_malformedJSON(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("observability.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`{bad`)))

	db := store.NewWithPgxPool(mock)
	cfg := &config.Config{}
	result := InitOtelConfig(context.Background(), db, cfg)
	// On parse error it falls back to defaults: Enabled=false.
	if result.Enabled {
		t.Error("expected Enabled=false after malformed JSON")
	}
}

// TestLoadPayloadCaptureConfig_nilDBReturnsDefault verifies nil db returns default.
func TestLoadPayloadCaptureConfig_nilDBReturnsDefault(t *testing.T) {
	cfg, err := LoadPayloadCaptureConfig(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	def := payloadcapture.DefaultConfig()
	if cfg.MaxInlineBodyBytes != def.MaxInlineBodyBytes {
		t.Errorf("expected default config, got %+v", cfg)
	}
}

// TestLoadPayloadCaptureConfig_noRowReturnsDefault verifies missing row returns default.
func TestLoadPayloadCaptureConfig_noRowReturnsDefault(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("payload_capture.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}))

	db := store.NewWithPgxPool(mock)
	cfg, err := LoadPayloadCaptureConfig(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	def := payloadcapture.DefaultConfig()
	if cfg.MaxInlineBodyBytes != def.MaxInlineBodyBytes {
		t.Errorf("expected default config, got %+v", cfg)
	}
}

// TestLoadPayloadCaptureConfig_queryErrorReturnsDefaultAndError verifies
// DB error returns default config and wraps the error.
func TestLoadPayloadCaptureConfig_queryErrorReturnsDefaultAndError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("payload_capture.config").
		WillReturnError(errTest("db query error"))

	db := store.NewWithPgxPool(mock)
	cfg, err := LoadPayloadCaptureConfig(context.Background(), db)
	if err == nil {
		t.Fatal("expected error when DB query fails")
	}
	def := payloadcapture.DefaultConfig()
	if cfg.MaxInlineBodyBytes != def.MaxInlineBodyBytes {
		t.Errorf("expected default config on error, got %+v", cfg)
	}
}

// TestLoadPayloadCaptureConfig_malformedJSONReturnsError verifies that when
// the system_metadata row contains malformed JSON, DecodeConfigJSON fails and
// LoadPayloadCaptureConfig returns DefaultConfig() + error (line 115 in observability.go).
func TestLoadPayloadCaptureConfig_malformedJSONReturnsError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	// Return non-nil but malformed JSON so DecodeConfigJSON fails.
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("payload_capture.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow([]byte(`{invalid json`)))

	db := store.NewWithPgxPool(mock)
	cfg, err := LoadPayloadCaptureConfig(context.Background(), db)
	if err == nil {
		t.Fatal("expected error when JSON is malformed")
	}
	def := payloadcapture.DefaultConfig()
	if cfg.MaxInlineBodyBytes != def.MaxInlineBodyBytes {
		t.Errorf("expected default config on JSON error, got %+v", cfg)
	}
}

// TestInitAuditWriter_nilSpill verifies disabled spill (Enabled=false) succeeds.
func TestInitAuditWriter_nilSpill(t *testing.T) {
	opsReg := registry.NewRegistry(prometheus.NewRegistry())
	pcs := payloadcapture.NewStore(payloadcapture.DefaultConfig())
	// Pass nil MQ producer — AuditWriter does not require a live producer
	// to construct; it degrades gracefully.
	w, normReg, err := InitAuditWriter(nil, spillfactory.FactoryConfig{Enabled: false}, config.AuditConfig{}, pcs, opsReg, discardLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w == nil {
		t.Fatal("expected non-nil AuditWriter")
	}
	if normReg == nil {
		t.Fatal("expected non-nil normalize registry")
	}
}

// TestLoadPayloadCaptureConfig_withJSONRow verifies successful JSON decode path.
func TestLoadPayloadCaptureConfig_withJSONRow(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer mock.Close()

	payload, _ := json.Marshal(map[string]any{
		"storeRequestBody":  true,
		"storeResponseBody": false,
	})
	mock.ExpectQuery(`SELECT value FROM system_metadata WHERE key`).
		WithArgs("payload_capture.config").
		WillReturnRows(pgxmock.NewRows([]string{"value"}).AddRow(payload))

	db := store.NewWithPgxPool(mock)
	cfg, err := LoadPayloadCaptureConfig(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.StoreRequestBody {
		t.Error("expected StoreRequestBody=true from JSON row")
	}
}

// TestInitMetricsRecorder_returnsNonNil verifies recorder construction.
func TestInitMetricsRecorder_returnsNonNil(t *testing.T) {
	opsReg := registry.NewRegistry(prometheus.NewRegistry())
	rec := InitMetricsRecorder(opsReg)
	if rec == nil {
		t.Fatal("expected non-nil metrics recorder")
	}
}
