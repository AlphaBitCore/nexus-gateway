package provtarget

import (
	"context"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
)

type fakeProviderStore struct{ row ProviderRow }

func (f *fakeProviderStore) GetProviderByID(_ context.Context, _ string) (ProviderRow, error) {
	return f.row, nil
}

type fakeModelStore struct{ row ModelRow }

func (f *fakeModelStore) GetModelByID(_ context.Context, _ string) (ModelRow, error) {
	return f.row, nil
}

type fakeCredentialStore struct{ key string }

func (f *fakeCredentialStore) ResolveForProvider(_ context.Context, _ string, _ string) (string, string, string, error) {
	return f.key, "c-1", "default", nil
}

func (f *fakeCredentialStore) ListForProvider(_ context.Context, _ string) ([]CredentialCandidate, error) {
	return []CredentialCandidate{{ID: "c-1", Name: "default", Weight: 100}}, nil
}

func TestPgResolver_CopiesAdapterTypeIntoFormat(t *testing.T) {
	r := NewPgResolver(
		&fakeProviderStore{row: ProviderRow{ID: "p-1", Name: "openai", AdapterType: "openai", BaseURL: "https://api.openai.com"}},
		&fakeModelStore{row: ModelRow{ID: "m-1", ProviderID: "p-1", ProviderModelID: "gpt-4o-mini"}},
		&fakeCredentialStore{key: "sk-x"},
	)
	target, err := r.Resolve(context.Background(), "p-1", "m-1", ResolveHints{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.Format != provcore.FormatOpenAI {
		t.Fatalf("Format = %q, want openai", target.Format)
	}
	if target.ProviderName != "openai" {
		t.Fatalf("ProviderName = %q", target.ProviderName)
	}
	if target.APIKey != "sk-x" {
		t.Fatalf("APIKey lost: %q", target.APIKey)
	}
}

func TestPgResolver_EmptyAdapterTypeIsError(t *testing.T) {
	r := NewPgResolver(
		&fakeProviderStore{row: ProviderRow{ID: "p-1", Name: "custom", AdapterType: "", BaseURL: "https://example.com"}},
		&fakeModelStore{row: ModelRow{ID: "m-1", ProviderID: "p-1", ProviderModelID: "x"}},
		&fakeCredentialStore{key: "sk"},
	)
	_, err := r.Resolve(context.Background(), "p-1", "m-1", ResolveHints{})
	if err == nil {
		t.Fatal("expected error for empty adapter_type")
	}
	if !strings.Contains(err.Error(), "adapter_type") {
		t.Fatalf("error %q should mention adapter_type", err)
	}
}
