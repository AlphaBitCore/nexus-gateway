package wiring

import (
	"context"
	"errors"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/forwardheader"
	provtarget "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/target"
)

// TestInitForwardHeaderAllowlist_defaultConfigSucceeds verifies that the
// default config resolves without error.
func TestInitForwardHeaderAllowlist_defaultConfigSucceeds(t *testing.T) {
	cfg := forwardheader.DefaultConfig()
	allowlist, err := InitForwardHeaderAllowlist(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if allowlist == nil {
		t.Fatal("expected non-nil Resolved allowlist")
	}
	// Hash must be non-empty — proves the allowlist content was processed.
	if allowlist.Hash() == "" {
		t.Error("expected non-empty hash from resolved allowlist")
	}
}

// TestNewResolver_nilLayerReturnsNil verifies nil layer short-circuits.
func TestNewResolver_nilLayerReturnsNil(t *testing.T) {
	r := NewResolver(nil, nil, nil)
	if r != nil {
		t.Error("expected nil resolver when layer=nil")
	}
}

// TestGeminiKeyResolverFrom_nilLayerReturnsNil verifies nil layer short-circuits.
func TestGeminiKeyResolverFrom_nilLayerReturnsNil(t *testing.T) {
	r := GeminiKeyResolverFrom(nil, nil)
	if r != nil {
		t.Error("expected nil resolver when layer=nil")
	}
}

// stubCredentialStore implements provtarget.CredentialStore for adapter unit tests.
type stubCredentialStore struct {
	apiKey string
	err    error
	list   []provtarget.CredentialCandidate
}

func (s *stubCredentialStore) ResolveForProvider(_ context.Context, _, credID string) (string, string, string, error) {
	if s.err != nil {
		return "", "", "", s.err
	}
	return s.apiKey, credID, "", nil
}

func (s *stubCredentialStore) ListForProvider(_ context.Context, _ string) ([]provtarget.CredentialCandidate, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.list, nil
}

// stubProviderStore2 implements provtarget.ProviderStore for adapter tests.
type stubProviderStore2 struct {
	row provtarget.ProviderRow
	err error
}

func (s *stubProviderStore2) GetProviderByID(_ context.Context, _ string) (provtarget.ProviderRow, error) {
	if s.err != nil {
		return provtarget.ProviderRow{}, s.err
	}
	return s.row, nil
}

// TestGeminiKeyResolverAdapter_Resolve_providerError verifies error propagation.
func TestGeminiKeyResolverAdapter_Resolve_providerError(t *testing.T) {
	provErr := errors.New("provider not found")
	adapter := &geminiKeyResolverAdapter{
		providers: &stubProviderStore2{err: provErr},
		creds:     &stubCredentialStore{apiKey: "k"},
	}
	_, _, err := adapter.Resolve(context.Background(), "prov-1", "")
	if err == nil {
		t.Fatal("expected error when provider lookup fails")
	}
}

// TestGeminiKeyResolverAdapter_Resolve_credError verifies credential error
// propagation.
func TestGeminiKeyResolverAdapter_Resolve_credError(t *testing.T) {
	credErr := errors.New("credential error")
	adapter := &geminiKeyResolverAdapter{
		providers: &stubProviderStore2{row: provtarget.ProviderRow{
			ID: "prov-1", BaseURL: "https://api.example.com",
		}},
		creds: &stubCredentialStore{err: credErr},
	}
	_, _, err := adapter.Resolve(context.Background(), "prov-1", "")
	if err == nil {
		t.Fatal("expected error when credential lookup fails")
	}
}

// TestGeminiKeyResolverAdapter_Resolve_success verifies apiKey and baseURL
// are returned on the happy path.
func TestGeminiKeyResolverAdapter_Resolve_success(t *testing.T) {
	adapter := &geminiKeyResolverAdapter{
		providers: &stubProviderStore2{row: provtarget.ProviderRow{
			ID: "prov-1", BaseURL: "https://api.example.com",
		}},
		creds: &stubCredentialStore{apiKey: "my-api-key"},
	}
	apiKey, baseURL, err := adapter.Resolve(context.Background(), "prov-1", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if apiKey != "my-api-key" {
		t.Errorf("expected apiKey=my-api-key, got %q", apiKey)
	}
	if baseURL != "https://api.example.com" {
		t.Errorf("expected baseURL=https://api.example.com, got %q", baseURL)
	}
}
