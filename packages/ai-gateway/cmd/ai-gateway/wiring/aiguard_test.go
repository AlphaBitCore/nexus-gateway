package wiring

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	creddecrypt "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/decrypt"
	credmanager "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/aiguard"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/configstore"
)

// TestWriterBackedTrafficSink_nilSinkIsNoOp verifies nil sink does not panic.
func TestWriterBackedTrafficSink_nilSinkIsNoOp(t *testing.T) {
	var sink *WriterBackedTrafficSink
	// Should not panic.
	sink.Emit(context.Background(), aiguard.TrafficEvent{})
}

// TestWriterBackedTrafficSink_nilWriterIsNoOp verifies non-nil sink with nil
// Writer is a no-op.
func TestWriterBackedTrafficSink_nilWriterIsNoOp(t *testing.T) {
	sink := &WriterBackedTrafficSink{Writer: nil}
	// Should not panic.
	sink.Emit(context.Background(), aiguard.TrafficEvent{Decision: "allow"})
}

// TestWriterBackedTrafficSink_Emit_cacheHit verifies CacheHit=true is handled
// without panic. Uses a real Writer backed by a nil producer.
func TestWriterBackedTrafficSink_Emit_cacheHit(t *testing.T) {
	opsReg := registry.NewRegistry(prometheus.NewRegistry())
	w := audit.NewWriter(nil, "test.queue", opsReg, discardLogger())
	t.Cleanup(w.Close)

	sink := &WriterBackedTrafficSink{Writer: w}
	sink.Emit(context.Background(), aiguard.TrafficEvent{
		CacheHit:       true,
		Decision:       "allow",
		DetectorType:   "pii",
		JudgeLatencyMs: 120,
	})
}

// TestWriterBackedTrafficSink_Emit_cacheMiss verifies CacheHit=false path.
func TestWriterBackedTrafficSink_Emit_cacheMiss(t *testing.T) {
	opsReg := registry.NewRegistry(prometheus.NewRegistry())
	w := audit.NewWriter(nil, "test.queue", opsReg, discardLogger())
	t.Cleanup(w.Close)

	sink := &WriterBackedTrafficSink{Writer: w}
	sink.Emit(context.Background(), aiguard.TrafficEvent{
		CacheHit:         false,
		Decision:         "block",
		PromptTokens:     100,
		CompletionTokens: 50,
		CostUsd:          0.001,
		BackendMode:      "configured_provider",
	})
}

// TestLiveClassifier_buildBackend_unknownMode returns error.
func TestLiveClassifier_buildBackend_unknownMode(t *testing.T) {
	lc := &LiveClassifier{Logger: discardLogger()}
	cfg := &configstore.AIGuardConfig{BackendMode: "unknown_mode"}
	_, err := lc.buildBackend(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for unknown backend mode")
	}
}

// TestLiveClassifier_buildBackend_configuredProvider_missingProviderID.
func TestLiveClassifier_buildBackend_configuredProvider_missingProviderID(t *testing.T) {
	lc := &LiveClassifier{Logger: discardLogger()}
	empty := ""
	cfg := &configstore.AIGuardConfig{
		BackendMode: "configured_provider",
		ProviderID:  &empty,
	}
	_, err := lc.buildBackend(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for missing providerId")
	}
}

// TestLiveClassifier_buildBackend_configuredProvider_missingModelID.
func TestLiveClassifier_buildBackend_configuredProvider_missingModelID(t *testing.T) {
	lc := &LiveClassifier{Logger: discardLogger()}
	pid := "prov-1"
	empty := ""
	cfg := &configstore.AIGuardConfig{
		BackendMode: "configured_provider",
		ProviderID:  &pid,
		ModelID:     &empty,
	}
	_, err := lc.buildBackend(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for missing modelId")
	}
}

// TestLiveClassifier_buildBackend_configuredProvider_nilResolver.
func TestLiveClassifier_buildBackend_configuredProvider_nilResolver(t *testing.T) {
	lc := &LiveClassifier{
		Resolver: nil,
		Adapters: nil,
		Logger:   discardLogger(),
	}
	pid := "prov-1"
	mid := "model-1"
	cfg := &configstore.AIGuardConfig{
		BackendMode: "configured_provider",
		ProviderID:  &pid,
		ModelID:     &mid,
	}
	_, err := lc.buildBackend(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when resolver and adapters are nil")
	}
}

// TestLiveClassifier_buildBackend_externalURL_missingURL.
func TestLiveClassifier_buildBackend_externalURL_missingURL(t *testing.T) {
	lc := &LiveClassifier{Logger: discardLogger()}
	empty := ""
	cfg := &configstore.AIGuardConfig{
		BackendMode: "external_url",
		ExternalURL: &empty,
	}
	_, err := lc.buildBackend(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for missing externalUrl")
	}
}

// TestLiveClassifier_buildBackend_externalURL_noCredSuccess happy path.
func TestLiveClassifier_buildBackend_externalURL_noCredSuccess(t *testing.T) {
	lc := &LiveClassifier{Logger: discardLogger()}
	url := "https://classifier.example.com"
	cfg := &configstore.AIGuardConfig{
		BackendMode: "external_url",
		ExternalURL: &url,
	}
	backend, err := lc.buildBackend(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if backend == nil {
		t.Fatal("expected non-nil ExternalBackend")
	}
}

// TestLiveClassifier_buildBackend_configuredProvider_success builds an
// AdapterBackend when providerID, modelID, resolver, and adapters are present.
func TestLiveClassifier_buildBackend_configuredProvider_success(t *testing.T) {
	allowlist, err := InitForwardHeaderAllowlist(forwardheader.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	adapterReg := InitProviderRegistry(allowlist, discardLogger())

	pid := "prov-1"
	mid := "model-uuid"
	cfg := &configstore.AIGuardConfig{
		BackendMode: "configured_provider",
		ProviderID:  &pid,
		ModelID:     &mid,
	}

	lc := &LiveClassifier{
		Resolver: &stubResolverForAIGuard{},
		Adapters: adapterReg,
		DB:       nil, // nil DB → priceLookup returns (0,0)
		Logger:   discardLogger(),
	}
	backend, err := lc.buildBackend(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if backend == nil {
		t.Fatal("expected non-nil AdapterBackend")
	}
}

// stubResolverForAIGuard satisfies provtarget.Resolver for AIGuard tests.
type stubResolverForAIGuard struct{}

func (s *stubResolverForAIGuard) Resolve(_ context.Context, _, _ string, _ provtarget.ResolveHints) (provcore.CallTarget, error) {
	return provcore.CallTarget{}, nil
}

// stubEncryptedCredSource is a credential Source that returns a single
// AES-GCM-encrypted credential for GetCredentialByID calls.
// Used by TestLiveClassifier_buildBackend_externalURL_withRealCredSuccess.
type stubEncryptedCredSource struct {
	cred *store.Credential
}

func (s *stubEncryptedCredSource) GetCredentialByID(_ context.Context, _ string) (*store.Credential, error) {
	return s.cred, nil
}
func (s *stubEncryptedCredSource) GetCredentialForProvider(_ context.Context, _ string) (*store.Credential, error) {
	return s.cred, nil
}
func (s *stubEncryptedCredSource) ListCredentialsForProvider(_ context.Context, _ string) ([]store.Credential, error) {
	if s.cred != nil {
		return []store.Credential{*s.cred}, nil
	}
	return nil, nil
}

// stubAIGuardLoader is a stub Loader for aiguard.ConfigCache in tests.
type stubAIGuardLoader struct {
	cfg *configstore.AIGuardConfig
	err error
}

func (s *stubAIGuardLoader) Load(_ context.Context) (*configstore.AIGuardConfig, error) {
	return s.cfg, s.err
}

// TestLiveClassifier_Classify_configLoadError verifies that Classify propagates
// config-load errors.
func TestLiveClassifier_Classify_configLoadError(t *testing.T) {
	loader := &stubAIGuardLoader{err: errors.New("config unavailable")}
	cache := aiguard.NewConfigCache(loader, 5*time.Second, discardLogger())

	lc := &LiveClassifier{
		ConfigCache: cache,
		Logger:      discardLogger(),
	}
	_, err := lc.Classify(context.Background(), aiguard.Request{})
	if err == nil {
		t.Fatal("expected error when config load fails")
	}
}

// TestLiveClassifier_Classify_buildBackendError verifies that Classify propagates
// the buildBackend error (line 78 in aiguard.go). Uses an unknown BackendMode
// so buildBackend returns an error while ConfigCache.Get succeeds.
func TestLiveClassifier_Classify_buildBackendError(t *testing.T) {
	loader := &stubAIGuardLoader{
		cfg: &configstore.AIGuardConfig{
			BackendMode: "unknown_mode_xyz", // causes buildBackend to return error
		},
	}
	cache := aiguard.NewConfigCache(loader, 5*time.Second, discardLogger())

	lc := &LiveClassifier{
		ConfigCache: cache,
		Logger:      discardLogger(),
	}
	_, err := lc.Classify(context.Background(), aiguard.Request{})
	if err == nil {
		t.Fatal("expected error when buildBackend fails with unknown mode")
	}
}

// TestLiveClassifier_Classify_withConfiguredProviderBackend verifies the
// full Classify path with a configured_provider backend.
func TestLiveClassifier_Classify_withConfiguredProviderBackend(t *testing.T) {
	pid := "prov-1"
	mid := "model-uuid"
	loader := &stubAIGuardLoader{
		cfg: &configstore.AIGuardConfig{
			BackendMode: "configured_provider",
			ProviderID:  &pid,
			ModelID:     &mid,
		},
	}
	cache := aiguard.NewConfigCache(loader, 5*time.Second, discardLogger())

	allowlist, err := InitForwardHeaderAllowlist(forwardheader.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	adapterReg := InitProviderRegistry(allowlist, discardLogger())

	lc := &LiveClassifier{
		ConfigCache: cache,
		Resolver:    &stubResolverForAIGuard{},
		Adapters:    adapterReg,
		Logger:      discardLogger(),
	}
	// This will call Classify → buildBackend → aiguard.Classify. The AdapterBackend
	// will try to Resolve and build a call target. Since stubResolverForAIGuard
	// returns an empty CallTarget, the actual classify will fail with a backend
	// error (no registry entry for empty adapter type). We only assert no panic.
	_, _ = lc.Classify(context.Background(), aiguard.Request{})
}

// buildBackend — external_url with valid credential (line 136 coverage)

// TestLiveClassifier_buildBackend_externalURL_withRealCredSuccess verifies the
// `apiKey = key` assignment at line 136 of aiguard.go when ExternalCredentialID
// is non-empty AND GetDecrypted succeeds (real AES-GCM encrypted credential).
func TestLiveClassifier_buildBackend_externalURL_withRealCredSuccess(t *testing.T) {
	const testKeyHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	// Reuse testEncryptForCred helper from coverage_gap2_test.go (same package).
	ctHex, ivHex, tagHex := testEncryptForCred(t, testKeyHex, "sk-external-key")

	cred := &store.Credential{
		ID:              "ext-cred-1",
		Name:            "ExternalCred",
		ProviderID:      "prov-ext",
		EncryptedKey:    ctHex,
		EncryptionIv:    ivHex,
		EncryptionTag:   tagHex,
		EncryptionKeyID: "v1",
		Enabled:         true,
		Status:          "active",
	}
	d, err := creddecrypt.NewDecryptor(testKeyHex)
	if err != nil {
		t.Fatalf("NewDecryptor: %v", err)
	}
	src := &stubEncryptedCredSource{cred: cred}
	mgr := credmanager.NewManager(src, d, discardLogger())

	url := "https://external-classifier.example.com"
	credID := "ext-cred-1"
	cfg := &configstore.AIGuardConfig{
		BackendMode:          "external_url",
		ExternalURL:          &url,
		ExternalCredentialID: &credID,
	}

	lc := &LiveClassifier{
		CredentialMgr: mgr,
		Logger:        discardLogger(),
	}
	backend, err := lc.buildBackend(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if backend == nil {
		t.Fatal("expected non-nil ExternalBackend with real credential")
	}
}
