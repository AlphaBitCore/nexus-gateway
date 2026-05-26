package debug

// semantic_prewarm_endpoint_test.go — unit tests for SemanticPrewarmHandler
// (FAQ pre-warm L2 from corpus).
//
// Uses package debug (white-box) to match existing test conventions.
// The writer=nil path tests the 503 code. Writer-present tests use a
// real semantic.Writer whose ConfigCache has Enabled=false so no Redis
// or embedding provider is required.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
)

// prewarmDiscardLogger returns a slog.Logger that discards all output.
func prewarmDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// buildValidPrewarmBody builds a valid prewarm request body with n entries.
func buildValidPrewarmBody(n int, dryRun bool) []byte {
	entries := make([]map[string]any, n)
	for i := range entries {
		entries[i] = map[string]any{
			"prompt":     "What is Go?",
			"response":   "Go is a compiled language.",
			"model":      "gpt-4o",
			"vkScope":    "",
			"ttlSeconds": 3600,
		}
	}
	body, _ := json.Marshal(map[string]any{
		"entries": entries,
		"dryRun":  dryRun,
	})
	return body
}

// buildDisabledSemanticWriter builds a real semantic.Writer whose ConfigCache
// has Enabled=false so EffectiveEnabled()=false. Calling Write() returns
// Skipped=true immediately — no Redis or embedding provider needed.
// Passes nil metrics so the test does not fight over prometheus.DefaultRegisterer
// with other tests in the same binary.
func buildDisabledSemanticWriter() *semantic.Writer {
	cfg := semantic.NewConfigCache()
	cfg.Set(semantic.ConfigSnapshot{Enabled: false})
	// nil client + nil singleflight + nil metrics are safe when EffectiveEnabled()=false.
	return semantic.NewWriter(cfg, nil, nil, prewarmDiscardLogger(), 0, nil)
}

// doPrewarm fires the handler directly via httptest.ResponseRecorder.
func doPrewarm(h http.HandlerFunc, bodyJSON string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/semantic-prewarm",
		strings.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec
}

// nil writer (L2 disabled)

// TestSemanticPrewarm_NilWriter_503 locks the nil-writer guard: 503 with
// code="semantic_cache_disabled".
func TestSemanticPrewarm_NilWriter_503(t *testing.T) {
	h := SemanticPrewarmHandler(nil, nil, nil, prewarmDiscardLogger())
	rec := doPrewarm(h, string(buildValidPrewarmBody(1, false)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d body=%s", rec.Code, rec.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["code"] != "semantic_cache_disabled" {
		t.Errorf("code: want semantic_cache_disabled, got %v", result["code"])
	}
}

// TestSemanticPrewarm_DryRun_NilWriter_503 confirms nil writer → 503 even
// when dryRun=true (writer check precedes dryRun branch).
func TestSemanticPrewarm_DryRun_NilWriter_503(t *testing.T) {
	h := SemanticPrewarmHandler(nil, nil, nil, prewarmDiscardLogger())
	rec := doPrewarm(h, string(buildValidPrewarmBody(1, true)))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// validation (writer-independent)

// TestSemanticPrewarm_EmptyEntries_400 locks the empty-entries guard.
func TestSemanticPrewarm_EmptyEntries_400(t *testing.T) {
	h := SemanticPrewarmHandler(nil, nil, nil, prewarmDiscardLogger())
	rec := doPrewarm(h, `{"entries":[],"dryRun":false}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "empty") {
		t.Errorf("body missing 'empty': %s", rec.Body.String())
	}
}

// TestSemanticPrewarm_TooManyEntries_413 locks the 500-entry cap: 501→413.
func TestSemanticPrewarm_TooManyEntries_413(t *testing.T) {
	h := SemanticPrewarmHandler(nil, nil, nil, prewarmDiscardLogger())
	rec := doPrewarm(h, string(buildValidPrewarmBody(501, false)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestSemanticPrewarm_MalformedJSON_400 locks the bad-JSON guard.
func TestSemanticPrewarm_MalformedJSON_400(t *testing.T) {
	h := SemanticPrewarmHandler(nil, nil, nil, prewarmDiscardLogger())
	rec := doPrewarm(h, `{not json`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// writer present, L2 disabled

// TestSemanticPrewarm_WriterPresent_L2Disabled_SkipsAll locks the path where
// Writer is wired but ConfigCache has Enabled=false: every entry is skipped.
func TestSemanticPrewarm_WriterPresent_L2Disabled_SkipsAll(t *testing.T) {
	writer := buildDisabledSemanticWriter()
	h := SemanticPrewarmHandler(writer, nil, nil, prewarmDiscardLogger())

	const n = 3
	rec := doPrewarm(h, string(buildValidPrewarmBody(n, false)))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var result SemanticPrewarmResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Written != 0 {
		t.Errorf("written: want 0, got %d", result.Written)
	}
	if result.Skipped != n {
		t.Errorf("skipped: want %d, got %d", n, result.Skipped)
	}
	if result.Errors != 0 {
		t.Errorf("errors: want 0, got %d", result.Errors)
	}
	if len(result.Entries) != n {
		t.Fatalf("entries len: want %d, got %d", n, len(result.Entries))
	}
	for i, e := range result.Entries {
		if !e.Skipped {
			t.Errorf("entries[%d].Skipped: want true", i)
		}
	}
}

// TestSemanticPrewarm_DryRun_SkipsAll locks the dryRun path: entries are
// returned as skipped with SkipReason="dry_run".
func TestSemanticPrewarm_DryRun_SkipsAll(t *testing.T) {
	writer := buildDisabledSemanticWriter()
	h := SemanticPrewarmHandler(writer, nil, nil, prewarmDiscardLogger())

	const n = 2
	rec := doPrewarm(h, string(buildValidPrewarmBody(n, true)))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var result SemanticPrewarmResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Written != 0 {
		t.Errorf("written: want 0, got %d", result.Written)
	}
	if result.Skipped != n {
		t.Errorf("skipped: want %d, got %d", n, result.Skipped)
	}
	if !result.DryRun {
		t.Error("dryRun: want true")
	}
	for i, e := range result.Entries {
		if e.SkipReason != "dry_run" {
			t.Errorf("entries[%d].SkipReason: want dry_run, got %q", i, e.SkipReason)
		}
	}
}

// TestSemanticPrewarm_DurationMs_NonNegative locks that DurationMs ≥0.
func TestSemanticPrewarm_DurationMs_NonNegative(t *testing.T) {
	writer := buildDisabledSemanticWriter()
	h := SemanticPrewarmHandler(writer, nil, nil, prewarmDiscardLogger())
	rec := doPrewarm(h, string(buildValidPrewarmBody(1, false)))

	var result SemanticPrewarmResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.DurationMs < 0 {
		t.Errorf("DurationMs: want ≥0, got %d", result.DurationMs)
	}
}

// TestSemanticPrewarm_EntryIndex_Matches locks that each result entry carries
// the correct zero-based index.
func TestSemanticPrewarm_EntryIndex_Matches(t *testing.T) {
	writer := buildDisabledSemanticWriter()
	h := SemanticPrewarmHandler(writer, nil, nil, prewarmDiscardLogger())

	const n = 4
	rec := doPrewarm(h, string(buildValidPrewarmBody(n, false)))

	var result SemanticPrewarmResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result.Entries) != n {
		t.Fatalf("entries len: want %d, got %d", n, len(result.Entries))
	}
	for i, e := range result.Entries {
		if e.Index != i {
			t.Errorf("entries[%d].Index: want %d, got %d", i, i, e.Index)
		}
	}
}

// TestSemanticPrewarm_ResponseShape_MatchesDoc locks the response JSON field
// names by checking specific keys are present.
func TestSemanticPrewarm_ResponseShape_MatchesDoc(t *testing.T) {
	writer := buildDisabledSemanticWriter()
	h := SemanticPrewarmHandler(writer, nil, nil, prewarmDiscardLogger())
	rec := doPrewarm(h, string(buildValidPrewarmBody(1, false)))

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"written", "skipped", "errors", "embeddingCalls", "durationMs", "dryRun", "entries"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("response missing key %q", key)
		}
	}
}

// TestSemanticPrewarm_SingleEntry_ValidTTL locks that an entry with
// ttlSeconds=3600 is processed without error.
func TestSemanticPrewarm_SingleEntry_ValidTTL(t *testing.T) {
	writer := buildDisabledSemanticWriter()
	h := SemanticPrewarmHandler(writer, nil, nil, prewarmDiscardLogger())
	body := `{"entries":[{"prompt":"Q","response":"A","model":"gpt-4o","ttlSeconds":3600}],"dryRun":false}`
	rec := doPrewarm(h, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestSemanticPrewarm_500Entries_AtCap locks that exactly 500 entries is
// accepted (cap is exclusive on the upper bound for too-many).
func TestSemanticPrewarm_500Entries_AtCap(t *testing.T) {
	// writer nil — the check for too-many-entries happens before writer check.
	h := SemanticPrewarmHandler(nil, nil, nil, prewarmDiscardLogger())
	rec := doPrewarm(h, string(buildValidPrewarmBody(500, false)))

	// 500 entries → nil writer → 503 (not 413); proves cap is at 501.
	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("500 entries should not return 413 (cap is 501+)")
	}
}

// TestSemanticPrewarm_jsonStringEscape_RoundTrip locks that the internal
// helper correctly escapes a string for inline JSON embedding.
func TestSemanticPrewarm_jsonStringEscape_RoundTrip(t *testing.T) {
	inputs := []string{
		"hello",
		`has "quotes"`,
		"has\nnewline",
		"has\\backslash",
		"Unicode: 你好",
	}
	for _, s := range inputs {
		escaped := jsonStringEscape(s)
		// The escaped form must be a valid JSON string literal.
		var decoded string
		if err := json.Unmarshal([]byte(escaped), &decoded); err != nil {
			t.Errorf("jsonStringEscape(%q) → %s is not valid JSON string: %v", s, escaped, err)
			continue
		}
		if decoded != s {
			t.Errorf("jsonStringEscape(%q) round-trip: got %q", s, decoded)
		}
	}
}

// stub writer for Write-path coverage

// stubWriterOK is a semanticWriterIface that always returns Stored=true.
type stubWriterOK struct{}

func (s *stubWriterOK) Write(_ context.Context, _ semantic.WriteRequest) (semantic.WriteResult, error) {
	return semantic.WriteResult{
		Stored:           true,
		EmbeddingCostUSD: 0.0001,
	}, nil
}

// stubWriterErr is a semanticWriterIface that always returns an error.
type stubWriterErr struct{}

func (s *stubWriterErr) Write(_ context.Context, _ semantic.WriteRequest) (semantic.WriteResult, error) {
	return semantic.WriteResult{}, errors.New("embedding provider unavailable")
}

// stubSnap implements configSnapshotGetter with a fixed snapshot.
type stubSnap struct {
	snap semantic.ConfigSnapshot
}

func (s *stubSnap) Get() semantic.ConfigSnapshot { return s.snap }

// stubCreds implements credResolverIface with a fixed key + optional error.
type stubCreds struct {
	key string
	err error
}

func (s *stubCreds) GetForProvider(_ context.Context, _ string) (string, string, string, error) {
	if s.err != nil {
		return "", "", "", s.err
	}
	return s.key, "cred-id", "cred-name", nil
}

// resolvableDeps returns a (snap, creds) pair where credential resolution
// succeeds — used by Write-path tests that need to bypass the skip-reason
// short-circuit and reach the underlying writer stub.
func resolvableDeps() (*stubSnap, *stubCreds) {
	return &stubSnap{snap: semantic.ConfigSnapshot{
			EmbeddingProviderID:      "openai",
			EmbeddingProviderBaseURL: "https://api.openai.com",
		}},
		&stubCreds{key: "sk-test"}
}

// Write-path branch coverage via stub writer

// TestSemanticPrewarm_Written_StubWriter locks the wr.Stored=true path:
// when Write returns Stored=true the entry is counted as written.
func TestSemanticPrewarm_Written_StubWriter(t *testing.T) {
	snap, creds := resolvableDeps()
	h := semanticPrewarmHandler(&stubWriterOK{}, snap, creds, prewarmDiscardLogger())
	const n = 2
	rec := doPrewarm(h, string(buildValidPrewarmBody(n, false)))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var result SemanticPrewarmResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Written != n {
		t.Errorf("written: want %d, got %d", n, result.Written)
	}
	if result.Skipped != 0 {
		t.Errorf("skipped: want 0, got %d", result.Skipped)
	}
	if result.Errors != 0 {
		t.Errorf("errors: want 0, got %d", result.Errors)
	}
	if result.EmbeddingCalls != n {
		t.Errorf("embeddingCalls: want %d, got %d", n, result.EmbeddingCalls)
	}
	// Cost should be n × 0.0001.
	const wantCost = float64(n) * 0.0001
	if result.EmbeddingCostUSD < wantCost/2 || result.EmbeddingCostUSD > wantCost*2 {
		t.Errorf("embeddingCostUsd: want ~%f, got %f", wantCost, result.EmbeddingCostUSD)
	}
	for i, e := range result.Entries {
		if !e.Written {
			t.Errorf("entries[%d].Written: want true", i)
		}
	}
}

// TestSemanticPrewarm_Error_StubWriter locks the write-error path:
// when Write returns an error the entry is counted as an error.
func TestSemanticPrewarm_Error_StubWriter(t *testing.T) {
	snap, creds := resolvableDeps()
	h := semanticPrewarmHandler(&stubWriterErr{}, snap, creds, prewarmDiscardLogger())
	const n = 2
	rec := doPrewarm(h, string(buildValidPrewarmBody(n, false)))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var result SemanticPrewarmResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Written != 0 {
		t.Errorf("written: want 0, got %d", result.Written)
	}
	if result.Errors != n {
		t.Errorf("errors: want %d, got %d", n, result.Errors)
	}
	for i, e := range result.Entries {
		if e.Error == "" {
			t.Errorf("entries[%d].Error: want non-empty", i)
		}
	}
}

// resolveEmbeddingCreds branch coverage

// TestSemanticPrewarm_NoConfig_SkipsSemanticUnavailable covers the branch
// where the live ConfigCache is nil — the resolver returns
// "semantic_unavailable" and every non-dryRun entry is stamped accordingly
// without touching the writer.
func TestSemanticPrewarm_NoConfig_SkipsSemanticUnavailable(t *testing.T) {
	h := semanticPrewarmHandler(&stubWriterOK{}, nil, &stubCreds{key: "sk-test"}, prewarmDiscardLogger())
	const n = 3
	rec := doPrewarm(h, string(buildValidPrewarmBody(n, false)))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var result SemanticPrewarmResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Written != 0 || result.Skipped != n || result.Errors != 0 {
		t.Fatalf("counts: want written=0 skipped=%d errors=0; got written=%d skipped=%d errors=%d",
			n, result.Written, result.Skipped, result.Errors)
	}
	for i, e := range result.Entries {
		if e.SkipReason != "semantic_unavailable" {
			t.Errorf("entries[%d].SkipReason: want semantic_unavailable, got %q", i, e.SkipReason)
		}
	}
}

// TestSemanticPrewarm_EmptyProviderInSnapshot_SkipsSemanticUnavailable covers
// the branch where the snapshot exists but EmbeddingProviderID is empty
// (admin has not yet selected an embedding provider).
func TestSemanticPrewarm_EmptyProviderInSnapshot_SkipsSemanticUnavailable(t *testing.T) {
	snap := &stubSnap{snap: semantic.ConfigSnapshot{
		EmbeddingProviderID:      "",
		EmbeddingProviderBaseURL: "",
	}}
	h := semanticPrewarmHandler(&stubWriterOK{}, snap, &stubCreds{key: "sk-test"}, prewarmDiscardLogger())
	rec := doPrewarm(h, string(buildValidPrewarmBody(1, false)))

	var result SemanticPrewarmResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Entries[0].SkipReason != "semantic_unavailable" {
		t.Errorf("SkipReason: want semantic_unavailable, got %q", result.Entries[0].SkipReason)
	}
}

// TestSemanticPrewarm_NilCredManager_SkipsEmbeddingProviderError covers the
// branch where the snapshot is fully populated but CredManager is nil
// (defensive — boot wiring guarantees it; ensures the handler does not
// crash and stamps a structured skipReason).
func TestSemanticPrewarm_NilCredManager_SkipsEmbeddingProviderError(t *testing.T) {
	snap, _ := resolvableDeps()
	h := semanticPrewarmHandler(&stubWriterOK{}, snap, nil, prewarmDiscardLogger())
	const n = 2
	rec := doPrewarm(h, string(buildValidPrewarmBody(n, false)))

	var result SemanticPrewarmResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Written != 0 || result.Skipped != n {
		t.Fatalf("counts: want skipped=%d; got written=%d skipped=%d", n, result.Written, result.Skipped)
	}
	for i, e := range result.Entries {
		if e.SkipReason != "embedding_provider_error" {
			t.Errorf("entries[%d].SkipReason: want embedding_provider_error, got %q", i, e.SkipReason)
		}
	}
}

// TestSemanticPrewarm_CredLookupError_SkipsEmbeddingProviderError covers the
// branch where the snapshot is populated but the credential store returns
// an error (no credential for the embedding provider).
func TestSemanticPrewarm_CredLookupError_SkipsEmbeddingProviderError(t *testing.T) {
	snap, _ := resolvableDeps()
	creds := &stubCreds{err: errors.New("no credential for provider")}
	h := semanticPrewarmHandler(&stubWriterOK{}, snap, creds, prewarmDiscardLogger())
	rec := doPrewarm(h, string(buildValidPrewarmBody(1, false)))

	var result SemanticPrewarmResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Entries[0].SkipReason != "embedding_provider_error" {
		t.Errorf("SkipReason: want embedding_provider_error, got %q", result.Entries[0].SkipReason)
	}
}

// TestSemanticPrewarm_EmptyDecryptedKey_SkipsEmbeddingProviderError covers
// the branch where decryption succeeds but the plaintext is empty
// (corrupt encrypted row).
func TestSemanticPrewarm_EmptyDecryptedKey_SkipsEmbeddingProviderError(t *testing.T) {
	snap, _ := resolvableDeps()
	creds := &stubCreds{key: ""}
	h := semanticPrewarmHandler(&stubWriterOK{}, snap, creds, prewarmDiscardLogger())
	rec := doPrewarm(h, string(buildValidPrewarmBody(1, false)))

	var result SemanticPrewarmResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Entries[0].SkipReason != "embedding_provider_error" {
		t.Errorf("SkipReason: want embedding_provider_error, got %q", result.Entries[0].SkipReason)
	}
}

// TestSemanticPrewarm_WriteRequestCarriesResolvedCreds verifies that the
// resolved baseURL + apiKey actually reach the writer's WriteRequest.
// Uses a capture stub to inspect the per-entry WriteRequest.
type captureWriter struct {
	got []semantic.WriteRequest
}

func (c *captureWriter) Write(_ context.Context, req semantic.WriteRequest) (semantic.WriteResult, error) {
	c.got = append(c.got, req)
	return semantic.WriteResult{Stored: true}, nil
}

func TestSemanticPrewarm_WriteRequestCarriesResolvedCreds(t *testing.T) {
	snap := &stubSnap{snap: semantic.ConfigSnapshot{
		EmbeddingProviderID:      "openai",
		EmbeddingProviderBaseURL: "https://api.openai.com",
	}}
	creds := &stubCreds{key: "sk-resolved-via-credmanager"}
	cw := &captureWriter{}
	h := semanticPrewarmHandler(cw, snap, creds, prewarmDiscardLogger())
	rec := doPrewarm(h, string(buildValidPrewarmBody(2, false)))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(cw.got) != 2 {
		t.Fatalf("writer called %d times, want 2", len(cw.got))
	}
	for i, req := range cw.got {
		if req.ProviderBaseURL != "https://api.openai.com" {
			t.Errorf("entry %d ProviderBaseURL=%q; want %q (must come from snapshot, not request body)",
				i, req.ProviderBaseURL, "https://api.openai.com")
		}
		if req.EmbeddingAPIKey != "sk-resolved-via-credmanager" {
			t.Errorf("entry %d EmbeddingAPIKey=%q; want sk-resolved-via-credmanager (must come from CredManager)",
				i, req.EmbeddingAPIKey)
		}
	}
}
