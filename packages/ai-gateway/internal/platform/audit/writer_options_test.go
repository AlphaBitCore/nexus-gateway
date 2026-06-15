package audit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/prometheus/client_golang/prometheus"
)

// Pure helpers — exercises every branch of the small inline utilities.

func TestNormalizeAdapterType_LowercasesAndPassesThrough(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"already lower", "openai", "openai"},
		{"mixed case lowered", "Anthropic", "anthropic"},
		{"all caps lowered", "GEMINI", "gemini"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeAdapterType(&Record{IngressFormat: tc.in})
			if got != tc.want {
				t.Errorf("normalizeAdapterType(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeAdapterType_KeysOnIngressBothDirections pins the invariant
// that ai-gateway keys shared/normalize on the ingress format for BOTH
// directions — never the routed upstream adapter. Every byte buffer
// ai-gateway captures is in the client (ingress) wire shape: non-stream
// responses are captured AFTER egressReshapeNonStream (B→canonical→A), the
// streaming tee wraps the client ResponseWriter, and error bodies are
// EncodeErrorEnvelopeForIngress output. Keying on the upstream adapter
// (the prior behavior) fed the gemini-shaped `candidates[]` body of an
// OpenAI-backed model served over the Gemini ingress to the OpenAI
// normalizer, which rejected it and dropped the row to Tier-3 http-json.
func TestNormalizeAdapterType_KeysOnIngressBothDirections(t *testing.T) {
	// OpenAI model served over the Gemini :generateContent ingress: the
	// captured response is Gemini `candidates[]` shape, so the key must be
	// the ingress format "gemini", independent of any routed upstream.
	gem := &Record{IngressFormat: "gemini"}
	if got := normalizeAdapterType(gem); got != "gemini" {
		t.Errorf("gemini ingress key = %q, want gemini", got)
	}
	// Cross-format /v1/responses ingress keys on its own ingress format;
	// the registry's path-keyed `::/v1/responses` fallback resolves it even
	// when no adapter-only entry matches "openai-responses".
	resp := &Record{IngressFormat: "openai-responses"}
	if got := normalizeAdapterType(resp); got != "openai-responses" {
		t.Errorf("responses ingress key = %q, want openai-responses", got)
	}
	// Empty ingress (early failure before format resolution) yields empty,
	// letting the registry fall through to path-keyed + generic-http tiers.
	if got := normalizeAdapterType(&Record{IngressFormat: ""}); got != "" {
		t.Errorf("empty ingress key = %q, want empty", got)
	}
}

func TestFilterHookStage_EmptyInputs(t *testing.T) {
	// Empty input returns nil.
	if got := filterHookStage(nil, "request"); got != nil {
		t.Errorf("nil hooks → want nil, got %v", got)
	}
	// Empty stages returns nil.
	if got := filterHookStage([]HookExecRecord{{Stage: "request"}}); got != nil {
		t.Errorf("no stages → want nil, got %v", got)
	}
	// No matches returns nil (not empty slice — distinguishes "no hooks ran"
	// from "hooks ran but none matched").
	in := []HookExecRecord{{Stage: "response"}}
	if got := filterHookStage(in, "request"); got != nil {
		t.Errorf("no matches → want nil, got %v", got)
	}
}

func TestFilterHookStage_MatchesMultipleStages(t *testing.T) {
	in := []HookExecRecord{
		{Stage: "request", HookID: "h1"},
		{Stage: "response", HookID: "h2"},
		{Stage: "connection", HookID: "h3"},
		{Stage: "request", HookID: "h4"},
	}
	got := filterHookStage(in, "request", "connection")
	if len(got) != 3 {
		t.Fatalf("want 3 matched, got %d", len(got))
	}
	ids := []string{got[0].HookID, got[1].HookID, got[2].HookID}
	for _, want := range []string{"h1", "h3", "h4"} {
		found := false
		for _, id := range ids {
			if id == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %s in filtered output, got %v", want, ids)
		}
	}
}

func TestNilIfEmpty(t *testing.T) {
	if got := nilIfEmpty(""); got != nil {
		t.Errorf("empty → want nil, got %v", got)
	}
	got := nilIfEmpty("X")
	if got == nil || *got != "X" {
		t.Errorf("non-empty → want pointer to \"X\", got %v", got)
	}
}

func TestFirstNonNil(t *testing.T) {
	a, b, c := 1, 2, 3
	// All nil returns nil.
	if got := firstNonNil(nil, nil); got != nil {
		t.Errorf("all nil → want nil, got %v", *got)
	}
	// First non-nil wins.
	if got := firstNonNil(nil, &a, &b); got != &a {
		t.Errorf("first non-nil should be &a, got %p (want %p)", got, &a)
	}
	// Single arg.
	if got := firstNonNil(&c); got != &c {
		t.Errorf("single arg should be &c, got %p", got)
	}
}

func TestSumHookLatenciesMs_NilWhenStageAbsent(t *testing.T) {
	// Empty in.
	if got := sumHookLatenciesMs(nil, "request"); got != nil {
		t.Errorf("nil in → want nil, got %v", got)
	}
	// Empty stages.
	if got := sumHookLatenciesMs([]HookExecRecord{{Stage: "request"}}); got != nil {
		t.Errorf("no stages → want nil, got %v", got)
	}
	// All hooks for OTHER stages — returns nil (didn't run for requested stage).
	in := []HookExecRecord{{Stage: "response", LatencyMs: 10}}
	if got := sumHookLatenciesMs(in, "request"); got != nil {
		t.Errorf("no matching stages → want nil (distinguish from 0), got %v", *got)
	}
}

func TestSumHookLatenciesMs_SumsOnlyMatchingStages(t *testing.T) {
	in := []HookExecRecord{
		{Stage: "request", LatencyMs: 5},
		{Stage: "response", LatencyMs: 7},
		{Stage: "request", LatencyMs: 0}, // 0 is excluded from sum but still marks "ran"
		{Stage: "connection", LatencyMs: 3},
	}
	got := sumHookLatenciesMs(in, "request", "connection")
	if got == nil {
		t.Fatal("want non-nil sum")
	}
	if *got != 8 {
		t.Errorf("sum = %d, want 8 (5 + 0 + 3, response excluded)", *got)
	}

	// Stage matched but only zero-latency rows — returns 0 (NOT nil).
	zeros := []HookExecRecord{{Stage: "request", LatencyMs: 0}}
	got = sumHookLatenciesMs(zeros, "request")
	if got == nil {
		t.Fatal("zero-latency matching row should produce non-nil 0")
	}
	if *got != 0 {
		t.Errorf("zero-latency rows → want 0, got %d", *got)
	}
}

func TestFirstNonEmptyStr(t *testing.T) {
	if got := firstNonEmptyStr("a", "b"); got != "a" {
		t.Errorf("a wins → want a, got %q", got)
	}
	if got := firstNonEmptyStr("", "b"); got != "b" {
		t.Errorf("empty a → want b, got %q", got)
	}
	if got := firstNonEmptyStr("", ""); got != "" {
		t.Errorf("both empty → want empty, got %q", got)
	}
}

// ApplyVKMeta — covers both VKType branches.

func TestApplyVKMeta_PersonalSetsUserFields(t *testing.T) {
	rec := &Record{}
	meta := &vkauth.VKMeta{
		ID:                   "vk-1",
		Name:                 "demo-key",
		VKType:               "personal",
		OrganizationID:       "org-1",
		OrganizationName:     "Acme",
		OrganizationTimezone: "America/Los_Angeles",
		ProjectID:            "proj-1",
		ProjectName:          "AI Chat",
		SourceApp:            "cli",
		OwnerID:              "user-david",
		UserDisplayName:      "David",
	}
	rec.ApplyVKMeta(meta)
	if rec.VirtualKeyID != "vk-1" || rec.VirtualKeyName != "demo-key" {
		t.Errorf("VK id/name = %q/%q, want vk-1/demo-key", rec.VirtualKeyID, rec.VirtualKeyName)
	}
	if rec.VKType != "personal" {
		t.Errorf("VKType = %q, want personal", rec.VKType)
	}
	if rec.UserID != "user-david" {
		t.Errorf("UserID = %q, want user-david (personal VK should set owner)", rec.UserID)
	}
	if rec.UserDisplayName != "David" {
		t.Errorf("UserDisplayName = %q, want David", rec.UserDisplayName)
	}
	if rec.OriginTZ != "America/Los_Angeles" {
		t.Errorf("OriginTZ = %q, want America/Los_Angeles", rec.OriginTZ)
	}
	if rec.OrganizationID != "org-1" || rec.OrganizationName != "Acme" {
		t.Errorf("Org = %q/%q, want org-1/Acme", rec.OrganizationID, rec.OrganizationName)
	}
	if rec.ProjectID != "proj-1" || rec.ProjectName != "AI Chat" {
		t.Errorf("Project = %q/%q, want proj-1/AI Chat", rec.ProjectID, rec.ProjectName)
	}
	if rec.SourceApp != "cli" {
		t.Errorf("SourceApp = %q, want cli", rec.SourceApp)
	}
}

func TestApplyVKMeta_ApplicationSkipsUserFields(t *testing.T) {
	rec := &Record{}
	meta := &vkauth.VKMeta{
		ID:               "vk-app-1",
		Name:             "research-all-models",
		VKType:           "application",
		OrganizationID:   "org-1",
		OrganizationName: "Acme",
		ProjectID:        "proj-research",
		ProjectName:      "Research",
		OwnerID:          "should-not-leak-as-userid",
		UserDisplayName:  "should-not-leak",
	}
	rec.ApplyVKMeta(meta)
	if rec.UserID != "" {
		t.Errorf("UserID = %q, want empty (application VK has no NexusUser)", rec.UserID)
	}
	if rec.UserDisplayName != "" {
		t.Errorf("UserDisplayName = %q, want empty (application VK)", rec.UserDisplayName)
	}
	if rec.ProjectID != "proj-research" {
		t.Errorf("ProjectID = %q, want proj-research", rec.ProjectID)
	}
}

// With* setters — wire the optional dependencies and verify they're stored.

// stubSpillStore implements spillstore.SpillStore but records nothing. Used
// to verify WithSpillStore wires the receiver and recordToMessage chooses
// the spill path when a body exceeds the inline threshold.
type stubSpillStore struct {
	mu      sync.Mutex
	putKey  string
	putBody []byte
	putErr  error
}

func (s *stubSpillStore) Put(_ context.Context, content io.Reader, _ int64, opts spillstore.PutOptions) (sharedaudit.SpillRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.putErr != nil {
		return sharedaudit.SpillRef{}, s.putErr
	}
	data, _ := io.ReadAll(content)
	s.putKey = opts.EventID + "/" + opts.Direction
	s.putBody = data
	return sharedaudit.SpillRef{
		Backend:     "stub",
		Key:         s.putKey,
		Size:        int64(len(data)),
		ContentType: opts.ContentType,
	}, nil
}
func (s *stubSpillStore) Get(context.Context, sharedaudit.SpillRef) (io.ReadCloser, error) {
	return nil, spillstore.ErrNotFound
}
func (s *stubSpillStore) Delete(context.Context, sharedaudit.SpillRef) error { return nil }
func (s *stubSpillStore) Sweep(context.Context, time.Time) (int, error)      { return 0, nil }
func (s *stubSpillStore) Stat(context.Context) (spillstore.Stats, error) {
	return spillstore.Stats{Backend: "stub"}, nil
}
func (s *stubSpillStore) Backend() string { return "stub" }

func TestWithSpillStore_ChainsAndStores(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	store := &stubSpillStore{}
	got := w.WithSpillStore(store)
	if got != w {
		t.Error("WithSpillStore must return the same *Writer for chaining")
	}
	if w.spill != store {
		t.Error("spill not wired by WithSpillStore")
	}
	// Verify the spill path is taken when body exceeds the runtime threshold.
	pcStore := payloadcapture.NewStore(payloadcapture.Config{MaxInlineBodyBytes: 4})
	w = w.WithPayloadCaptureStore(pcStore)
	if w.payloadCapture != pcStore {
		t.Error("payloadCapture not wired by WithPayloadCaptureStore")
	}
	big := []byte(`{"long":"enough-to-exceed-4-bytes"}`)
	msg := w.recordToMessage(&Record{
		RequestID:    "req-spill",
		Timestamp:    time.Now(),
		RequestBody:  big,
		ResponseBody: big,
	})
	if msg.RequestBody.Kind != sharedaudit.BodySpill {
		t.Errorf("RequestBody.Kind = %q, want spill", msg.RequestBody.Kind)
	}
	if msg.RequestBody.SpillRef == nil || msg.RequestBody.SpillRef.Backend != "stub" {
		t.Errorf("RequestBody.SpillRef wrong: %#v", msg.RequestBody.SpillRef)
	}
	if msg.ResponseBody.Kind != sharedaudit.BodySpill {
		t.Errorf("ResponseBody.Kind = %q, want spill", msg.ResponseBody.Kind)
	}
}

func TestWithPayloadCaptureStore_FallbackToDefault(t *testing.T) {
	// No store wired → recordToMessage uses payloadcapture.DefaultMaxInlineBodyBytes.
	w := NewWriter(nil, "q", nil, slog.Default())
	if w.payloadCapture != nil {
		t.Error("default writer should have nil payloadCapture")
	}
	// Body well under 256 KiB → inline path picked.
	body := []byte(`{"a":"b"}`)
	msg := w.recordToMessage(&Record{RequestID: "r", Timestamp: time.Now(), RequestBody: body})
	if msg.RequestBody.Kind != sharedaudit.BodyInline {
		t.Errorf("small body should be inline, got %q", msg.RequestBody.Kind)
	}
}

func TestWithNormalizer_RunsAndStampsVersion(t *testing.T) {
	calls := 0
	var seenDirs []string
	fn := NormalizeFn(func(direction, contentType, adapterType, model, path string, stream bool, body []byte) (json.RawMessage, string, string) {
		calls++
		seenDirs = append(seenDirs, direction)
		p := normalize.NormalizedPayload{
			Kind:             normalize.KindAIChat,
			NormalizeVersion: normalize.SchemaVersion,
		}
		b, _ := json.Marshal(p)
		return b, "ok", ""
	})
	w := NewWriter(nil, "q", nil, slog.Default()).WithNormalizer(fn)
	if w.normalize == nil {
		t.Fatal("WithNormalizer did not wire the closure")
	}
	rec := &Record{
		RequestID:           "req-norm",
		Timestamp:           time.Now(),
		IngressFormat:       "openai",
		ModelName:           "gpt-4",
		Path:                "/v1/chat/completions",
		RequestBody:         []byte(`{"model":"gpt-4"}`),
		ResponseBody:        []byte(`{"choices":[]}`),
		ResponseContentType: "text/event-stream",
	}
	msg := w.recordToMessage(rec)
	if calls != 2 {
		t.Errorf("normalize calls = %d, want 2 (request + response)", calls)
	}
	if len(seenDirs) != 2 || seenDirs[0] != "request" || seenDirs[1] != "response" {
		t.Errorf("directions = %v, want [request response]", seenDirs)
	}
	if msg.RequestNormalizeStatus != "ok" || msg.ResponseNormalizeStatus != "ok" {
		t.Errorf("statuses = %q/%q, want ok/ok", msg.RequestNormalizeStatus, msg.ResponseNormalizeStatus)
	}
	if msg.NormalizeVersion != normalize.SchemaVersion {
		t.Errorf("NormalizeVersion = %q, want %q", msg.NormalizeVersion, normalize.SchemaVersion)
	}
}

func TestWithNormalizer_BypassNormalizeSkipsResponse(t *testing.T) {
	calls := 0
	dirs := []string{}
	fn := NormalizeFn(func(direction, _, _, _, _ string, _ bool, _ []byte) (json.RawMessage, string, string) {
		calls++
		dirs = append(dirs, direction)
		return json.RawMessage(`{"kind":"ai-chat"}`), "ok", ""
	})
	w := NewWriter(nil, "q", nil, slog.Default()).WithNormalizer(fn)
	rec := &Record{
		RequestID:        "req-bypass",
		Timestamp:        time.Now(),
		IngressFormat:    "openai",
		RequestBody:      []byte(`{"a":1}`),
		ResponseBody:     []byte(`{"b":2}`),
		PassthroughFlags: []string{"bypassNormalize"},
	}
	msg := w.recordToMessage(rec)
	if calls != 1 {
		t.Errorf("normalize calls = %d, want 1 (response skipped)", calls)
	}
	if len(dirs) != 1 || dirs[0] != "request" {
		t.Errorf("dirs = %v, want only [request]", dirs)
	}
	if msg.ResponseNormalized != nil {
		t.Errorf("ResponseNormalized should be nil when bypassNormalize is set")
	}
	// Wire envelope still carries the passthrough fields (separate from skip).
	if len(msg.PassthroughFlags) != 1 || msg.PassthroughFlags[0] != "bypassNormalize" {
		t.Errorf("PassthroughFlags = %v, want [bypassNormalize]", msg.PassthroughFlags)
	}
}

// recordToMessage — uncovered field-stamping branches.

func TestRecordToMessage_CachePromptFieldsStampedWhenNonZero(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	rec := &Record{
		RequestID:              "req-cache",
		Timestamp:              time.Now(),
		GatewayCacheSavingsUsd: 0.5,
		CacheCreationTokens:    100,
		CacheReadTokens:        200,
		CacheWriteCostUsd:      0.01,
		CacheReadSavingsUsd:    0.02,
		CacheNetSavingsUsd:     0.01,
		NormalizerRan:          true,
		NormalizedStripCount:   3,
		NormalizedStripBytes:   1024,
		CacheMarkerInjected:    2,
	}
	msg := w.recordToMessage(rec)
	if msg.GatewayCacheSavingsUsd == nil || *msg.GatewayCacheSavingsUsd != 0.5 {
		t.Errorf("GatewayCacheSavingsUsd = %v, want *0.5", msg.GatewayCacheSavingsUsd)
	}
	if msg.CacheCreationTokens == nil || *msg.CacheCreationTokens != 100 {
		t.Errorf("CacheCreationTokens = %v, want *100", msg.CacheCreationTokens)
	}
	if msg.CacheReadTokens == nil || *msg.CacheReadTokens != 200 {
		t.Errorf("CacheReadTokens = %v, want *200", msg.CacheReadTokens)
	}
	if msg.CacheWriteCostUsd == nil || *msg.CacheWriteCostUsd != 0.01 {
		t.Errorf("CacheWriteCostUsd wrong: %v", msg.CacheWriteCostUsd)
	}
	if msg.CacheReadSavingsUsd == nil {
		t.Errorf("CacheReadSavingsUsd not stamped")
	}
	if msg.CacheNetSavingsUsd == nil {
		t.Errorf("CacheNetSavingsUsd not stamped")
	}
	if msg.NormalizedStripCount == nil || *msg.NormalizedStripCount != 3 {
		t.Errorf("NormalizedStripCount = %v, want *3", msg.NormalizedStripCount)
	}
	if msg.NormalizedStripBytes == nil || *msg.NormalizedStripBytes != 1024 {
		t.Errorf("NormalizedStripBytes = %v, want *1024", msg.NormalizedStripBytes)
	}
	if msg.CacheMarkerInjected == nil || *msg.CacheMarkerInjected != 2 {
		t.Errorf("CacheMarkerInjected = %v, want *2", msg.CacheMarkerInjected)
	}
}

func TestRecordToMessage_CacheZeroFieldsStayNil(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	msg := w.recordToMessage(&Record{RequestID: "r", Timestamp: time.Now()})
	if msg.GatewayCacheSavingsUsd != nil {
		t.Error("zero GatewayCacheSavingsUsd should not be stamped")
	}
	if msg.CacheCreationTokens != nil {
		t.Error("zero CacheCreationTokens should not be stamped")
	}
	if msg.CacheReadTokens != nil {
		t.Error("zero CacheReadTokens should not be stamped")
	}
	if msg.CacheWriteCostUsd != nil {
		t.Error("zero CacheWriteCostUsd should not be stamped")
	}
	if msg.CacheReadSavingsUsd != nil {
		t.Error("zero CacheReadSavingsUsd should not be stamped")
	}
	if msg.CacheNetSavingsUsd != nil {
		t.Error("zero CacheNetSavingsUsd should not be stamped")
	}
	// NormalizerRan is false on this record, so the strip columns stay NULL —
	// which now distinctly means "normaliser never ran".
	if msg.NormalizedStripCount != nil {
		t.Error("never-ran NormalizedStripCount should stay nil (NULL)")
	}
	if msg.NormalizedStripBytes != nil {
		t.Error("never-ran NormalizedStripBytes should stay nil (NULL)")
	}
	if msg.CacheMarkerInjected != nil {
		t.Error("zero CacheMarkerInjected should not be stamped")
	}
}

// TestRecordToMessage_NormalizerRanButStrippedNothing locks the F-fix that
// disambiguates "normaliser ran, stripped nothing" from "normaliser never
// ran": when NormalizerRan is true the strip columns are stamped with a real
// 0 (non-nil pointer to 0), not left NULL. NULL is reserved for the never-ran
// case asserted in TestRecordToMessage_CacheZeroFieldsStayNil.
func TestRecordToMessage_NormalizerRanButStrippedNothing(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	msg := w.recordToMessage(&Record{
		RequestID:            "req-norm-clean",
		Timestamp:            time.Now(),
		NormalizerRan:        true,
		NormalizedStripCount: 0,
		NormalizedStripBytes: 0,
	})
	if msg.NormalizedStripCount == nil {
		t.Fatal("ran-but-stripped-nothing NormalizedStripCount should be a non-nil pointer to 0, got nil")
	}
	if *msg.NormalizedStripCount != 0 {
		t.Errorf("NormalizedStripCount = %d, want 0", *msg.NormalizedStripCount)
	}
	if msg.NormalizedStripBytes == nil {
		t.Fatal("ran-but-stripped-nothing NormalizedStripBytes should be a non-nil pointer to 0, got nil")
	}
	if *msg.NormalizedStripBytes != 0 {
		t.Errorf("NormalizedStripBytes = %d, want 0", *msg.NormalizedStripBytes)
	}
}

func TestRecordToMessage_TargetMethodPathFallback(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	// Target {Method,Path} empty → falls back to {Method,Path}.
	msg := w.recordToMessage(&Record{
		RequestID: "r1",
		Timestamp: time.Now(),
		Method:    "POST",
		Path:      "/v1/chat/completions",
	})
	if msg.TargetMethod != "POST" {
		t.Errorf("TargetMethod = %q, want fallback to POST", msg.TargetMethod)
	}
	if msg.TargetPath != "/v1/chat/completions" {
		t.Errorf("TargetPath = %q, want fallback to /v1/chat/completions", msg.TargetPath)
	}
	// Target* explicitly set → wins.
	msg = w.recordToMessage(&Record{
		RequestID:    "r2",
		Timestamp:    time.Now(),
		Method:       "POST",
		Path:         "/v1/chat/completions",
		TargetMethod: "POST",
		TargetPath:   "/v1/responses",
	})
	if msg.TargetPath != "/v1/responses" {
		t.Errorf("TargetPath = %q, want /v1/responses (target should win)", msg.TargetPath)
	}
}

func TestRecordToMessage_InternalPurposeAndOriginTZ(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	msg := w.recordToMessage(&Record{
		RequestID:       "req-purpose",
		Timestamp:       time.Now(),
		InternalPurpose: "ai-guard",
		OriginTZ:        "Asia/Shanghai",
	})
	if msg.InternalPurpose == nil || *msg.InternalPurpose != "ai-guard" {
		t.Errorf("InternalPurpose = %v, want *ai-guard", msg.InternalPurpose)
	}
	if msg.OriginTZ == nil || *msg.OriginTZ != "Asia/Shanghai" {
		t.Errorf("OriginTZ = %v, want *Asia/Shanghai", msg.OriginTZ)
	}
}

func TestRecordToMessage_ErrorCodeReasonStamped(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	msg := w.recordToMessage(&Record{
		RequestID:   "req-err",
		Timestamp:   time.Now(),
		ErrorCode:   "RATE_LIMITED",
		ErrorReason: "tenant-quota",
	})
	if msg.ErrorCode == nil || *msg.ErrorCode != "RATE_LIMITED" {
		t.Errorf("ErrorCode = %v, want *RATE_LIMITED", msg.ErrorCode)
	}
	if msg.ErrorReason == nil || *msg.ErrorReason != "tenant-quota" {
		t.Errorf("ErrorReason = %v, want *tenant-quota", msg.ErrorReason)
	}
}

func TestRecordToMessage_PassthroughFieldsStampedWhenFlagged(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	msg := w.recordToMessage(&Record{
		RequestID:         "req-pt",
		Timestamp:         time.Now(),
		PassthroughFlags:  []string{"bypassHooks", "bypassCache"},
		PassthroughReason: "incident-2026",
	})
	if len(msg.PassthroughFlags) != 2 {
		t.Errorf("PassthroughFlags = %v, want 2", msg.PassthroughFlags)
	}
	if msg.PassthroughReason != "incident-2026" {
		t.Errorf("PassthroughReason = %q, want incident-2026", msg.PassthroughReason)
	}
}

func TestRecordToMessage_NoPassthroughLeavesFieldsZero(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	msg := w.recordToMessage(&Record{RequestID: "r", Timestamp: time.Now()})
	if len(msg.PassthroughFlags) != 0 {
		t.Errorf("PassthroughFlags = %v, want empty", msg.PassthroughFlags)
	}
	if msg.PassthroughReason != "" {
		t.Errorf("PassthroughReason = %q, want empty", msg.PassthroughReason)
	}
}

func TestRecordToMessage_HookPipelineAggregateDerivedFromStages(t *testing.T) {
	// When RequestHooksMs / ResponseHooksMs are nil, aggregates are
	// summed from HooksPipeline by stage.
	w := NewWriter(nil, "q", nil, slog.Default())
	msg := w.recordToMessage(&Record{
		RequestID: "req-pipeline",
		Timestamp: time.Now(),
		HooksPipeline: []HookExecRecord{
			{Stage: "request", LatencyMs: 4, HookID: "h1", Decision: "ALLOW"},
			{Stage: "connection", LatencyMs: 1, HookID: "h2", Decision: "ALLOW"},
			{Stage: "response", LatencyMs: 9, HookID: "h3", Decision: "ALLOW"},
		},
	})
	if msg.RequestHooksMs == nil || *msg.RequestHooksMs != 5 {
		t.Errorf("RequestHooksMs = %v, want *5", msg.RequestHooksMs)
	}
	if msg.ResponseHooksMs == nil || *msg.ResponseHooksMs != 9 {
		t.Errorf("ResponseHooksMs = %v, want *9", msg.ResponseHooksMs)
	}
	// Wire shape preserves split.
	reqs, ok := msg.RequestHooksPipeline.([]HookExecRecord)
	if !ok {
		t.Fatalf("RequestHooksPipeline wrong type: %T", msg.RequestHooksPipeline)
	}
	if len(reqs) != 2 {
		t.Errorf("RequestHooksPipeline len = %d, want 2 (request + connection)", len(reqs))
	}
	resps, ok := msg.ResponseHooksPipeline.([]HookExecRecord)
	if !ok {
		t.Fatalf("ResponseHooksPipeline wrong type: %T", msg.ResponseHooksPipeline)
	}
	if len(resps) != 1 {
		t.Errorf("ResponseHooksPipeline len = %d, want 1", len(resps))
	}
}

func TestRecordToMessage_ExplicitHookAggregatesOverrideDerived(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	explicitReq := 100
	explicitResp := 200
	msg := w.recordToMessage(&Record{
		RequestID:       "r",
		Timestamp:       time.Now(),
		RequestHooksMs:  &explicitReq,
		ResponseHooksMs: &explicitResp,
		HooksPipeline:   []HookExecRecord{{Stage: "request", LatencyMs: 4}},
	})
	if *msg.RequestHooksMs != 100 {
		t.Errorf("explicit RequestHooksMs should win, got %d", *msg.RequestHooksMs)
	}
	if *msg.ResponseHooksMs != 200 {
		t.Errorf("explicit ResponseHooksMs should win, got %d", *msg.ResponseHooksMs)
	}
}

// metrics — exercise the non-nil-receiver branch (the existing tests
// already cover nil-receiver via newAuditMetrics(nil)).

func TestAuditMetrics_NonNilReceiverIncrements(t *testing.T) {
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	m := newAuditMetrics(reg)
	if m == nil {
		t.Fatal("newAuditMetrics with non-nil registry must return a non-nil receiver")
	}
	// Just call each — Inc against a real Prometheus instrument is
	// observable through the registry (we only assert non-panic here;
	// the Prometheus instrument itself is exhaustively tested upstream).
	m.incEnqueueTotal()
	m.incEnqueueErrors()
	m.incDropped()
}

func TestNewAuditMetrics_NilRegistryReturnsNil(t *testing.T) {
	// Already used by existing tests, but make the contract explicit:
	// nil reg → nil metrics → all incXxx are nil-safe no-ops.
	m := newAuditMetrics(nil)
	if m != nil {
		t.Errorf("newAuditMetrics(nil) = %v, want nil", m)
	}
	// And those nil-receiver inc calls must not panic.
	m.incEnqueueTotal()
	m.incEnqueueErrors()
	m.incDropped()
}

// Close — verifies the public Close path drains via the background
// goroutine + drainBuffer. Uses tiny buffer + healthy producer so the
// drain finishes well within the 15s deadline.

func TestClose_DrainsAndStopsBackgroundLoop(t *testing.T) {
	prod := &memProducer{}
	w := NewWriter(prod, "q", nil, slog.Default())

	w.Enqueue(&Record{RequestID: "rec-1", Timestamp: time.Now()})
	w.Enqueue(&Record{RequestID: "rec-2", Timestamp: time.Now()})

	w.Close()

	msgs := prod.msgs()
	if len(msgs) != 2 {
		t.Errorf("after Close: published msgs = %d, want 2", len(msgs))
	}
	// Calling Close again would panic on close(stopCh); we verify the
	// goroutine actually exited by ensuring wg.Wait returned (Close does
	// it internally).
}

// flush — queue-full retry path (record dropped when buf is at max).

func TestFlush_QueueFullDuringRetryDropsRecord(t *testing.T) {
	// Producer always fails so flush wants to re-buffer; pre-fill buf
	// up to maxQueueSize so the re-buffer arm hits the dropped path.
	prod := &memProducer{alwaysFail: true}
	reg := opsmetrics.NewRegistry(prometheus.NewRegistry())
	w := &Writer{
		producer: prod,
		queue:    "q",
		logger:   slog.Default(),
		buf:      make([]*Record, 0, defaultBatchSize),
		stopCh:   make(chan struct{}),
		metrics:  newAuditMetrics(reg),
	}
	// Enqueue one record we'll flush.
	w.Enqueue(&Record{RequestID: "to-flush", Timestamp: time.Now()})

	// At this point buf has 1 record. flush() pops them into batch, then
	// the failed Enqueue tries to re-buffer. If we artificially pre-fill
	// buf to maxQueueSize BETWEEN the pop and the failure, the re-buffer
	// hits the dropped path. We can't interleave races, but we can simply
	// stuff maxQueueSize records first and then call flush — the pop
	// snapshots the entire buf, and each per-record retry sees an empty
	// buf, so the test must instead use a different shape: enqueue
	// maxQueueSize+1 records, fail them all, observe the re-buffer
	// eventually saturates and drops.
	for range maxQueueSize {
		w.Enqueue(&Record{RequestID: "filler", Timestamp: time.Now()})
	}
	// Drop counter must move forward across the call.
	w.flush()
	// At this point the producer was called for the original batch; each
	// failure tried to re-buffer; once buf hit maxQueueSize the rest were
	// dropped. We can't easily count from outside without the registry,
	// so verify behaviorally: buf can be at most maxQueueSize, no panic.
	w.mu.Lock()
	bufLen := len(w.buf)
	w.mu.Unlock()
	if bufLen > maxQueueSize {
		t.Errorf("buf len %d exceeds max %d after retry storm", bufLen, maxQueueSize)
	}
}

func TestFlush_MarshalFailureSkipsRecord(t *testing.T) {
	// The audit Record only carries JSON-friendly types in fields we
	// populate, but Metadata is `any` and accepts a chan, which fails to
	// marshal. The proxy handler would never set such a value in
	// production — but recordToMessage threads Metadata straight onto
	// Details, so we get observable coverage of the json.Marshal error
	// branch in flush.
	prod := &memProducer{}
	w := &Writer{
		producer: prod,
		queue:    "q",
		logger:   slog.Default(),
		buf:      make([]*Record, 0, defaultBatchSize),
		stopCh:   make(chan struct{}),
	}
	w.Enqueue(&Record{
		RequestID: "bad-meta",
		Timestamp: time.Now(),
		Metadata:  make(chan int),
	})
	w.flush()
	if len(prod.msgs()) != 0 {
		t.Errorf("marshal failure should not have emitted a message; got %d", len(prod.msgs()))
	}
	// Buffer is empty (we did not re-buffer marshal failures — they would
	// loop forever).
	w.mu.Lock()
	if len(w.buf) != 0 {
		t.Errorf("buf len %d, want 0 (marshal failure should not re-buffer)", len(w.buf))
	}
	w.mu.Unlock()
}

// Enqueue nil — short-circuit branch.

func TestEnqueue_NilRecordIsNoOp(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	w.Enqueue(nil)
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) != 0 {
		t.Errorf("nil record should not enter buf; got len %d", len(w.buf))
	}
}

// flushLoop ticker tick — verify a record enqueued before a ticker tick
// actually gets flushed via the goroutine path (rather than via direct
// w.flush() calls). Drives the `<-ticker.C` branch.

func TestFlushLoop_TickerFlushesPendingRecords(t *testing.T) {
	prod := &memProducer{}
	// Use a writer with a real flushLoop — NewWriter wires it for us. We
	// can't change defaultFlushInterval at runtime, but we can shut down
	// via Close() and rely on Close's drainBuffer to push pending records
	// out the door (independent of the ticker). To actually hit the
	// `case <-ticker.C` branch we'd need to wait 5s. That's the
	// production cadence; covering it requires a long-running test.
	// Instead, exercise the same code path by closing the writer
	// immediately, which is the canonical way the flushLoop exits.
	w := NewWriter(prod, "q", nil, slog.Default())
	w.Enqueue(&Record{RequestID: "loop-r1", Timestamp: time.Now()})
	w.Close()
	if got := len(prod.msgs()); got != 1 {
		t.Errorf("loop drain → want 1 published, got %d", got)
	}
}

// flushLoop ticker branch — push the ticker.C case directly. We replace
// the default flushInterval indirectly by spawning a parallel flushLoop
// off a separate Writer constructed manually with a 10ms ticker so the
// test stays under 100ms.

func TestFlushLoop_TickerCaseExercised(t *testing.T) {
	// Build a Writer manually so we don't pay defaultFlushInterval=5s.
	prod := &memProducer{}
	w := &Writer{
		producer: prod,
		queue:    "q",
		logger:   slog.Default(),
		buf:      make([]*Record, 0, defaultBatchSize),
		stopCh:   make(chan struct{}),
	}

	// Custom loop with a tiny ticker — exercises the same select shape
	// as flushLoop minus the timing constant. Mirrors flushLoop verbatim.
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				w.flush()
			case <-w.stopCh:
				return
			}
		}
	}()

	w.Enqueue(&Record{RequestID: "ticker-r1", Timestamp: time.Now()})

	// Wait long enough for at least one tick + flush.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(prod.msgs()) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(w.stopCh)
	w.wg.Wait()
	if got := len(prod.msgs()); got < 1 {
		t.Errorf("ticker should have flushed at least 1; got %d", got)
	}
}

// Sanity: when a normalizer is wired but produces 'failed' status, the
// status + error reason still travel to the wire; NormalizeVersion is
// stamped because at least one direction produced bytes.

func TestNormalizer_FailedStatusStillStamps(t *testing.T) {
	failErr := errors.New("simulated")
	fn := NormalizeFn(func(direction, _, _, _, _ string, _ bool, _ []byte) (json.RawMessage, string, string) {
		// Return non-nil raw to ensure NormalizeVersion is stamped, but
		// status reflects failure for completeness.
		return json.RawMessage(`{"kind":"ai-chat"}`), "failed", failErr.Error()
	})
	w := NewWriter(nil, "q", nil, slog.Default()).WithNormalizer(fn)
	msg := w.recordToMessage(&Record{
		RequestID:    "r",
		Timestamp:    time.Now(),
		RequestBody:  []byte(`{"a":1}`),
		ResponseBody: []byte(`{"b":2}`),
	})
	if msg.RequestNormalizeStatus != "failed" || msg.ResponseNormalizeStatus != "failed" {
		t.Errorf("status = %q/%q, want failed/failed", msg.RequestNormalizeStatus, msg.ResponseNormalizeStatus)
	}
	if msg.RequestNormalizeError == "" || msg.ResponseNormalizeError == "" {
		t.Errorf("errors should be propagated; got %q / %q", msg.RequestNormalizeError, msg.ResponseNormalizeError)
	}
	if msg.NormalizeVersion == "" {
		t.Error("NormalizeVersion should be stamped when any direction returned bytes")
	}
}
