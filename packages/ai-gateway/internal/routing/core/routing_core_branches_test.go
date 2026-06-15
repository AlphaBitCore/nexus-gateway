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

func TestMatchGlob_wildcardStar_matchesAll(t *testing.T) {
	if !MatchGlob("*", "anything") {
		t.Error("* should match anything")
	}
	if !MatchGlob("*", "") {
		t.Error("* should match empty string")
	}
}

func TestMatchGlob_exactMatch_noWildcard(t *testing.T) {
	if !MatchGlob("gpt-5", "gpt-5") {
		t.Error("exact match should return true")
	}
	if MatchGlob("gpt-5", "gpt-4o") {
		t.Error("exact non-match should return false")
	}
}

func TestMatchGlob_suffixWildcard(t *testing.T) {
	if !MatchGlob("gpt-*", "gpt-5") {
		t.Error("prefix glob should match")
	}
	if !MatchGlob("gpt-*", "gpt-4o-mini") {
		t.Error("prefix glob should match longer string")
	}
	if MatchGlob("gpt-*", "claude-sonnet") {
		t.Error("prefix glob should not match different prefix")
	}
}

func TestMatchGlob_prefixWildcard(t *testing.T) {
	if !MatchGlob("*-mini", "gpt-4o-mini") {
		t.Error("suffix glob should match")
	}
	if MatchGlob("*-mini", "gpt-4o") {
		t.Error("suffix glob should not match non-suffix")
	}
}

func TestMatchGlob_tooLongPattern_returnsFalse(t *testing.T) {
	// Pattern longer than maxRegexLen (200) with a wildcard → getCachedGlobRegex returns nil → false.
	longPattern := strings.Repeat("a*", 110) // 220 chars, exceeds 200
	if MatchGlob(longPattern, "aaa") {
		t.Error("too-long pattern should return false")
	}
}

func TestMatchGlob_cachedPattern_secondCallUsesCache(t *testing.T) {
	// Call twice with same pattern to exercise the cache hit path.
	if !MatchGlob("cached-*", "cached-value") {
		t.Error("first call: expected true")
	}
	if !MatchGlob("cached-*", "cached-other") {
		t.Error("second call (cached): expected true")
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

func TestModelMatchesAllowedRefs_globPattern_matches(t *testing.T) {
	refs := []store.AllowedModelRef{
		{ProviderID: "prov-1", ModelID: "gpt-*"},
	}
	if !ModelMatchesAllowedRefs("gpt-5", "gpt-5", "prov-1", refs) {
		t.Error("glob pattern should match")
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

// getCachedGlobRegex: invalid regex path

// TestMatchGlob_invalidRegex — a glob pattern that produces an invalid regex
// (regexp.Compile error) should return false without panicking.
func TestMatchGlob_invalidRegex(t *testing.T) {
	// Build a pattern that, after QuoteMeta + replacing \* → .*, produces an
	// invalid regex. regexp.QuoteMeta escapes all meta chars, so we need a
	// pattern that contains a valid-looking glob but after transformation yields
	// bad regex. The easiest approach: force the regex compile to fail via a
	// pattern whose escaped form exceeds nothing (regexp.Compile never fails on
	// escaped input), so instead directly call getCachedGlobRegex with a bad
	// pattern (not via MatchGlob, since MatchGlob only calls it for wildcard patterns).
	// getCachedGlobRegex is package-internal, so we call it here.
	result := getCachedGlobRegex("(invalid[") // malformed regex
	if result != nil {
		t.Error("expected nil for invalid regex pattern")
	}
}
