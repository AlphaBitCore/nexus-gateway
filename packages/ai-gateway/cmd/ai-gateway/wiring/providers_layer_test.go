package wiring

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pashagolub/pgxmock/v4"

	cachelayer "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/layer"
	credmanager "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/credentials/manager"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// providerColsForLayer mirrors the SELECT list in cachelayer/loaders.go
// for provider queries.
var providerColsForLayer = []string{
	"id", "name", "displayName", "adapter_type", "baseUrl",
	"pathPrefix", "apiVersion", "region", "enabled",
}

// modelColsForLayer mirrors the SELECT list for model queries (23 columns).
var modelColsForLayer = []string{
	"id", "code", "name", "providerId", "p_name", "p_adapter_type",
	"p_displayName", "p_baseUrl", "providerModelId", "type", "enabled",
	"inputPricePerMillion", "outputPricePerMillion",
	"cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
	"features", "maxContextTokens", "maxOutputTokens",
	"aliases", "inputModalities", "outputModalities",
	"lifecycle", "capabilityJson",
}

// newLayerWithMock creates a Layer backed by pgxmock. No queries are
// pre-loaded; the layer is empty at construction (cachelayer uses explicit
// Start/Reload to load snapshots, not lazy loading on Get).
func newLayerWithMock(t *testing.T) (pgxmock.PgxPoolIface, *cachelayer.Layer) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)

	db := store.NewWithPgxPool(mock)
	l, err := cachelayer.NewWithPool(db, mock, discardLogger(), cachelayer.Config{})
	if err != nil {
		t.Fatalf("NewWithPool: %v", err)
	}
	return mock, l
}

// TestProviderStoreAdapter_GetProviderByID_notFound verifies error when
// provider is not in the layer snapshot (empty snapshot → GetProvider returns
// "not found" error). The snapshot starts empty; no reload needed.
func TestProviderStoreAdapter_GetProviderByID_notFound(t *testing.T) {
	_, l := newLayerWithMock(t)
	// No reload means the snapshot is empty; GetProvider returns not found.
	adapter := &providerStoreAdapter{layer: l}
	_, err := adapter.GetProviderByID(context.Background(), "nonexistent-id")
	if err == nil {
		t.Fatal("expected error for missing provider")
	}
}

// TestModelStoreAdapter_GetModelByID_notFound verifies error when model is
// not in the layer snapshot.
func TestModelStoreAdapter_GetModelByID_notFound(t *testing.T) {
	_, l := newLayerWithMock(t)
	// Empty snapshot returns not found.
	adapter := &modelStoreAdapter{layer: l}
	_, err := adapter.GetModelByID(context.Background(), "nonexistent-model-id")
	if err == nil {
		t.Fatal("expected error for missing model")
	}
}

// TestProviderStoreAdapter_GetProviderByID_extras verifies that region,
// apiVersion, and pathPrefix are projected into the Extras map.
// We reload the provider snapshot via mock to populate the in-memory cache.
func TestProviderStoreAdapter_GetProviderByID_extras(t *testing.T) {
	mock, l := newLayerWithMock(t)

	regionStr := "us-east-1"
	apiVerStr := "2024-01-01"
	pathPrefix := "/openai"

	provRows := pgxmock.NewRows(providerColsForLayer).AddRow(
		"prov-uuid",              // id
		"openai",                 // name
		nil,                      // displayName
		"openai",                 // adapter_type
		"https://api.openai.com", // baseUrl
		pathPrefix,               // pathPrefix
		&apiVerStr,               // apiVersion (*string)
		&regionStr,               // region (*string)
		true,                     // enabled
	)
	mock.ExpectQuery(`FROM "Provider"`).WillReturnRows(provRows)

	// Reload to populate the in-memory snapshot.
	if err := l.ReloadProviders(context.Background()); err != nil {
		t.Fatalf("ReloadProviders: %v", err)
	}

	adapter := &providerStoreAdapter{layer: l}
	row, err := adapter.GetProviderByID(context.Background(), "prov-uuid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if row.ID != "prov-uuid" {
		t.Errorf("expected id=prov-uuid, got %q", row.ID)
	}
	if row.Extras["provider.region"] != "us-east-1" {
		t.Errorf("expected provider.region=us-east-1, got %q", row.Extras["provider.region"])
	}
	if row.Extras["provider.apiVersion"] != "2024-01-01" {
		t.Errorf("expected provider.apiVersion, got %q", row.Extras["provider.apiVersion"])
	}
	if row.Extras["provider.pathPrefix"] != pathPrefix {
		t.Errorf("expected provider.pathPrefix=%q, got %q", pathPrefix, row.Extras["provider.pathPrefix"])
	}
	if row.Disabled {
		t.Error("expected Disabled=false for enabled=true provider")
	}
}

// TestProviderStoreAdapter_GetProviderByID_noExtras verifies that extras map
// remains empty when region, apiVersion, pathPrefix are all absent.
func TestProviderStoreAdapter_GetProviderByID_noExtras(t *testing.T) {
	mock, l := newLayerWithMock(t)

	provRows := pgxmock.NewRows(providerColsForLayer).AddRow(
		"prov-2",                    // id
		"anthropic",                 // name
		nil,                         // displayName
		"anthropic",                 // adapter_type
		"https://api.anthropic.com", // baseUrl
		"",                          // pathPrefix (empty)
		(*string)(nil),              // apiVersion (NULL)
		(*string)(nil),              // region (NULL)
		true,                        // enabled
	)
	mock.ExpectQuery(`FROM "Provider"`).WillReturnRows(provRows)

	if err := l.ReloadProviders(context.Background()); err != nil {
		t.Fatalf("ReloadProviders: %v", err)
	}

	adapter := &providerStoreAdapter{layer: l}
	row, err := adapter.GetProviderByID(context.Background(), "prov-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// region, apiVersion, pathPrefix are absent → extras should not have them.
	if v, ok := row.Extras["provider.region"]; ok {
		t.Errorf("expected no provider.region extra, got %q", v)
	}
}

// makeTestModelRow builds a 23-value row matching the cachelayer loadModels
// SELECT (exact column order from loaders.go).
func makeTestModelRow(id, code, providerID string, enabled bool) []any {
	displayName := "OpenAI"
	inP := "3.0"
	outP := "12.0"
	crP := "0.3"
	cwP := "3.75"
	return []any{
		id, code, "model-" + id, providerID,
		"openai", "openai", &displayName, "https://api.openai.com",
		code, "chat", enabled,
		&inP, &outP, &crP, &cwP,
		[]string{"vision"},
		pgtype.Int4{Int32: 128000, Valid: true},
		pgtype.Int4{Int32: 16384, Valid: true},
		[]string{},
		[]string{"text"},
		[]string{"text"},
		"ga",
		[]byte(`{}`),
	}
}

// TestModelStoreAdapter_GetModelByID_found verifies a loaded model is found.
func TestModelStoreAdapter_GetModelByID_found(t *testing.T) {
	mock, l := newLayerWithMock(t)

	modelRows := pgxmock.NewRows(modelColsForLayer).
		AddRow(makeTestModelRow("model-uuid", "gpt-4", "prov-uuid", true)...)
	mock.ExpectQuery(`FROM "Model"`).WillReturnRows(modelRows)

	if err := l.ReloadModels(context.Background()); err != nil {
		t.Fatalf("ReloadModels: %v", err)
	}

	adapter := &modelStoreAdapter{layer: l}
	row, err := adapter.GetModelByID(context.Background(), "model-uuid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if row.ID != "model-uuid" {
		t.Errorf("expected id=model-uuid, got %q", row.ID)
	}
	if row.ProviderID != "prov-uuid" {
		t.Errorf("expected providerId=prov-uuid, got %q", row.ProviderID)
	}
	if row.Disabled {
		t.Error("expected Disabled=false for enabled=true model")
	}
}

// TestNewResolver_nilCredMgrReturnsNil verifies nil credMgr returns nil.
func TestNewResolver_nilCredMgrReturnsNil(t *testing.T) {
	_, l := newLayerWithMock(t)
	r := NewResolver(l, nil, nil)
	if r != nil {
		t.Error("expected nil resolver when credMgr=nil")
	}
}

// TestGeminiKeyResolverFrom_nilCredMgrReturnsNil verifies nil credMgr returns nil.
func TestGeminiKeyResolverFrom_nilCredMgrReturnsNil(t *testing.T) {
	_, l := newLayerWithMock(t)
	r := GeminiKeyResolverFrom(l, nil)
	if r != nil {
		t.Error("expected nil resolver when credMgr=nil")
	}
}

// stubCredSource is a credential Source that returns empty/error responses.
type stubCredSource struct {
	cred *store.Credential
	err  error
}

func (s *stubCredSource) GetCredentialByID(_ context.Context, _ string) (*store.Credential, error) {
	return s.cred, s.err
}
func (s *stubCredSource) GetCredentialForProvider(_ context.Context, _ string) (*store.Credential, error) {
	return s.cred, s.err
}
func (s *stubCredSource) ListCredentialsForProvider(_ context.Context, _ string) ([]store.Credential, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.cred != nil {
		return []store.Credential{*s.cred}, nil
	}
	return nil, nil
}

// TestCredentialStoreAdapter_ResolveForProvider_error verifies error propagation.
func TestCredentialStoreAdapter_ResolveForProvider_error(t *testing.T) {
	src := &stubCredSource{err: errors.New("db unavailable")}
	mgr := credmanager.NewManager(src, nil)
	adapter := &credentialStoreAdapter{mgr: mgr}

	_, _, _, err := adapter.ResolveForProvider(context.Background(), "prov-1", "")
	if err == nil {
		t.Fatal("expected error when source returns error")
	}
}

// TestCredentialStoreAdapter_ListForProvider_error verifies error propagation.
func TestCredentialStoreAdapter_ListForProvider_error(t *testing.T) {
	src := &stubCredSource{err: errors.New("list error")}
	mgr := credmanager.NewManager(src, nil)
	adapter := &credentialStoreAdapter{mgr: mgr}

	_, err := adapter.ListForProvider(context.Background(), "prov-1")
	if err == nil {
		t.Fatal("expected error when source returns error")
	}
}

// TestCredentialStoreAdapter_ListForProvider_empty verifies empty result
// when source returns no credentials.
func TestCredentialStoreAdapter_ListForProvider_empty(t *testing.T) {
	src := &stubCredSource{cred: nil, err: nil}
	mgr := credmanager.NewManager(src, nil)
	adapter := &credentialStoreAdapter{mgr: mgr}

	creds, err := adapter.ListForProvider(context.Background(), "prov-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(creds) != 0 {
		t.Errorf("expected 0 credentials, got %d", len(creds))
	}
}
