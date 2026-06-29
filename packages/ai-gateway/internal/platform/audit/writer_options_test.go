package audit

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/auth/vkauth"
	sharedaudit "github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/storage/spillstore"
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
		RequestID:      "req-spill",
		Timestamp:      time.Now(),
		RequestBody:    big,
		ResponseBody:   big,
		RequestAction:  decision.ActionApprove,
		ResponseAction: decision.ActionApprove,
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
	msg := w.recordToMessage(&Record{RequestID: "r", Timestamp: time.Now(), RequestBody: body, RequestAction: decision.ActionApprove})
	if msg.RequestBody.Kind != sharedaudit.BodyInline {
		t.Errorf("small body should be inline, got %q", msg.RequestBody.Kind)
	}
}

func TestWithNormalizer_WiredButNotInvokedAtWriteTime(t *testing.T) {
	// The normalize seam stays wireable for tests like this one, but
	// recordToMessage never invokes it: the normalized projection is recomputed at
	// view time, so the wire envelope's normalized columns + NormalizeVersion stay
	// empty.
	fn := NormalizeFn(func(direction, contentType, adapterType, model, path string, stream bool, body []byte) (json.RawMessage, string, string) {
		t.Fatalf("normalize bridge must never run at write time (direction=%s)", direction)
		return nil, "", ""
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
		RequestAction:       decision.ActionApprove,
		ResponseAction:      decision.ActionApprove,
	}
	msg := w.recordToMessage(rec)
	if msg.RequestNormalized != nil || msg.ResponseNormalized != nil {
		t.Errorf("normalized columns must stay nil; got req=%s resp=%s", msg.RequestNormalized, msg.ResponseNormalized)
	}
	if msg.RequestNormalizeStatus != "" || msg.ResponseNormalizeStatus != "" {
		t.Errorf("normalize statuses must stay empty; got %q/%q", msg.RequestNormalizeStatus, msg.ResponseNormalizeStatus)
	}
	if msg.NormalizeVersion != "" {
		t.Errorf("NormalizeVersion must stay empty, got %q", msg.NormalizeVersion)
	}
}

func TestWithNormalizer_BypassNormalizePassthroughFlagsStillStamped(t *testing.T) {
	fn := NormalizeFn(func(direction, _, _, _, _ string, _ bool, _ []byte) (json.RawMessage, string, string) {
		t.Fatalf("normalize bridge must never run at write time (direction=%s)", direction)
		return nil, "", ""
	})
	w := NewWriter(nil, "q", nil, slog.Default()).WithNormalizer(fn)
	rec := &Record{
		RequestID:        "req-bypass",
		Timestamp:        time.Now(),
		IngressFormat:    "openai",
		RequestBody:      []byte(`{"a":1}`),
		ResponseBody:     []byte(`{"b":2}`),
		RequestAction:    decision.ActionApprove,
		ResponseAction:   decision.ActionApprove,
		PassthroughFlags: []string{"bypassNormalize"},
	}
	msg := w.recordToMessage(rec)
	if msg.RequestNormalized != nil || msg.ResponseNormalized != nil {
		t.Errorf("normalized columns must stay nil regardless of bypass; got req=%s resp=%s", msg.RequestNormalized, msg.ResponseNormalized)
	}
	// Wire envelope still carries the passthrough fields.
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

// Close — verifies the public Close path drains the queue via the consumer
// workers and waits for them to exit. Healthy producer so the drain finishes
// promptly.

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

// publish — queue-full retry path (records dropped, counted, when the queue is at
// cap and no durable spill is wired).

func TestPublishBatch_QueueFullDuringRetryDropsRecords(t *testing.T) {
	// Producer always fails so each publish wants to re-queue; a full bounded queue
	// with no durable spill forces the re-queue overflow to a counted bounded drop
	// (handlePublishFailure → spillData(false) → incDropped) — the queue never grows
	// past its cap, and the drops surface on the metric.
	prom := prometheus.NewRegistry()
	prod := &memProducer{alwaysFail: true}
	w := NewWriter(prod, "q", opsmetrics.NewRegistry(prom), slog.Default())
	const cap = 1
	w.recCh = make(chan *Record, cap)
	w.recCh <- &Record{RequestID: "fill"} // saturate

	const n = 16
	batch := make([]*Record, n)
	for i := range batch {
		batch[i] = &Record{RequestID: fmt.Sprintf("filler-%d", i), Timestamp: time.Now()}
	}
	w.publishBatchOn(0, batch) // all fail → re-queue; one fits, the rest drop

	// The queue never grows past its cap under the retry storm.
	if got := len(w.recCh); got > cap {
		t.Errorf("queue len %d exceeds cap %d after retry storm", got, cap)
	}
	// The overflow records were counted as drops (n records, one re-queued slot →
	// at least n-cap drops surface on the metric).
	if got := counterValue(t, prom, "nexus_audit_mq_dropped_total"); got < float64(n-cap) {
		t.Errorf("dropped_total = %v, want >= %d (retry-storm overflow must count drops)", got, n-cap)
	}
}

func TestPublishBatch_MarshalFailureSkipsRecord(t *testing.T) {
	// The audit Record only carries JSON-friendly types in fields we
	// populate, but Metadata is `any` and accepts a chan, which fails to
	// marshal. The proxy handler would never set such a value in
	// production — but recordToMessage threads Metadata straight onto
	// Details, so we get observable coverage of the json.Marshal error
	// branch in marshalRecord (the record is dropped at marshal, never
	// published and never re-queued — a re-queue would loop forever).
	prod := &memProducer{}
	w := NewWriter(prod, "q", nil, slog.Default())
	w.recCh = make(chan *Record, 4) // would catch any erroneous re-queue
	w.publishBatchOn(0, []*Record{{
		RequestID: "bad-meta",
		Timestamp: time.Now(),
		Metadata:  make(chan int),
	}})
	if len(prod.msgs()) != 0 {
		t.Errorf("marshal failure should not have emitted a message; got %d", len(prod.msgs()))
	}
	// The failed record was not re-queued (a marshal failure is terminal — re-queuing
	// it would loop forever).
	if len(w.recCh) != 0 {
		t.Errorf("queue len %d, want 0 (marshal failure should not re-queue)", len(w.recCh))
	}
}

// Enqueue nil — short-circuit branch.

func TestEnqueue_NilRecordIsNoOp(t *testing.T) {
	w := NewWriter(nil, "q", nil, slog.Default())
	// Claim startOnce so Enqueue's ensureStarted() does not start consumers, then
	// size the queue so we can observe that a nil record never enters it.
	w.startOnce.Do(func() {})
	w.recCh = make(chan *Record, 4)
	w.Enqueue(nil)
	if len(w.recCh) != 0 {
		t.Errorf("nil record should not enter the queue; got len %d", len(w.recCh))
	}
}

// Consumer lazy-start via Enqueue — a record enqueued onto a writer that was never
// explicitly Start()ed must still be published: the first Enqueue lazily starts the
// consumer workers (ensureStarted), and Close drains the remainder. Pins the
// lazy-start contract that lets a caller skip Start().
func TestEnqueue_LazyStartsConsumerAndPublishes(t *testing.T) {
	prod := &memProducer{}
	w := NewWriter(prod, "q", nil, slog.Default()) // NOT Start()ed
	w.Enqueue(&Record{RequestID: "loop-r1", Timestamp: time.Now()})
	w.Close() // drains the queue via the lazily-started consumer
	if got := len(prod.msgs()); got != 1 {
		t.Errorf("lazy-start drain → want 1 published, got %d", got)
	}
}

// consumeLoop linger branch — a single record (a partial batch, well under
// batchMaxCount) is published by the consumer's consumerLinger timer rather than
// being held until a full batch accumulates. Exercises the `<-timer.C` arm of the
// real consumeLoop.

func TestConsumeLoop_LingerFlushesPartialBatch(t *testing.T) {
	prod := &memProducer{}
	w := NewWriter(prod, "q", nil, slog.Default()).Start()
	defer w.Close()

	// One record only — far below batchMaxCount, so it can only be published when the
	// consumerLinger timer fires for the partial batch.
	w.Enqueue(&Record{RequestID: "linger-r1", Timestamp: time.Now()})

	// consumerLinger is 100ms; allow generous slack for scheduling.
	if got := waitForMsgCount(prod, 1, 2*time.Second); got != 1 {
		t.Errorf("linger timer should have published the partial batch; got %d", got)
	}
}

// The write path no longer invokes the normalize bridge, so a wired
// normalizer — even one that would report a failed status — never runs and
// never stamps the normalize status / error / version columns.

func TestNormalizer_NotInvokedSoNoStatusStamped(t *testing.T) {
	fn := NormalizeFn(func(direction, _, _, _, _ string, _ bool, _ []byte) (json.RawMessage, string, string) {
		t.Fatalf("normalize bridge must never run at write time (direction=%s)", direction)
		return nil, "", ""
	})
	w := NewWriter(nil, "q", nil, slog.Default()).WithNormalizer(fn)
	msg := w.recordToMessage(&Record{
		RequestID:      "r",
		Timestamp:      time.Now(),
		RequestBody:    []byte(`{"a":1}`),
		ResponseBody:   []byte(`{"b":2}`),
		RequestAction:  decision.ActionApprove,
		ResponseAction: decision.ActionApprove,
	})
	if msg.RequestNormalizeStatus != "" || msg.ResponseNormalizeStatus != "" {
		t.Errorf("normalize statuses must stay empty; got %q/%q", msg.RequestNormalizeStatus, msg.ResponseNormalizeStatus)
	}
	if msg.RequestNormalizeError != "" || msg.ResponseNormalizeError != "" {
		t.Errorf("normalize errors must stay empty; got %q/%q", msg.RequestNormalizeError, msg.ResponseNormalizeError)
	}
	if msg.NormalizeVersion != "" {
		t.Errorf("NormalizeVersion must stay empty, got %q", msg.NormalizeVersion)
	}
}
