// Package core — routing_core_gap_test.go covers FormatTargetFriendly,
// FormatTargetPath, MatchGlob, ModelMatchesAllowedRefs,
// NewSmartStoreDB/ListEnabledChatModels, and NoCompatibleProviderError.
//
// Named failure modes:
//   - nil RoutingTarget → safe placeholders (no panic)
//   - empty ProviderName/ModelCode → ? substituted
//   - MatchGlob: exact match, wildcard, no-wildcard mismatch
//   - MatchGlob: too-long pattern (>200 chars) → false (no panic)
//   - ModelMatchesAllowedRefs: empty refs → unrestricted
//   - ModelMatchesAllowedRefs: wrong provider → skip
//   - ListEnabledChatModels: non-chat models filtered out
//   - ListEnabledChatModels: disabled providers filtered out
//   - ListEnabledChatModels: DB list error propagated
package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func TestFormatTargetFriendly_nilTarget_safeString(t *testing.T) {
	got := FormatTargetFriendly(nil)
	if got != "?/? (\"?\")" {
		t.Errorf("nil: got %q", got)
	}
}

func TestFormatTargetFriendly_emptyFields_questionMarks(t *testing.T) {
	got := FormatTargetFriendly(&RoutingTarget{})
	if !strings.Contains(got, "?") {
		t.Errorf("empty target: expected ? placeholders, got %q", got)
	}
}

func TestFormatTargetFriendly_populated_formattedCorrectly(t *testing.T) {
	got := FormatTargetFriendly(&RoutingTarget{
		ProviderName: "openai",
		ModelCode:    "gpt-5",
		ModelName:    "GPT-5",
	})
	want := `openai/gpt-5 ("GPT-5")`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatTargetFriendly_partiallyEmpty_mixedPlaceholders(t *testing.T) {
	got := FormatTargetFriendly(&RoutingTarget{ProviderName: "anthropic"})
	if !strings.HasPrefix(got, "anthropic/?") {
		t.Errorf("partial: got %q", got)
	}
}

func TestFormatTargetPath_nilTarget_safeString(t *testing.T) {
	got := FormatTargetPath(nil)
	if got != "?/?" {
		t.Errorf("nil: got %q", got)
	}
}

func TestFormatTargetPath_emptyFields_questionMarks(t *testing.T) {
	got := FormatTargetPath(&RoutingTarget{})
	if got != "?/?" {
		t.Errorf("empty: got %q", got)
	}
}

func TestFormatTargetPath_populated(t *testing.T) {
	got := FormatTargetPath(&RoutingTarget{ProviderName: "gemini", ModelCode: "gemini-2.5-pro"})
	if got != "gemini/gemini-2.5-pro" {
		t.Errorf("got %q", got)
	}
}

func TestModelMatchesAllowedRefs_emptyRefs_unrestricted(t *testing.T) {
	if !ModelMatchesAllowedRefs("model-id", "provider-model", "prov-id", nil) {
		t.Error("empty refs should return true (unrestricted)")
	}
}

func TestModelMatchesAllowedRefs_matchByModelID(t *testing.T) {
	refs := []store.AllowedModelRef{
		{ProviderID: "prov-1", ModelID: "model-abc"},
	}
	if !ModelMatchesAllowedRefs("model-abc", "external-model", "prov-1", refs) {
		t.Error("should match by modelID")
	}
}

func TestModelMatchesAllowedRefs_matchByProviderModelID(t *testing.T) {
	refs := []store.AllowedModelRef{
		{ProviderID: "prov-1", ModelID: "external-model"},
	}
	if !ModelMatchesAllowedRefs("model-uuid", "external-model", "prov-1", refs) {
		t.Error("should match by providerModelID")
	}
}

func TestModelMatchesAllowedRefs_wrongProvider_noMatch(t *testing.T) {
	refs := []store.AllowedModelRef{
		{ProviderID: "prov-different", ModelID: "model-abc"},
	}
	if ModelMatchesAllowedRefs("model-abc", "model-abc", "prov-1", refs) {
		t.Error("wrong providerID should not match")
	}
}

// A pattern is NOT a wildcard here — it is a literal that matches nothing.
//
// This test asserted the opposite. Globbing an allowed-model ref makes the admin
// picker misreport the key: the picker writes concrete UUIDs and decides a
// checkbox by exact equality, so a `gpt-*` ref (writable only via the API)
// matches no box, renders as "0 model(s) selected", and silently permits every
// gpt model. Showing less access than a key has is the dangerous direction.
func TestModelMatchesAllowedRefs_patternIsLiteralNotWildcard(t *testing.T) {
	refs := []store.AllowedModelRef{
		{ProviderID: "prov-1", ModelID: "gpt-*"},
	}
	if ModelMatchesAllowedRefs("gpt-5", "gpt-5", "prov-1", refs) {
		t.Error("a '*' in an allowed-model ref must not act as a wildcard — the picker cannot render one, " +
			"so the key would permit more than the UI shows")
	}
	if !ModelMatchesAllowedRefs("gpt-*", "gpt-*", "prov-1", refs) {
		t.Error("the ref should still match a model literally named 'gpt-*', however unlikely")
	}
}

func TestModelMatchesAllowedRefs_noneMatch_returnsFalse(t *testing.T) {
	refs := []store.AllowedModelRef{
		{ProviderID: "prov-1", ModelID: "gpt-4"},
		{ProviderID: "prov-2", ModelID: "claude-3"},
	}
	if ModelMatchesAllowedRefs("claude-3", "claude-3", "prov-1", refs) {
		t.Error("claude-3 allowed only for prov-2, should not match prov-1")
	}
}

// NewSmartStoreDB / ListEnabledChatModels

// stubSmartCatalog implements SmartCatalog for testing.
type stubSmartCatalog struct {
	models   []store.Model
	provider *store.Provider
	listErr  error
	provErr  error
}

func (s *stubSmartCatalog) ListEnabledModels(_ context.Context) ([]store.Model, error) {
	return s.models, s.listErr
}

func (s *stubSmartCatalog) GetProvider(_ context.Context, _ string) (*store.Provider, error) {
	return s.provider, s.provErr
}

func TestNewSmartStoreDB_listEnabledChatModels_chatModelsOnly(t *testing.T) {
	catalog := &stubSmartCatalog{
		models: []store.Model{
			{ID: "m1", Code: "gpt-5", Name: "GPT-5", Type: "chat", ProviderID: "prov-1"},
			{ID: "m2", Code: "embed-model", Name: "Embed", Type: "embedding", ProviderID: "prov-1"},
		},
		provider: &store.Provider{ID: "prov-1", Name: "openai", Enabled: true},
	}
	ss := NewSmartStoreDB(catalog)
	rows, err := ss.ListEnabledChatModels(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d, want 1", len(rows))
	}
	if rows[0].ModelCode != "gpt-5" {
		t.Errorf("expected gpt-5, got %q", rows[0].ModelCode)
	}
}

func TestNewSmartStoreDB_listEnabledChatModels_disabledProviderFiltered(t *testing.T) {
	catalog := &stubSmartCatalog{
		models: []store.Model{
			{ID: "m1", Code: "gpt-5", Type: "chat", ProviderID: "prov-1"},
		},
		provider: &store.Provider{ID: "prov-1", Enabled: false},
	}
	ss := NewSmartStoreDB(catalog)
	rows, err := ss.ListEnabledChatModels(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("disabled provider: expected 0 rows, got %d", len(rows))
	}
}

func TestNewSmartStoreDB_listEnabledChatModels_listError_propagated(t *testing.T) {
	catalog := &stubSmartCatalog{
		listErr: errors.New("db error"),
	}
	ss := NewSmartStoreDB(catalog)
	_, err := ss.ListEnabledChatModels(context.Background())
	if err == nil {
		t.Error("expected error from list failure")
	}
	if !strings.Contains(err.Error(), "db error") {
		t.Errorf("error text: got %q", err.Error())
	}
}

func TestNewSmartStoreDB_listEnabledChatModels_providerLookupError_rowSkipped(t *testing.T) {
	catalog := &stubSmartCatalog{
		models: []store.Model{
			{ID: "m1", Code: "gpt-5", Type: "chat", ProviderID: "prov-missing"},
		},
		provErr: errors.New("provider not found"),
	}
	ss := NewSmartStoreDB(catalog)
	rows, err := ss.ListEnabledChatModels(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Row is skipped when provider lookup fails.
	if len(rows) != 0 {
		t.Errorf("expected 0 rows when provider lookup fails, got %d", len(rows))
	}
}

func TestNewSmartStoreDB_listEnabledChatModels_multipleModels_providerCached(t *testing.T) {
	// Two models for same provider — GetProvider is called once (cached).
	providerCallCount := 0
	catalog := &countingSmartCatalog{
		models: []store.Model{
			{ID: "m1", Code: "gpt-5", Type: "chat", ProviderID: "prov-1"},
			{ID: "m2", Code: "gpt-4o", Type: "chat", ProviderID: "prov-1"},
		},
		provider:          &store.Provider{ID: "prov-1", Name: "openai", Enabled: true},
		providerCallCount: &providerCallCount,
	}
	ss := NewSmartStoreDB(catalog)
	rows, err := ss.ListEnabledChatModels(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("rows: got %d, want 2", len(rows))
	}
	if providerCallCount != 1 {
		t.Errorf("GetProvider called %d times, want 1 (caching)", providerCallCount)
	}
}

// countingSmartCatalog counts GetProvider calls to verify caching.
type countingSmartCatalog struct {
	models            []store.Model
	provider          *store.Provider
	providerCallCount *int
}

func (c *countingSmartCatalog) ListEnabledModels(_ context.Context) ([]store.Model, error) {
	return c.models, nil
}

func (c *countingSmartCatalog) GetProvider(_ context.Context, _ string) (*store.Provider, error) {
	*c.providerCallCount++
	return c.provider, nil
}

// TestNoCompatibleProviderError_ErrorString verifies the sentinel error message.
func TestNoCompatibleProviderError_ErrorString(t *testing.T) {
	e := &NoCompatibleProviderError{Available: []CandidateCapability{
		{Provider: "openai", Model: "ada-002"},
	}}
	if e.Error() != "no_compatible_provider" {
		t.Errorf("Error() = %q, want %q", e.Error(), "no_compatible_provider")
	}
}

// TestNoCompatibleProviderError_EmptyAvailable verifies the error string with nil Available.
func TestNoCompatibleProviderError_EmptyAvailable(t *testing.T) {
	e := &NoCompatibleProviderError{}
	if e.Error() != "no_compatible_provider" {
		t.Errorf("Error() = %q, want %q", e.Error(), "no_compatible_provider")
	}
}

// TestListEnabledCandidates_TheKindDecidesTheCandidateSet.
//
// `model=auto` on an image endpoint must not draw from the chat pool. If it
// does, the router is handed models that cannot produce an image and picks one
// on the strength of its description; the request fails at the provider with a
// wire error about an unsupported field, and the trace shows a deliberate
// selection — the one shape of failure that reads as correct behaviour.
//
// The mirror matters just as much: a chat request must not be offered an
// image-only model, which is why both directions are asserted against the same
// catalogue rather than one direction against a convenient one.
func TestListEnabledCandidates_TheKindDecidesTheCandidateSet(t *testing.T) {
	catalog := &stubSmartCatalog{
		models: []store.Model{
			{ID: "m1", Code: "gpt-5", Name: "GPT-5", Type: "chat", ProviderID: "prov-1"},
			{ID: "m2", Code: "dall-e-3", Name: "DALL-E 3", Type: "image", ProviderID: "prov-1"},
		},
		provider: &store.Provider{ID: "prov-1", Name: "openai", Enabled: true},
	}
	ss := NewSmartStoreDB(catalog)

	for _, tc := range []struct {
		kind typology.EndpointKind
		want string
	}{
		{typology.EndpointKindImageGeneration, "dall-e-3"},
		{typology.EndpointKindChat, "gpt-5"},
	} {
		rows, err := ss.ListEnabledCandidates(context.Background(), tc.kind)
		if err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		if len(rows) != 1 || rows[0].ModelCode != tc.want {
			got := make([]string, len(rows))
			for i, r := range rows {
				got[i] = r.ModelCode
			}
			t.Errorf("%s candidates = %v, want exactly [%s] — the router would be offered a "+
				"model that cannot serve this endpoint, choose it on its description, and the "+
				"failure would arrive from the provider looking like a correct decision",
				tc.kind, got, tc.want)
		}
	}
}

// TestNormalizeModalities_LowersOnlyWhenARowActuallyNeedsIt.
//
// Routing compares modalities exactly, so the catalogue's values are lowered
// once at load. A row already lower-case must come back as the SAME slice: this
// runs per catalogue row on every reload, and allocating a copy for rows that
// need no change was the measured cost the single-pass shape exists to avoid.
func TestNormalizeModalities_LowersOnlyWhenARowActuallyNeedsIt(t *testing.T) {
	already := []string{"text", "image"}
	if got := NormalizeModalities(already); &got[0] != &already[0] {
		t.Error("an already-lower-case row was copied; the no-change path must return the input")
	}

	mixed := []string{"text", "Image", "AUDIO"}
	got := NormalizeModalities(mixed)
	want := []string{"text", "image", "audio"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalized = %v, want %v — a mixed-case row leaves routing comparing "+
				"%q against %q and finding no match", got, want, mixed[i], want[i])
		}
	}
	if mixed[1] != "Image" {
		t.Error("the input slice was mutated; /v1/models publishes these values verbatim")
	}
}
