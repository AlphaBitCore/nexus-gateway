package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// newWebhookHook builds a webhook-forward hook for tests. Production default
// for webhook-forward without an explicit `onMatch` block is
// `inflightAction=approve` (advisory ceiling — webhook drives the decision),
// so tests that don't override `onMatch` exercise the same path admins get
// when they leave the field off. Reconcile-specific tests pass their own
// `onMatch` block explicitly to drive a non-default ceiling.
func newWebhookHook(t *testing.T, endpoint string, extraConfig map[string]any) Hook {
	t.Helper()
	cfg := &HookConfig{
		ID:               "wh-1",
		ImplementationID: "webhook-forward",
		Name:             "test-webhook",
		Config:           map[string]any{"endpoint": endpoint},
	}
	for k, v := range extraConfig {
		cfg.Config[k] = v
	}
	h, err := NewWebhookForward(cfg)
	if err != nil {
		t.Fatalf("NewWebhookForward: %v", err)
	}
	return h
}

func TestWebhookForward_Factory_MissingEndpointRejected(t *testing.T) {
	_, err := NewWebhookForward(&HookConfig{Config: map[string]any{}})
	if err == nil {
		t.Fatal("missing endpoint should error")
	}
	if !strings.Contains(err.Error(), "endpoint") {
		t.Errorf("error should mention endpoint: %v", err)
	}
}

func TestWebhookForward_Factory_EmptyEndpointRejected(t *testing.T) {
	_, err := NewWebhookForward(&HookConfig{Config: map[string]any{"endpoint": ""}})
	if err == nil {
		t.Fatal("empty endpoint should error")
	}
}

func TestWebhookForward_Factory_UnknownPayloadModeRejected(t *testing.T) {
	_, err := NewWebhookForward(&HookConfig{Config: map[string]any{
		"endpoint":    "http://example.com",
		"payloadMode": "everything-and-the-kitchen-sink",
	}})
	if err == nil {
		t.Fatal("unknown payloadMode should error")
	}
	if !strings.Contains(err.Error(), "payloadMode") {
		t.Errorf("error should mention payloadMode: %v", err)
	}
}

func TestWebhookForward_Factory_TimeoutOverrideAccepted(t *testing.T) {
	h, err := NewWebhookForward(&HookConfig{Config: map[string]any{
		"endpoint":  "http://example.com",
		"timeoutMs": float64(123),
	}})
	if err != nil {
		t.Fatalf("timeoutMs override: %v", err)
	}
	wf := h.(*WebhookForward)
	if wf.timeout.Milliseconds() != 123 {
		t.Errorf("timeout: got %v, want 123ms", wf.timeout)
	}
}

func TestWebhookForward_Factory_DefaultPayloadModeRedacted(t *testing.T) {
	h, err := NewWebhookForward(&HookConfig{Config: map[string]any{
		"endpoint": "http://example.com",
	}})
	if err != nil {
		t.Fatal(err)
	}
	wf := h.(*WebhookForward)
	if wf.payloadMode != WebhookPayloadRedacted {
		t.Errorf("default payloadMode: got %q want redacted", wf.payloadMode)
	}
}

func TestWebhookForward_Factory_OnMatchValidationPropagates(t *testing.T) {
	_, err := NewWebhookForward(&HookConfig{Config: map[string]any{
		"endpoint": "http://example.com",
		"onMatch":  map[string]any{"inflightAction": "purge-it"},
	}})
	if err == nil {
		t.Fatal("bad onMatch should be rejected")
	}
	if !strings.Contains(err.Error(), "webhook-forward") {
		t.Errorf("error should be wrapped with webhook-forward prefix: %v", err)
	}
}

func TestWebhookForward_Factory_WithClientUsesProvidedClient(t *testing.T) {
	// When a custom client is provided the hook must use it (not create
	// its own).
	custom := &http.Client{}
	h, err := NewWebhookForwardWithClient(&HookConfig{
		Config: map[string]any{"endpoint": "http://example.com"},
	}, custom)
	if err != nil {
		t.Fatal(err)
	}
	wf := h.(*WebhookForward)
	if wf.client != custom {
		t.Error("custom client was not retained")
	}
}

func TestWebhookForward_Execute_DecisionMapping(t *testing.T) {
	cases := []struct {
		wireDecision string
		want         Decision
	}{
		{"reject", RejectHard},
		{"reject_hard", RejectHard},
		{"REJECT_HARD", RejectHard}, // case-insensitive
		{"  reject  ", RejectHard},  // trimmed
		{"block_soft", BlockSoft},
		{"modify", Modify},
		{"abstain", Abstain},
		{"approve", Approve},       // explicit approve maps to Approve
		{"unknown-thing", Approve}, // unrecognized → safe default Approve
	}
	for _, tc := range cases {
		t.Run(tc.wireDecision, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"decision":   tc.wireDecision,
					"reason":     "test reason",
					"reasonCode": "TEST_CODE",
				})
			}))
			defer srv.Close()

			h := newWebhookHook(t, srv.URL, nil)
			res, err := h.Execute(context.Background(), &HookInput{Stage: "request"})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.Decision != tc.want {
				t.Errorf("decision: got %s want %s for wire %q", res.Decision, tc.want, tc.wireDecision)
			}
		})
	}
}

func TestWebhookForward_Execute_ReasonAndReasonCodePropagate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"decision":   "reject",
			"reason":     "policy violation X",
			"reasonCode": "POLICY_X_BLOCKED",
		})
	}))
	defer srv.Close()
	h := newWebhookHook(t, srv.URL, nil)
	res, _ := h.Execute(context.Background(), &HookInput{})
	if res.Reason != "policy violation X" {
		t.Errorf("Reason: %q", res.Reason)
	}
	if res.ReasonCode != "POLICY_X_BLOCKED" {
		t.Errorf("ReasonCode: %q", res.ReasonCode)
	}
}

func TestWebhookForward_Execute_RedactedPayloadModeShipsTextSegments(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"decision":"approve"}`))
	}))
	defer srv.Close()

	h := newWebhookHook(t, srv.URL, nil) // default redacted
	_, err := h.Execute(context.Background(), &HookInput{
		Stage:       "request",
		Method:      "POST",
		Path:        "/v1/chat/completions",
		TargetHost:  "api.openai.com",
		SourceIP:    "10.0.0.1",
		BodySize:    1024,
		ContentType: "application/json",
		Model:       "gpt-4",
		IngressType: "AI_GATEWAY",
		Normalized:  PayloadFromTextSegments([]string{"hello", "world"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got["payloadMode"] != "redacted" {
		t.Errorf("payloadMode: %v", got["payloadMode"])
	}
	segs, _ := got["normalizedContent"].([]any)
	if len(segs) != 2 || segs[0] != "hello" || segs[1] != "world" {
		t.Errorf("redacted mode should ship text segments; got %+v", segs)
	}
	if got["normalized"] != nil {
		t.Errorf("redacted mode must NOT ship full normalized payload; got %v", got["normalized"])
	}
	// Envelope fields must be present regardless of mode.
	if got["targetHost"] != "api.openai.com" || got["model"] != "gpt-4" {
		t.Errorf("envelope fields missing: %+v", got)
	}
}

func TestWebhookForward_Execute_RedactedPayloadMode_NoSegmentsOmitsField(t *testing.T) {
	// When TextSegments is empty (nil Normalized), normalizedContent should
	// be omitted entirely — not sent as an empty array.
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"decision":"approve"}`))
	}))
	defer srv.Close()

	h := newWebhookHook(t, srv.URL, nil)
	_, _ = h.Execute(context.Background(), &HookInput{Stage: "request"})
	if _, exists := got["normalizedContent"]; exists {
		t.Errorf("normalizedContent should be omitted on no-segments; got %v", got["normalizedContent"])
	}
}

func TestWebhookForward_Execute_FullPayloadModeShipsNormalized(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"decision":"approve"}`))
	}))
	defer srv.Close()

	h := newWebhookHook(t, srv.URL, map[string]any{"payloadMode": "full"})
	_, _ = h.Execute(context.Background(), &HookInput{
		Normalized: PayloadFromTextSegments([]string{"the full payload"}),
	})
	if got["payloadMode"] != "full" {
		t.Errorf("payloadMode: %v", got["payloadMode"])
	}
	if got["normalized"] == nil {
		t.Errorf("full mode should ship normalized; got %+v", got)
	}
	if _, exists := got["normalizedContent"]; exists {
		t.Errorf("full mode must NOT ship normalizedContent")
	}
}

func TestWebhookForward_Execute_FullPayloadMode_NilNormalizedOmitsField(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"decision":"approve"}`))
	}))
	defer srv.Close()
	h := newWebhookHook(t, srv.URL, map[string]any{"payloadMode": "full"})
	_, _ = h.Execute(context.Background(), &HookInput{Stage: "request"})
	if _, exists := got["normalized"]; exists {
		t.Errorf("nil Normalized should omit field; got %v", got["normalized"])
	}
}

func TestWebhookForward_Execute_MetadataOnlyOmitsAllContent(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"decision":"approve"}`))
	}))
	defer srv.Close()
	h := newWebhookHook(t, srv.URL, map[string]any{"payloadMode": "metadata-only"})
	_, _ = h.Execute(context.Background(), &HookInput{
		TargetHost: "api.openai.com",
		Normalized: PayloadFromTextSegments([]string{"sensitive text"}),
	})
	if got["payloadMode"] != "metadata-only" {
		t.Errorf("payloadMode: %v", got["payloadMode"])
	}
	if _, exists := got["normalized"]; exists {
		t.Errorf("metadata-only must NOT ship normalized payload")
	}
	if _, exists := got["normalizedContent"]; exists {
		t.Errorf("metadata-only must NOT ship normalizedContent")
	}
	// Envelope still present.
	if got["targetHost"] != "api.openai.com" {
		t.Errorf("envelope missing: %+v", got)
	}
}

func TestWebhookForward_Execute_NetworkErrorReturnsError(t *testing.T) {
	// Use a closed-port endpoint — guaranteed connection refused.
	h := newWebhookHook(t, "http://127.0.0.1:1", nil)
	_, err := h.Execute(context.Background(), &HookInput{Stage: "request"})
	if err == nil {
		t.Fatal("expected network error")
	}
	if !strings.Contains(err.Error(), "webhook-forward") {
		t.Errorf("error should be wrapped: %v", err)
	}
}

func TestWebhookForward_Execute_UnparseableResponseFallsBackToApprove(t *testing.T) {
	// Non-JSON response body — must fall back to APPROVE so a misbehaving
	// webhook can't block traffic.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	h := newWebhookHook(t, srv.URL, nil)
	res, err := h.Execute(context.Background(), &HookInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Approve {
		t.Errorf("unparseable response: got %s want Approve (fail-open)", res.Decision)
	}
	if !strings.Contains(res.Reason, "unparseable") {
		t.Errorf("reason should mention unparseable: %q", res.Reason)
	}
}

func TestWebhookForward_Execute_ContentTypeHeaderSet(t *testing.T) {
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{"decision":"approve"}`))
	}))
	defer srv.Close()
	h := newWebhookHook(t, srv.URL, nil)
	_, _ = h.Execute(context.Background(), &HookInput{})
	if gotCT != "application/json" {
		t.Errorf("Content-Type: %q", gotCT)
	}
}

func TestWebhookForward_Execute_MethodIsPOST(t *testing.T) {
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		_, _ = w.Write([]byte(`{"decision":"approve"}`))
	}))
	defer srv.Close()
	h := newWebhookHook(t, srv.URL, nil)
	_, _ = h.Execute(context.Background(), &HookInput{})
	if gotMethod != http.MethodPost {
		t.Errorf("method: %q want POST", gotMethod)
	}
}

func TestWebhookForward_Execute_RedactionsDecodedIntoSpans(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"decision": "modify",
			"redactions": []map[string]any{
				{"start": 0, "end": 5, "replacement": "[X]", "action": "redact", "reason": "pii"},
				{"start": 10, "end": 12, "replacement": "[Y]", "action": "strip"},
				{"start": 20, "end": 21, "replacement": "Z", "action": "inject"},
				{"start": 25, "end": 30, "replacement": "[R]", "action": "replace"},
				{"start": 40, "end": 45, "replacement": "[U]"}, // no action → default redact
				{"start": 50, "end": 55, "action": "WEIRD"},    // unknown action → default redact
			},
		})
	}))
	defer srv.Close()

	h := newWebhookHook(t, srv.URL, nil)
	res, err := h.Execute(context.Background(), &HookInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Modify {
		t.Errorf("decision: %s", res.Decision)
	}
	if len(res.TransformSpans) != 6 {
		t.Fatalf("len(spans): got %d want 6", len(res.TransformSpans))
	}
	wantActions := []normalize.TransformAction{
		normalize.ActionRedact,
		normalize.ActionStrip,
		normalize.ActionInject,
		normalize.ActionReplace,
		normalize.ActionRedact, // missing action defaults to redact
		normalize.ActionRedact, // unknown action defaults to redact
	}
	for i, sp := range res.TransformSpans {
		if sp.Action != wantActions[i] {
			t.Errorf("span[%d].Action: got %s want %s", i, sp.Action, wantActions[i])
		}
		if sp.Source != normalize.SourceHook {
			t.Errorf("span[%d].Source: got %s want SourceHook", i, sp.Source)
		}
		if sp.SourceID != srv.URL {
			t.Errorf("span[%d].SourceID: got %q want endpoint URL", i, sp.SourceID)
		}
		if sp.ContentAddress != "webhook.flat" {
			t.Errorf("span[%d].ContentAddress: got %q want webhook.flat", i, sp.ContentAddress)
		}
	}
	// First span should carry reason from wire.
	if res.TransformSpans[0].Reason != "pii" {
		t.Errorf("span[0].Reason: %q want pii", res.TransformSpans[0].Reason)
	}
}

func TestWebhookForward_Execute_NoRedactionsYieldsNilSpans(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"decision":"approve"}`))
	}))
	defer srv.Close()
	h := newWebhookHook(t, srv.URL, nil)
	res, _ := h.Execute(context.Background(), &HookInput{})
	if res.TransformSpans != nil {
		t.Errorf("spans should be nil on no redactions; got %v", res.TransformSpans)
	}
}

func TestRedactionsToTransformSpans_EmptyInputReturnsNil(t *testing.T) {
	if got := redactionsToTransformSpans(nil, "http://x"); got != nil {
		t.Errorf("nil input: got %v want nil", got)
	}
	if got := redactionsToTransformSpans([]webhookRedactionWire{}, "http://x"); got != nil {
		t.Errorf("empty input: got %v want nil", got)
	}
}

func TestRedactionsToTransformSpans_ActionMappingExhaustive(t *testing.T) {
	in := []webhookRedactionWire{
		{Action: "redact"},
		{Action: "REDACT"},    // case-insensitive
		{Action: "  strip  "}, // trimmed
		{Action: "inject"},
		{Action: "replace"},
		{Action: ""},      // empty → default redact
		{Action: "bogus"}, // unknown → default redact
	}
	got := redactionsToTransformSpans(in, "http://endpoint")
	want := []normalize.TransformAction{
		normalize.ActionRedact,
		normalize.ActionRedact,
		normalize.ActionStrip,
		normalize.ActionInject,
		normalize.ActionReplace,
		normalize.ActionRedact,
		normalize.ActionRedact,
	}
	if len(got) != len(want) {
		t.Fatalf("len: got %d want %d", len(got), len(want))
	}
	for i, sp := range got {
		if sp.Action != want[i] {
			t.Errorf("[%d] Action: got %s want %s", i, sp.Action, want[i])
		}
	}
}

func TestRedactionsToTransformSpans_FieldPropagation(t *testing.T) {
	in := []webhookRedactionWire{{
		Start: 3, End: 7, Replacement: "[***]", Action: "redact", Reason: "email",
	}}
	got := redactionsToTransformSpans(in, "http://hook")
	if len(got) != 1 {
		t.Fatalf("len: %d", len(got))
	}
	sp := got[0]
	if sp.Start != 3 || sp.End != 7 {
		t.Errorf("offsets: (%d,%d)", sp.Start, sp.End)
	}
	if sp.Replacement != "[***]" {
		t.Errorf("Replacement: %q", sp.Replacement)
	}
	if sp.Reason != "email" {
		t.Errorf("Reason: %q", sp.Reason)
	}
	if sp.SourceID != "http://hook" {
		t.Errorf("SourceID: %q", sp.SourceID)
	}
	if sp.Source != normalize.SourceHook {
		t.Errorf("Source: %s", sp.Source)
	}
	if sp.ContentAddress != "webhook.flat" {
		t.Errorf("ContentAddress: %q", sp.ContentAddress)
	}
}

func TestWebhookForward_Execute_PreCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"decision":"approve"}`))
	}))
	defer srv.Close()
	h := newWebhookHook(t, srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := h.Execute(ctx, &HookInput{})
	if err == nil {
		t.Fatal("pre-cancelled context should produce error")
	}
}

// ─── Reconcile against onMatch.InflightAction policy ceiling ────────────────
//
// The five tests below pin the AI-Guard reconcile contract: when the webhook's
// suggested decision is permissive but the admin policy ceiling is strict,
// the ceiling wins and ReasonAIGuardSuggestedVsPolicy is stamped. When the
// suggestion matches or exceeds the ceiling, the suggestion's Reason/ReasonCode
// pass through verbatim. Abstain short-circuits the reconcile entirely so the
// pipeline aggregator can still skip a no-opinion hook.

func TestWebhookForward_Reconcile_PolicyCeilingOverridesPermissiveSuggestion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"decision":   "approve",
			"reason":     "webhook says ok",
			"reasonCode": "WEBHOOK_OK",
		})
	}))
	defer srv.Close()

	// Admin policy: block-hard ceiling. Webhook's approve must be clobbered.
	h := newWebhookHook(t, srv.URL, map[string]any{
		"onMatch": map[string]any{"inflightAction": "block-hard"},
	})
	res, err := h.Execute(context.Background(), &HookInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != RejectHard {
		t.Errorf("Decision: got %s want REJECT_HARD (policy ceiling)", res.Decision)
	}
	if res.ReasonCode != "AIGUARD_SUGGESTED_VS_POLICY" {
		t.Errorf("ReasonCode: got %q want AIGUARD_SUGGESTED_VS_POLICY", res.ReasonCode)
	}
	if !strings.Contains(res.Reason, "webhook suggested approve") {
		t.Errorf("Reason should name webhook's suggestion; got %q", res.Reason)
	}
	if !strings.Contains(res.Reason, "policy ceiling: block-hard") {
		t.Errorf("Reason should name policy ceiling in InflightAction vocab; got %q", res.Reason)
	}
}

func TestWebhookForward_Reconcile_SuggestionMatchesCeilingPassesThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"decision":   "reject_hard",
			"reason":     "policy violation",
			"reasonCode": "POLICY_X",
		})
	}))
	defer srv.Close()

	// Suggestion (reject_hard) == ceiling (block-hard). No override fires;
	// webhook's reason/reasonCode propagate verbatim.
	h := newWebhookHook(t, srv.URL, map[string]any{
		"onMatch": map[string]any{"inflightAction": "block-hard"},
	})
	res, err := h.Execute(context.Background(), &HookInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != RejectHard {
		t.Errorf("Decision: got %s want REJECT_HARD", res.Decision)
	}
	if res.ReasonCode != "POLICY_X" {
		t.Errorf("ReasonCode: got %q want POLICY_X (webhook's, no override stamped)", res.ReasonCode)
	}
	if res.Reason != "policy violation" {
		t.Errorf("Reason: got %q want webhook's verbatim", res.Reason)
	}
}

func TestWebhookForward_Reconcile_SuggestionStricterThanCeilingPassesThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"decision":   "reject_hard",
			"reason":     "ai-guard flagged",
			"reasonCode": "AIGUARD_FLAGGED",
		})
	}))
	defer srv.Close()

	// Webhook reject_hard > ceiling redact (Modify). Webhook wins — strictest.
	h := newWebhookHook(t, srv.URL, map[string]any{
		"onMatch": map[string]any{"inflightAction": "redact"},
	})
	res, err := h.Execute(context.Background(), &HookInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != RejectHard {
		t.Errorf("Decision: got %s want REJECT_HARD", res.Decision)
	}
	if res.ReasonCode != "AIGUARD_FLAGGED" {
		t.Errorf("ReasonCode: got %q want AIGUARD_FLAGGED (no override since webhook stricter)", res.ReasonCode)
	}
}

func TestWebhookForward_Reconcile_ModifyCeilingPromotesApproveToModify(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"decision":   "approve",
			"reason":     "webhook says clean",
			"reasonCode": "WEBHOOK_CLEAN",
		})
	}))
	defer srv.Close()

	// Ceiling = redact (Modify) > approve. Decision becomes Modify; both
	// values appear in Reason.
	h := newWebhookHook(t, srv.URL, map[string]any{
		"onMatch": map[string]any{"inflightAction": "redact"},
	})
	res, err := h.Execute(context.Background(), &HookInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Modify {
		t.Errorf("Decision: got %s want MODIFY (redact ceiling)", res.Decision)
	}
	if res.ReasonCode != "AIGUARD_SUGGESTED_VS_POLICY" {
		t.Errorf("ReasonCode: got %q", res.ReasonCode)
	}
	if !strings.Contains(res.Reason, "webhook suggested approve") {
		t.Errorf("Reason should name webhook's suggestion; got %q", res.Reason)
	}
	if !strings.Contains(res.Reason, "policy ceiling: redact") {
		t.Errorf("Reason should name policy ceiling in InflightAction vocab; got %q", res.Reason)
	}
}

func TestWebhookForward_Reconcile_BlockSoftCeilingPromotesApprove(t *testing.T) {
	// Pins the BlockSoft > Modify ordering in StrictestDecision: with a
	// block-soft ceiling and an approve suggestion, reconcile lands on
	// BlockSoft, and the Reason renders both halves in InflightAction
	// vocabulary.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"decision":   "approve",
			"reason":     "webhook clean",
			"reasonCode": "WEBHOOK_CLEAN",
		})
	}))
	defer srv.Close()
	h := newWebhookHook(t, srv.URL, map[string]any{
		"onMatch": map[string]any{"inflightAction": "block-soft"},
	})
	res, err := h.Execute(context.Background(), &HookInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != BlockSoft {
		t.Errorf("Decision: got %s want BLOCK_SOFT", res.Decision)
	}
	if res.ReasonCode != "AIGUARD_SUGGESTED_VS_POLICY" {
		t.Errorf("ReasonCode: got %q", res.ReasonCode)
	}
	if !strings.Contains(res.Reason, "policy ceiling: block-soft") {
		t.Errorf("Reason should render ceiling in InflightAction vocab; got %q", res.Reason)
	}
}

func TestWebhookForward_PartialOnMatchStorageOnlyStillGetsApproveCeiling(t *testing.T) {
	// Pins the Q4 partial-config bypass fix: an `onMatch` block that
	// only configures storageAction (no inflightAction key) must still
	// trigger the webhook-forward approve-ceiling override. Otherwise a
	// partial config silently inherits ParseOnMatch's block-hard default
	// and clobbers webhook decisions.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"decision":   "approve",
			"reason":     "webhook clean",
			"reasonCode": "WEBHOOK_CLEAN",
		})
	}))
	defer srv.Close()
	cfg := &HookConfig{
		ID:               "wh-partial",
		ImplementationID: "webhook-forward",
		Name:             "test-webhook-partial",
		Config: map[string]any{
			"endpoint": srv.URL,
			"onMatch": map[string]any{
				// Only storageAction set — inflightAction key absent.
				"storageAction": "redact",
			},
		},
	}
	h, err := NewWebhookForward(cfg)
	if err != nil {
		t.Fatalf("NewWebhookForward: %v", err)
	}
	res, err := h.Execute(context.Background(), &HookInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Approve {
		t.Errorf("partial-onMatch without inflightAction should still get approve ceiling; got %s", res.Decision)
	}
	if res.ReasonCode != "WEBHOOK_CLEAN" {
		t.Errorf("ReasonCode: got %q want WEBHOOK_CLEAN (no reconcile should fire)", res.ReasonCode)
	}
}

func TestWebhookForward_DefaultOnMatchIsApproveCeiling(t *testing.T) {
	// Pins the Q4 fix: webhook-forward overrides ParseOnMatch's
	// block-hard default to InflightApprove so production admins who
	// leave `onMatch` off do NOT see every webhook decision clobbered
	// to RejectHard.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"decision":   "approve",
			"reason":     "webhook clean",
			"reasonCode": "WEBHOOK_CLEAN",
		})
	}))
	defer srv.Close()
	// Build the hook WITHOUT injecting onMatch via the helper —
	// construct the config directly to test the no-onMatch path.
	cfg := &HookConfig{
		ID:               "wh-default",
		ImplementationID: "webhook-forward",
		Name:             "test-webhook-default",
		Config:           map[string]any{"endpoint": srv.URL},
	}
	h, err := NewWebhookForward(cfg)
	if err != nil {
		t.Fatalf("NewWebhookForward: %v", err)
	}
	res, err := h.Execute(context.Background(), &HookInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Approve {
		t.Errorf("Decision: got %s want APPROVE (default ceiling should not clobber)", res.Decision)
	}
	if res.ReasonCode != "WEBHOOK_CLEAN" {
		t.Errorf("ReasonCode: got %q want WEBHOOK_CLEAN (no reconcile fired)", res.ReasonCode)
	}
}

func TestWebhookForward_Reconcile_AbstainShortCircuitsReconcile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"decision":   "abstain",
			"reason":     "no signal",
			"reasonCode": "NO_SIGNAL",
		})
	}))
	defer srv.Close()

	// Abstain = no opinion. Even with a block-hard ceiling, the per-hook
	// decision stays Abstain so the pipeline aggregator can still skip it.
	h := newWebhookHook(t, srv.URL, map[string]any{
		"onMatch": map[string]any{"inflightAction": "block-hard"},
	})
	res, err := h.Execute(context.Background(), &HookInput{})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Abstain {
		t.Errorf("Decision: got %s want ABSTAIN (reconcile must short-circuit on abstain)", res.Decision)
	}
	if res.ReasonCode != "NO_SIGNAL" {
		t.Errorf("ReasonCode: got %q want NO_SIGNAL (webhook's verbatim — no override stamp on abstain)", res.ReasonCode)
	}
}
