package spillfactory

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore/async"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestNew_DisabledReturnsNilNil(t *testing.T) {
	store, err := New(FactoryConfig{Enabled: false}, testLogger())
	if err != nil {
		t.Errorf("disabled config returned err: %v", err)
	}
	if store != nil {
		t.Errorf("disabled config returned non-nil store: %T", store)
	}
}

func TestNew_DefaultLogger(t *testing.T) {
	// Passing nil logger should not panic — the factory defaults to slog.Default.
	cfg := FactoryConfig{
		Enabled: true,
		Backend: "localfs",
		Localfs: LocalfsOptions{Root: t.TempDir()},
	}
	if _, err := New(cfg, nil); err != nil {
		t.Errorf("nil logger should not error: %v", err)
	}
}

func TestNew_UnknownBackendErrors(t *testing.T) {
	cfg := FactoryConfig{Enabled: true, Backend: "azure-blob"}
	store, err := New(cfg, testLogger())
	if err == nil {
		t.Fatalf("unknown backend should error; got store=%T", store)
	}
	if !strings.Contains(err.Error(), "azure-blob") {
		t.Errorf("error message should name the bad backend, got: %v", err)
	}
	if !strings.Contains(err.Error(), "localfs, s3") {
		t.Errorf("error message should hint supported backends, got: %v", err)
	}
}

func TestNew_EmptyBackendDefaultsToLocalfs(t *testing.T) {
	// "" backend with a localfs.Root should successfully construct.
	// Without the default-to-localfs branch, a minimal YAML would error.
	cfg := FactoryConfig{
		Enabled: true,
		Backend: "",
		Localfs: LocalfsOptions{Root: t.TempDir()},
	}
	store, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("empty backend should default to localfs: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil store")
	}
}

func TestNew_LocalfsRequiresRoot(t *testing.T) {
	// localfs.New errors when Root is empty (per its own validation).
	// Verify the factory propagates that error with context.
	cfg := FactoryConfig{Enabled: true, Backend: "localfs", Localfs: LocalfsOptions{Root: ""}}
	_, err := New(cfg, testLogger())
	if err == nil {
		t.Fatal("missing localfs.Root should error")
	}
	if !strings.Contains(err.Error(), "localfs") {
		t.Errorf("error should mention localfs backend: %v", err)
	}
}

func TestPerObjectCap_S3Override(t *testing.T) {
	cfg := FactoryConfig{Backend: "s3", S3: S3Options{PerObjectCap: 50 * 1024 * 1024}}
	if got := cfg.PerObjectCap(); got != 50*1024*1024 {
		t.Errorf("got %d, want %d", got, 50*1024*1024)
	}
}

func TestPerObjectCap_LocalfsOverride(t *testing.T) {
	cfg := FactoryConfig{Backend: "localfs", Localfs: LocalfsOptions{PerObjectCap: 10 * 1024 * 1024}}
	if got := cfg.PerObjectCap(); got != 10*1024*1024 {
		t.Errorf("got %d, want %d", got, 10*1024*1024)
	}
}

func TestPerObjectCap_EmptyBackendUsesLocalfs(t *testing.T) {
	// "" backend → localfs path. PerObjectCap from Localfs block.
	cfg := FactoryConfig{Backend: "", Localfs: LocalfsOptions{PerObjectCap: 7 * 1024 * 1024}}
	if got := cfg.PerObjectCap(); got != 7*1024*1024 {
		t.Errorf("got %d, want %d", got, 7*1024*1024)
	}
}

func TestPerObjectCap_DefaultFallback(t *testing.T) {
	// No PerObjectCap configured anywhere → 256 MiB fallback.
	const want = int64(256 * 1024 * 1024)
	if got := (FactoryConfig{Backend: "localfs"}).PerObjectCap(); got != want {
		t.Errorf("localfs default: got %d want %d", got, want)
	}
	if got := (FactoryConfig{Backend: "s3"}).PerObjectCap(); got != want {
		t.Errorf("s3 default: got %d want %d", got, want)
	}
	if got := (FactoryConfig{}).PerObjectCap(); got != want {
		t.Errorf("zero-value config: got %d want %d", got, want)
	}
}

func TestPerObjectCap_S3UsesS3BlockNotLocalfs(t *testing.T) {
	// A misconfigured FactoryConfig with values in both blocks must
	// honour the Backend field; falling back to Localfs when Backend="s3"
	// would silently apply the wrong cap.
	cfg := FactoryConfig{
		Backend: "s3",
		S3:      S3Options{PerObjectCap: 1024},
		Localfs: LocalfsOptions{PerObjectCap: 9999},
	}
	if got := cfg.PerObjectCap(); got != 1024 {
		t.Errorf("s3 backend read localfs block: got %d", got)
	}
}

// TestNew_S3Backend covers the s3 switch arm. The AWS SDK's
// s3.NewFromConfig wraps a real http.Client but performs no network
// I/O during construction — so a minimal config with an obviously
// invalid endpoint still constructs cleanly. We only need to drive
// the switch case + the success-return statement; the actual S3
// connectivity is tested at the integration tier.
func TestNew_S3Backend(t *testing.T) {
	cfg := FactoryConfig{
		Enabled: true,
		Backend: "s3",
		S3: S3Options{
			Bucket:        "test-bucket",
			Region:        "us-east-1",
			Endpoint:      "http://localhost:9999", // not contacted at construction
			Prefix:        "spill/",
			UsePathStyle:  true,
			RetentionDays: 7,
		},
	}
	store, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("s3 backend construction should not require network: %v", err)
	}
	if store == nil {
		t.Fatal("expected non-nil store for s3 backend")
	}
}

// TestNew_AsyncWrapsLocalfs verifies cfg.Async=true returns an
// async.AsyncStore wrapper. The underlying backend is exercised via
// the wrapper's pass-through methods; we type-assert to confirm the
// wrapper is in front so callers can defer Close.
func TestNew_AsyncWrapsLocalfs(t *testing.T) {
	cfg := FactoryConfig{
		Enabled: true,
		Backend: "localfs",
		Localfs: LocalfsOptions{Root: t.TempDir()},
		Async:   true,
	}
	store, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("async wrap should construct: %v", err)
	}
	asyncStore, ok := store.(*async.AsyncStore)
	if !ok {
		t.Fatalf("expected *async.AsyncStore wrapper; got %T", store)
	}
	if err := asyncStore.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestNew_AsyncWithQueueCapacity verifies the AsyncQueueCapacity tunable
// is plumbed through. We can't directly read async.Options.QueueCapacity
// from outside, but we can confirm construction succeeds with the
// non-default value.
func TestNew_AsyncWithQueueCapacity(t *testing.T) {
	cfg := FactoryConfig{
		Enabled:            true,
		Backend:            "localfs",
		Localfs:            LocalfsOptions{Root: t.TempDir()},
		Async:              true,
		AsyncQueueCapacity: 32,
	}
	store, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("async with custom queue cap should construct: %v", err)
	}
	if _, ok := store.(*async.AsyncStore); !ok {
		t.Fatalf("expected *async.AsyncStore wrapper; got %T", store)
	}
	_ = store.(*async.AsyncStore).Close(context.Background())
}

// TestNew_AsyncWrapsS3 verifies the async wrapping path runs for an
// s3 backend too (the SDK config builds without network I/O, so this
// stays a unit test).
func TestNew_AsyncWrapsS3(t *testing.T) {
	cfg := FactoryConfig{
		Enabled: true,
		Backend: "s3",
		S3: S3Options{
			Bucket: "b", Region: "us-east-1",
			Endpoint: "http://localhost:9999", UsePathStyle: true,
		},
		Async: true,
	}
	store, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("async + s3 should construct: %v", err)
	}
	if _, ok := store.(*async.AsyncStore); !ok {
		t.Fatalf("expected *async.AsyncStore wrapper; got %T", store)
	}
	_ = store.(*async.AsyncStore).Close(context.Background())
}
