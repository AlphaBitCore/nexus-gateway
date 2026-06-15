package redact

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestApplyStorageAction_EmptyInput(t *testing.T) {
	// len == 0 short-circuits regardless of action.
	for _, action := range []string{"", "keep", "redact", "drop-content"} {
		got, _ := ApplyStorageAction(nil, action, nil, nil, nil)
		if got != nil {
			t.Errorf("action=%q empty input → want nil, got %q", action, got)
		}
	}
}

func TestApplyStorageAction_KeepNoop(t *testing.T) {
	raw := json.RawMessage(`{"kind":"ai-chat"}`)
	for _, action := range []string{"", "keep"} {
		got, spans := ApplyStorageAction(raw, action, nil, nil, nil)
		if string(got) != string(raw) {
			t.Errorf("action=%q → want unchanged, got %q", action, got)
		}
		if spans != nil {
			t.Errorf("keep returns no spans, got %v", spans)
		}
	}
}

func TestApplyStorageAction_UnknownActionFallsThrough(t *testing.T) {
	raw := json.RawMessage(`{"kind":"ai-chat"}`)
	got, _ := ApplyStorageAction(raw, "unrecognized", nil, nil, nil)
	if string(got) != string(raw) {
		t.Errorf("unknown action → want unchanged, got %q", got)
	}
}

func TestApplyStorageAction_RedactNoSpansDegradesToPlaceholder(t *testing.T) {
	// A hook demanded redaction but located no byte ranges (keyword /
	// content-safety matches carry no spans). Persisting the raw payload
	// would ignore the operator's policy — the helper degrades to the
	// drop-content placeholder, stamped as a degradation (not an operator
	// drop) with cause "no-spans".
	raw := json.RawMessage(`{"kind":"ai-chat","messages":[{"role":"user","content":[{"type":"text","text":"secret"}]}]}`)
	got, spans := ApplyStorageAction(raw, "redact", nil, []string{"kw-1"}, nil)
	if strings.Contains(string(got), "secret") {
		t.Fatalf("redact with no spans must not persist content, got %q", got)
	}
	var p normalize.NormalizedPayload
	if err := json.Unmarshal(got, &p); err != nil {
		t.Fatalf("placeholder unmarshal: %v", err)
	}
	if !p.Redacted || len(p.RuleIDs) != 1 || p.RuleIDs[0] != "kw-1" {
		t.Errorf("want {redacted:true, ruleIds:[kw-1]} placeholder, got %q", got)
	}
	if p.RedactedReason != normalize.RedactedReasonDegraded {
		t.Errorf("redactedReason = %q, want %q", p.RedactedReason, normalize.RedactedReasonDegraded)
	}
	if p.RedactedDetail == nil || p.RedactedDetail.Cause != normalize.DegradeCauseNoSpans {
		t.Errorf("redactedDetail = %+v, want cause %q", p.RedactedDetail, normalize.DegradeCauseNoSpans)
	}
	if p.RedactedDetail != nil && p.RedactedDetail.FailedAddresses != nil {
		t.Errorf("no-spans degradation has no failed addresses, got %v", p.RedactedDetail.FailedAddresses)
	}
	if spans != nil {
		t.Errorf("no spans existed, so none can be preserved, got %v", spans)
	}
}

func TestApplyStorageAction_RedactBadJSONDegradesToPlaceholder(t *testing.T) {
	// Un-parsable bytes cannot be span-redacted; under a redact policy the
	// helper must produce the placeholder, never the original bytes. The
	// original spans are preserved for diagnosis (they carry offsets and
	// rule IDs, never matched content).
	raw := json.RawMessage(`not-json`)
	spans := []normalize.TransformSpan{{SourceID: "r-1", ContentAddress: "messages.0.content.0", Start: 0, End: 3}}
	got, gotSpans := ApplyStorageAction(raw, "redact", spans, []string{"r-1"}, nil)
	if strings.Contains(string(got), "not-json") {
		t.Fatalf("redact with un-parsable JSON must not persist the raw bytes, got %q", got)
	}
	var p normalize.NormalizedPayload
	if err := json.Unmarshal(got, &p); err != nil {
		t.Fatalf("placeholder unmarshal: %v", err)
	}
	if !p.Redacted {
		t.Errorf("want redacted placeholder, got %q", got)
	}
	if p.RedactedReason != normalize.RedactedReasonDegraded {
		t.Errorf("redactedReason = %q, want %q", p.RedactedReason, normalize.RedactedReasonDegraded)
	}
	if p.RedactedDetail == nil || p.RedactedDetail.Cause != normalize.DegradeCausePayloadUnmarshal {
		t.Errorf("redactedDetail = %+v, want cause %q", p.RedactedDetail, normalize.DegradeCausePayloadUnmarshal)
	}
	if len(gotSpans) != 1 || gotSpans[0].SourceID != "r-1" {
		t.Errorf("degradation must preserve the original spans, got %v", gotSpans)
	}
}

func TestApplyStorageAction_RedactUnresolvedSpanDegradesToPlaceholder(t *testing.T) {
	// A span addressing a content block that does not exist leaves matched
	// content in the patched payload. With no redetector available the
	// helper must degrade to the placeholder rather than persist a partial
	// redaction — stamped with cause "spans-unresolved" plus the failed
	// content addresses, and the original spans preserved for diagnosis.
	p := normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "leak@example.com"}}},
		},
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	spans := []normalize.TransformSpan{
		{SourceID: "pii-1", Action: normalize.ActionRedact, ContentAddress: "messages.9.content.9", Start: 0, End: 4, Replacement: "[X]"},
		{SourceID: "pii-1", Action: normalize.ActionRedact, ContentAddress: "messages.9.content.9", Start: 6, End: 8, Replacement: "[X]"},
		{Action: normalize.ActionRedact, ContentAddress: "", Start: 0, End: 1, Replacement: "[X]"},
	}
	got, gotSpans := ApplyStorageAction(raw, "redact", spans, []string{"pii-1"}, nil)
	if strings.Contains(string(got), "leak@example.com") {
		t.Fatalf("unresolved span must not persist matched content, got %q", got)
	}
	var decoded normalize.NormalizedPayload
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("placeholder unmarshal: %v", err)
	}
	if !decoded.Redacted {
		t.Errorf("want redacted placeholder, got %q", got)
	}
	if decoded.RedactedReason != normalize.RedactedReasonDegraded {
		t.Errorf("redactedReason = %q, want %q", decoded.RedactedReason, normalize.RedactedReasonDegraded)
	}
	if decoded.RedactedDetail == nil || decoded.RedactedDetail.Cause != normalize.DegradeCauseSpansUnresolved {
		t.Fatalf("redactedDetail = %+v, want cause %q", decoded.RedactedDetail, normalize.DegradeCauseSpansUnresolved)
	}
	// Duplicate addresses dedupe; the empty address is dropped; content
	// addresses only — never content.
	if len(decoded.RedactedDetail.FailedAddresses) != 1 || decoded.RedactedDetail.FailedAddresses[0] != "messages.9.content.9" {
		t.Errorf("failedAddresses = %v, want [messages.9.content.9]", decoded.RedactedDetail.FailedAddresses)
	}
	if len(gotSpans) != 3 {
		t.Errorf("degradation must preserve all original spans, got %v", gotSpans)
	}
}

func TestApplyStorageAction_OperatorDropStampsReason(t *testing.T) {
	raw := json.RawMessage(`{"kind":"ai-chat","messages":[{"role":"user","content":[{"type":"text","text":"sensitive"}]}]}`)
	got, spans := ApplyStorageAction(raw, "drop-content", nil, []string{"r-1"}, nil)
	var p normalize.NormalizedPayload
	if err := json.Unmarshal(got, &p); err != nil {
		t.Fatalf("placeholder unmarshal: %v", err)
	}
	if p.RedactedReason != normalize.RedactedReasonOperatorDrop {
		t.Errorf("redactedReason = %q, want %q", p.RedactedReason, normalize.RedactedReasonOperatorDrop)
	}
	if p.RedactedDetail != nil {
		t.Errorf("operator drop carries no degradation detail, got %+v", p.RedactedDetail)
	}
	if spans != nil {
		t.Errorf("drop-content returns no spans, got %v", spans)
	}
}

func TestApplyStorageAction_RedactAppliesSpans(t *testing.T) {
	p := normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{
			{
				Role: normalize.RoleUser,
				Content: []normalize.ContentBlock{
					{Type: normalize.ContentText, Text: "hello world"},
				},
			},
		},
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	spans := []normalize.TransformSpan{
		{
			Action:         normalize.ActionRedact,
			ContentAddress: "messages.0.content.0",
			Start:          0,
			End:            5,
			Replacement:    "[REDACTED]",
		},
	}
	got, gotSpans := ApplyStorageAction(raw, "redact", spans, nil, nil)
	if string(got) == string(raw) {
		t.Fatal("redact with valid spans should mutate the payload")
	}
	var decoded normalize.NormalizedPayload
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Messages) == 0 || len(decoded.Messages[0].Content) == 0 {
		t.Fatal("decoded payload missing message content")
	}
	gotText := decoded.Messages[0].Content[0].Text
	if !strings.Contains(gotText, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in redacted text, got %q", gotText)
	}
	// The returned spans must be relocated to the redacted text: "hello"
	// [0,5) → "[REDACTED]" at [0,10) in "[REDACTED] world".
	if len(gotSpans) != 1 {
		t.Fatalf("redact must return 1 post-redact span, got %d", len(gotSpans))
	}
	if gotSpans[0].Start != 0 || gotSpans[0].End != len("[REDACTED]") {
		t.Errorf("post-redact span = [%d,%d), want [0,%d)", gotSpans[0].Start, gotSpans[0].End, len("[REDACTED]"))
	}
}

func TestApplyStorageAction_RedactEmbeddingInputs(t *testing.T) {
	// Embedding payloads address content via the inputs.<i> grammar.
	p := normalize.NormalizedPayload{
		Kind:             normalize.KindAIEmbedding,
		NormalizeVersion: normalize.SchemaVersion,
		Inputs:           []string{"contact alice@example.com now"},
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	spans := []normalize.TransformSpan{
		{
			Action:         normalize.ActionRedact,
			ContentAddress: "inputs.0",
			Start:          8,
			End:            25,
			Replacement:    "[EMAIL-REDACTED]",
		},
	}
	got, gotSpans := ApplyStorageAction(raw, "redact", spans, nil, nil)
	if strings.Contains(string(got), "alice@example.com") {
		t.Fatalf("embedding input must be redacted, got %q", got)
	}
	var decoded normalize.NormalizedPayload
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Inputs) != 1 || !strings.Contains(decoded.Inputs[0], "[EMAIL-REDACTED]") {
		t.Errorf("inputs.0 = %q, want [EMAIL-REDACTED] marker", decoded.Inputs)
	}
	if len(gotSpans) != 1 || gotSpans[0].ContentAddress != "inputs.0" {
		t.Errorf("post-redact spans = %v, want one span at inputs.0", gotSpans)
	}
}

func TestApplyStorageAction_DropContentPlaceholder(t *testing.T) {
	// Valid input — placeholder preserves Kind/NormalizeVersion/Protocol
	// from the source payload.
	p := normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: "v2.0",
		Protocol:         "openai",
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "sensitive"}}},
		},
	}
	raw, _ := json.Marshal(p)
	ruleIDs := []string{"rule-1", "rule-2"}
	got, _ := ApplyStorageAction(raw, "drop-content", nil, ruleIDs, nil)
	var decoded normalize.NormalizedPayload
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !decoded.Redacted {
		t.Error("redacted=true should be set on drop-content")
	}
	if decoded.Kind != normalize.KindAIChat {
		t.Errorf("kind = %q, want ai-chat", decoded.Kind)
	}
	if decoded.NormalizeVersion != "v2.0" {
		t.Errorf("version = %q, want v2.0", decoded.NormalizeVersion)
	}
	if decoded.Protocol != "openai" {
		t.Errorf("protocol = %q, want openai", decoded.Protocol)
	}
	if len(decoded.RuleIDs) != 2 || decoded.RuleIDs[0] != "rule-1" {
		t.Errorf("ruleIds = %v, want %v", decoded.RuleIDs, ruleIDs)
	}
	if len(decoded.Messages) != 0 {
		t.Errorf("drop-content should strip messages, got %d", len(decoded.Messages))
	}
}

func TestApplyStorageAction_DropContent_BadJSONStillEmitsPlaceholder(t *testing.T) {
	// Unmarshal of raw fails → placeholder still produced with defaults
	// (Kind=ai-chat, NormalizeVersion=current schema version).
	raw := json.RawMessage(`{bad json}`)
	got, _ := ApplyStorageAction(raw, "drop-content", nil, []string{"r1"}, nil)
	var decoded normalize.NormalizedPayload
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("placeholder should be valid JSON even on bad input: %v", err)
	}
	if !decoded.Redacted {
		t.Error("redacted=true must always be stamped on drop-content path")
	}
	if decoded.Kind != normalize.KindAIChat {
		t.Errorf("default kind = %q, want ai-chat", decoded.Kind)
	}
	if decoded.NormalizeVersion != normalize.SchemaVersion {
		t.Errorf("default version = %q, want %q", decoded.NormalizeVersion, normalize.SchemaVersion)
	}
}

// piiRedetector builds a Redetector backed by real email/phone regexes —
// the storage-time re-detection stand-in for the hook pipeline's compiled
// patterns.
func piiRedetector() Redetector {
	patterns := map[string]*regexp.Regexp{
		"email": regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`),
		"phone": regexp.MustCompile(`\d{3}-\d{3}-\d{4}`),
	}
	return func(text string, ruleIDs []string) []Match {
		var out []Match
		for _, id := range ruleIDs {
			re, ok := patterns[id]
			if !ok {
				continue
			}
			for _, loc := range re.FindAllStringIndex(text, -1) {
				out = append(out, Match{RuleID: id, Start: loc[0], End: loc[1], Replacement: "[REDACTED_" + id + "]"})
			}
		}
		return out
	}
}

func TestApplyStorageAction_RedetectRedactsCrossFormatPayload(t *testing.T) {
	// Cross-format shape: hook-time spans carry the openai-projection
	// addresses (system + tool segments indexed as extra messages), while
	// the storage-time payload is anthropic-shaped and indexes the same
	// content lower. The addresses do not resolve — the old behavior was a
	// drop placeholder. New behavior: re-detect the failed rules' content
	// on the storage-time payload and store the REDACTED conversation.
	p := normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Protocol:         "anthropic",
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "reach me at alice@example.com please"}}},
			{Role: normalize.RoleAssistant, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "calling 555-123-4567 now"}}},
		},
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	spans := []normalize.TransformSpan{
		{Source: normalize.SourceHook, SourceID: "email", Action: normalize.ActionRedact, ContentAddress: "messages.2.content.0", Start: 12, End: 29, Replacement: "[REDACTED_email]"},
		{Source: normalize.SourceHook, SourceID: "phone", Action: normalize.ActionRedact, ContentAddress: "messages.3.content.0", Start: 8, End: 20, Replacement: "[REDACTED_phone]"},
	}
	got, gotSpans := ApplyStorageAction(raw, "redact", spans, []string{"email", "phone"}, piiRedetector())

	// The PII must be gone from the stored bytes and the markers present.
	if strings.Contains(string(got), "alice@example.com") || strings.Contains(string(got), "555-123-4567") {
		t.Fatalf("re-detected redaction must remove PII from stored bytes, got %q", got)
	}
	var decoded normalize.NormalizedPayload
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Redacted {
		t.Fatalf("re-detection succeeded — the stored copy must be the conversation, not a placeholder: %q", got)
	}
	if decoded.Kind != normalize.KindAIChat || len(decoded.Messages) != 2 {
		t.Fatalf("kind/messages must be preserved, got kind=%q messages=%d", decoded.Kind, len(decoded.Messages))
	}
	if !strings.Contains(decoded.Messages[0].Content[0].Text, "[REDACTED_email]") {
		t.Errorf("messages.0 = %q, want [REDACTED_email] marker", decoded.Messages[0].Content[0].Text)
	}
	if !strings.Contains(decoded.Messages[1].Content[0].Text, "[REDACTED_phone]") {
		t.Errorf("messages.1 = %q, want [REDACTED_phone] marker", decoded.Messages[1].Content[0].Text)
	}
	// Spans are relocated to the storage-time addresses so the UI badges
	// land on the markers.
	if len(gotSpans) != 2 {
		t.Fatalf("want 2 relocated spans, got %v", gotSpans)
	}
	addrs := map[string]bool{}
	for _, s := range gotSpans {
		addrs[s.ContentAddress] = true
	}
	if !addrs["messages.0.content.0"] || !addrs["messages.1.content.0"] {
		t.Errorf("relocated span addresses = %v, want storage-time addresses", gotSpans)
	}
}

func TestApplyStorageAction_RedetectFindsNothingDegrades(t *testing.T) {
	// Re-detection that cannot re-locate a failed rule's content must NOT
	// store the payload — degrade with the Layer-1 diagnosis instead.
	p := normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "no detectable content here"}}},
		},
	}
	raw, _ := json.Marshal(p)
	spans := []normalize.TransformSpan{
		{Source: normalize.SourceHook, SourceID: "email", Action: normalize.ActionRedact, ContentAddress: "messages.5.content.0", Start: 0, End: 5, Replacement: "[REDACTED_email]"},
	}
	got, gotSpans := ApplyStorageAction(raw, "redact", spans, []string{"email"}, piiRedetector())
	var decoded normalize.NormalizedPayload
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("placeholder unmarshal: %v", err)
	}
	if !decoded.Redacted || decoded.RedactedReason != normalize.RedactedReasonDegraded {
		t.Fatalf("want degraded placeholder, got %q", got)
	}
	if decoded.RedactedDetail == nil || decoded.RedactedDetail.Cause != normalize.DegradeCauseSpansUnresolved {
		t.Errorf("redactedDetail = %+v, want cause %q", decoded.RedactedDetail, normalize.DegradeCauseSpansUnresolved)
	}
	if len(decoded.RedactedDetail.FailedAddresses) != 1 || decoded.RedactedDetail.FailedAddresses[0] != "messages.5.content.0" {
		t.Errorf("failedAddresses = %v, want [messages.5.content.0]", decoded.RedactedDetail.FailedAddresses)
	}
	if len(gotSpans) != 1 || gotSpans[0].SourceID != "email" {
		t.Errorf("degradation must preserve the original spans, got %v", gotSpans)
	}
}

func TestApplyStorageAction_RedetectUnattributedSkipDegrades(t *testing.T) {
	// Skipped spans without a SourceID cannot be re-detected — there is no
	// rule to re-run. Degrade even though a redetector is available.
	p := normalize.NormalizedPayload{
		Kind:     normalize.KindAIChat,
		Messages: []normalize.Message{{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "x alice@example.com"}}}},
	}
	raw, _ := json.Marshal(p)
	spans := []normalize.TransformSpan{
		{Action: normalize.ActionRedact, ContentAddress: "", Start: 0, End: 1, Replacement: "[X]"},
	}
	got, _ := ApplyStorageAction(raw, "redact", spans, nil, piiRedetector())
	var decoded normalize.NormalizedPayload
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("placeholder unmarshal: %v", err)
	}
	if !decoded.Redacted || decoded.RedactedDetail == nil || decoded.RedactedDetail.Cause != normalize.DegradeCauseSpansUnresolved {
		t.Errorf("unattributed skipped span must degrade, got %q", got)
	}
	// A skipped span with no content address contributes nothing to the
	// failed-address list — the field stays absent rather than [""].
	if decoded.RedactedDetail != nil && decoded.RedactedDetail.FailedAddresses != nil {
		t.Errorf("failedAddresses = %v, want absent", decoded.RedactedDetail.FailedAddresses)
	}
}

func TestApplyStorageAction_RedetectSkipsAlreadyRedactedRanges(t *testing.T) {
	// One span of the rule resolved (the first occurrence), one did not.
	// Re-detection re-finds BOTH occurrences; the one overlapping the
	// already-resolved span must not double-redact, and a match without a
	// Replacement falls back to the [REDACTED_<RULE_ID>] marker. Invalid
	// and duplicate redetector output is ignored.
	p := normalize.NormalizedPayload{
		Kind: normalize.KindAIChat,
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "email a@b.co and a@b.co again"}}},
		},
	}
	raw, _ := json.Marshal(p)
	spans := []normalize.TransformSpan{
		// Resolves: first "a@b.co" at [6,12).
		{Source: normalize.SourceHook, SourceID: "email", Action: normalize.ActionRedact, ContentAddress: "messages.0.content.0", Start: 6, End: 12, Replacement: "[REDACTED_email]"},
		// Does not resolve: hook-time projection indexed a second message.
		{Source: normalize.SourceHook, SourceID: "email", Action: normalize.ActionRedact, ContentAddress: "messages.1.content.0", Start: 0, End: 6, Replacement: "[REDACTED_email]"},
	}
	redetect := func(text string, ruleIDs []string) []Match {
		re := regexp.MustCompile(`[a-z]@b\.co`)
		var out []Match
		for _, loc := range re.FindAllStringIndex(text, -1) {
			// Replacement intentionally empty → default marker.
			out = append(out, Match{RuleID: "email", Start: loc[0], End: loc[1]})
			out = append(out, Match{RuleID: "email", Start: loc[0], End: loc[1]}) // duplicate — ignored
		}
		// Garbage the helper must ignore.
		out = append(out,
			Match{RuleID: "", Start: 0, End: 2, Replacement: "[X]"},
			Match{RuleID: "email", Start: -1, End: 2, Replacement: "[X]"},
			Match{RuleID: "email", Start: 0, End: len(text) + 5, Replacement: "[X]"},
			Match{RuleID: "email", Start: 3, End: 3, Replacement: "[X]"},
		)
		return out
	}
	got, gotSpans := ApplyStorageAction(raw, "redact", spans, []string{"email"}, redetect)
	if strings.Contains(string(got), "a@b.co") {
		t.Fatalf("all PII occurrences must be redacted, got %q", got)
	}
	var decoded normalize.NormalizedPayload
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Redacted {
		t.Fatalf("re-detection succeeded — want redacted conversation, got placeholder %q", got)
	}
	text := decoded.Messages[0].Content[0].Text
	// First occurrence: the resolvable span's explicit marker. Second
	// occurrence: the re-detected match had no Replacement, so the default
	// [REDACTED_<RULE_ID>] template (uppercased) applies.
	if strings.Count(text, "[REDACTED_email]") != 1 || strings.Count(text, "[REDACTED_EMAIL]") != 1 {
		t.Errorf("want one explicit and one default marker (no double-redaction), got %q", text)
	}
	if len(gotSpans) != 2 {
		t.Errorf("want 2 relocated spans (resolvable + re-detected), got %v", gotSpans)
	}
}

func TestApplyStorageAction_RedetectPartialOverlapRedactsRemainder(t *testing.T) {
	// A re-detected match PARTIALLY overlapping an already-resolved span
	// must not be suppressed wholesale: the uncovered remainder bytes are
	// matched sensitive content and would otherwise persist unredacted
	// whenever the rule's coverage is satisfied by another occurrence.
	// Here the resolved span covers only the head of the first phone
	// occurrence ("555-123-"); a second clean occurrence keeps the rule
	// covered. The trailing fragment "4567" must be GONE from the stored
	// bytes.
	p := normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "a 555-123-4567 b 555-987-6543 end"}}},
		},
	}
	raw, _ := json.Marshal(p)
	spans := []normalize.TransformSpan{
		// Resolves: head of the first occurrence, [2,10) = "555-123-".
		{Source: normalize.SourceHook, SourceID: "phone", Action: normalize.ActionRedact, ContentAddress: "messages.0.content.0", Start: 2, End: 10, Replacement: "[REDACTED_phone]"},
		// Does not resolve → triggers re-detection for "phone".
		{Source: normalize.SourceHook, SourceID: "phone", Action: normalize.ActionRedact, ContentAddress: "messages.9.content.0", Start: 0, End: 12, Replacement: "[REDACTED_phone]"},
	}
	got, gotSpans := ApplyStorageAction(raw, "redact", spans, []string{"phone"}, piiRedetector())
	if strings.Contains(string(got), "4567") {
		t.Fatalf("partial-overlap remainder bytes must be redacted, got %q", got)
	}
	if strings.Contains(string(got), "555") || strings.Contains(string(got), "6543") {
		t.Fatalf("no phone bytes may survive, got %q", got)
	}
	var decoded normalize.NormalizedPayload
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Redacted {
		t.Fatalf("re-detection covers the rule — want redacted conversation, got placeholder %q", got)
	}
	// Resolvable head + re-detected remainder + clean second occurrence.
	if len(gotSpans) != 3 {
		t.Errorf("want 3 relocated spans, got %v", gotSpans)
	}
}

func TestApplyStorageAction_RedetectMatchContainingResolvedSpanRedactsBothSides(t *testing.T) {
	// A re-detected match strictly CONTAINING an already-resolved span is
	// trimmed to the uncovered left + right remainders, each redacted with
	// the match's marker. Handing the overlapping range to ApplySpans
	// as-is would break its disjoint-span assumption: the resolved span's
	// replacement marker is longer than the contained range, so the
	// lower-start overlapping span's range would end inside the marker and
	// leave the match's trailing original bytes in place.
	p := normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "p 555-123-4567 q"}}},
		},
	}
	raw, _ := json.Marshal(p)
	spans := []normalize.TransformSpan{
		// Resolves: mid-section of the phone, [5,9) = "-123", with a
		// marker longer than the match's tail beyond it.
		{Source: normalize.SourceHook, SourceID: "frag", Action: normalize.ActionRedact, ContentAddress: "messages.0.content.0", Start: 5, End: 9, Replacement: "[REDACTED_FRAGMENT_MARKER]"},
		// Does not resolve → triggers re-detection for "phone", whose
		// match [2,14) contains the resolved [5,9).
		{Source: normalize.SourceHook, SourceID: "phone", Action: normalize.ActionRedact, ContentAddress: "messages.9.content.0", Start: 0, End: 12, Replacement: "[REDACTED_phone]"},
	}
	got, gotSpans := ApplyStorageAction(raw, "redact", spans, []string{"phone"}, piiRedetector())
	if strings.Contains(string(got), "555") || strings.Contains(string(got), "4567") {
		t.Fatalf("both remainder fragments must be redacted, got %q", got)
	}
	var decoded normalize.NormalizedPayload
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Redacted {
		t.Fatalf("re-detection covers the rule — want redacted conversation, got placeholder %q", got)
	}
	text := decoded.Messages[0].Content[0].Text
	if strings.Count(text, "[REDACTED_phone]") != 2 || strings.Count(text, "[REDACTED_FRAGMENT_MARKER]") != 1 {
		t.Errorf("want phone markers on both remainders around the fragment marker, got %q", text)
	}
	// The relocated spans must bracket their replacements exactly — the
	// disjoint-span trim is what keeps AppliedSpanOffsets correct here.
	if len(gotSpans) != 3 {
		t.Fatalf("want 3 relocated spans, got %v", gotSpans)
	}
	for _, s := range gotSpans {
		if text[s.Start:s.End] != s.Replacement {
			t.Errorf("span %v does not bracket its replacement in %q", s, text)
		}
	}
}

func TestApplyStorageAction_RedetectContainedMatchCountsAsCovered(t *testing.T) {
	// A failed rule whose ONLY occurrence lies fully inside an
	// already-resolved span must not degrade: those bytes are replaced by
	// the resolved span, so the stored copy is provably clean for the
	// rule. No second marker is emitted (no double-redaction).
	p := normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "contact alice@example.com now"}}},
		},
	}
	raw, _ := json.Marshal(p)
	spans := []normalize.TransformSpan{
		// Resolves: a broader rule already replaces the whole email.
		{Source: normalize.SourceHook, SourceID: "pii", Action: normalize.ActionRedact, ContentAddress: "messages.0.content.0", Start: 8, End: 25, Replacement: "[PII]"},
		// Does not resolve → triggers re-detection for "email", whose only
		// match coincides with the resolved range.
		{Source: normalize.SourceHook, SourceID: "email", Action: normalize.ActionRedact, ContentAddress: "messages.9.content.0", Start: 0, End: 17, Replacement: "[REDACTED_email]"},
	}
	got, gotSpans := ApplyStorageAction(raw, "redact", spans, []string{"pii", "email"}, piiRedetector())
	if strings.Contains(string(got), "alice@example.com") {
		t.Fatalf("email must be redacted by the containing span, got %q", got)
	}
	var decoded normalize.NormalizedPayload
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Redacted {
		t.Fatalf("contained match covers the failed rule — must not degrade, got %q", got)
	}
	text := decoded.Messages[0].Content[0].Text
	if text != "contact [PII] now" {
		t.Errorf("want single containing marker without double-redaction, got %q", text)
	}
	if len(gotSpans) != 1 || gotSpans[0].SourceID != "pii" {
		t.Errorf("want only the resolvable span relocated, got %v", gotSpans)
	}
}

func TestApplyStorageAction_RedetectOccupiedRangesClampLikeApply(t *testing.T) {
	// The occupied-range projection must clamp out-of-bounds resolvable
	// spans exactly the way span application clamps them — a span that
	// blanket-replaced a whole block (negative start, end past the text)
	// covers every re-detected match in that block, and spans addressed at
	// OTHER blocks must not suppress matches here.
	p := normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "see a@b.co ok"}}},
			{Role: normalize.RoleAssistant, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "call 555-123-4567"}}},
		},
	}
	raw, _ := json.Marshal(p)
	spans := []normalize.TransformSpan{
		// Resolves with clamping: replaces all of messages.0.content.0.
		{Source: normalize.SourceHook, SourceID: "wide", Action: normalize.ActionRedact, ContentAddress: "messages.0.content.0", Start: -3, End: 100, Replacement: "[ALL]"},
		// Neither resolves → re-detection for "email" (only occurrence
		// inside the blanket-replaced block) and "phone" (occurrence in the
		// second block, untouched by the messages.0 span).
		{Source: normalize.SourceHook, SourceID: "email", Action: normalize.ActionRedact, ContentAddress: "messages.7.content.0", Start: 0, End: 6, Replacement: "[REDACTED_email]"},
		{Source: normalize.SourceHook, SourceID: "phone", Action: normalize.ActionRedact, ContentAddress: "messages.8.content.0", Start: 0, End: 12, Replacement: "[REDACTED_phone]"},
	}
	got, _ := ApplyStorageAction(raw, "redact", spans, []string{"wide", "email", "phone"}, piiRedetector())
	if strings.Contains(string(got), "a@b.co") || strings.Contains(string(got), "555") {
		t.Fatalf("no sensitive bytes may survive, got %q", got)
	}
	var decoded normalize.NormalizedPayload
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Redacted {
		t.Fatalf("email is covered by the blanket span and phone re-detects — must not degrade, got %q", got)
	}
	if decoded.Messages[0].Content[0].Text != "[ALL]" {
		t.Errorf("messages.0 = %q, want blanket marker only (covered match emits nothing)", decoded.Messages[0].Content[0].Text)
	}
	if !strings.Contains(decoded.Messages[1].Content[0].Text, "[REDACTED_phone]") {
		t.Errorf("messages.1 = %q, want re-detected phone marker", decoded.Messages[1].Content[0].Text)
	}
}

func TestApplyStorageAction_RedetectWalksAllTextBlocks(t *testing.T) {
	// Re-detection must scan every text-bearing block shape: tool_result
	// output, reasoning blocks, embedding inputs, and HTTP body views
	// (text + form, in deterministic key order). Blocks with empty text
	// and nil toolResult pointers are skipped.
	redetect := piiRedetector()
	cases := []struct {
		name     string
		payload  normalize.NormalizedPayload
		skipAddr string
		rule     string
		wantAddr string
		leak     string
	}{
		{
			name: "tool result output",
			payload: normalize.NormalizedPayload{
				Kind: normalize.KindAIChat,
				Messages: []normalize.Message{
					{Role: normalize.RoleTool, Content: []normalize.ContentBlock{
						{Type: normalize.ContentToolResult},     // nil toolResult — skipped
						{Type: normalize.ContentText, Text: ""}, // empty — skipped
						{Type: normalize.ContentToolResult, ToolResult: &normalize.ToolResult{Output: "user mail bob@corp.io"}}, //nolint:lll
					}},
				},
			},
			skipAddr: "messages.7.content.0.toolResult",
			rule:     "email",
			wantAddr: "messages.0.content.2.toolResult",
			leak:     "bob@corp.io",
		},
		{
			name: "reasoning block",
			payload: normalize.NormalizedPayload{
				Kind: normalize.KindAIChat,
				Messages: []normalize.Message{
					{Role: normalize.RoleAssistant, Content: []normalize.ContentBlock{
						{Type: normalize.ContentReasoning, Text: "thinking about bob@corp.io"},
					}},
				},
			},
			skipAddr: "messages.3.content.0",
			rule:     "email",
			wantAddr: "messages.0.content.0",
			leak:     "bob@corp.io",
		},
		{
			name: "embedding inputs",
			payload: normalize.NormalizedPayload{
				Kind:   normalize.KindAIEmbedding,
				Inputs: []string{"call 555-123-4567"},
			},
			skipAddr: "inputs.9",
			rule:     "phone",
			wantAddr: "inputs.0",
			leak:     "555-123-4567",
		},
		{
			name: "http body view text",
			payload: normalize.NormalizedPayload{
				Kind: normalize.KindHTTPText,
				HTTP: &normalize.HTTPPayload{BodyView: &normalize.HTTPBodyView{Text: "to bob@corp.io"}},
			},
			skipAddr: "messages.0.content.0",
			rule:     "email",
			wantAddr: "http.bodyView",
			leak:     "bob@corp.io",
		},
		{
			name: "http form field",
			payload: normalize.NormalizedPayload{
				Kind: normalize.KindHTTPForm,
				HTTP: &normalize.HTTPPayload{BodyView: &normalize.HTTPBodyView{Form: map[string]string{
					"aa": "clean",
					"zz": "mail bob@corp.io",
				}}},
			},
			skipAddr: "http.bodyView.form.missing",
			rule:     "email",
			wantAddr: "http.bodyView.form.zz",
			leak:     "bob@corp.io",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			spans := []normalize.TransformSpan{
				{Source: normalize.SourceHook, SourceID: tc.rule, Action: normalize.ActionRedact, ContentAddress: tc.skipAddr, Start: 0, End: 4, Replacement: "[R]"},
			}
			got, gotSpans := ApplyStorageAction(raw, "redact", spans, []string{tc.rule}, redetect)
			if strings.Contains(string(got), tc.leak) {
				t.Fatalf("PII must be gone from stored bytes, got %q", got)
			}
			var decoded normalize.NormalizedPayload
			if err := json.Unmarshal(got, &decoded); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if decoded.Redacted {
				t.Fatalf("want redacted content, got placeholder %q", got)
			}
			if !strings.Contains(string(got), "[REDACTED_"+tc.rule+"]") {
				t.Errorf("want [REDACTED_%s] marker in %q", tc.rule, got)
			}
			found := false
			for _, s := range gotSpans {
				if s.ContentAddress == tc.wantAddr {
					found = true
				}
			}
			if !found {
				t.Errorf("relocated spans %v missing address %q", gotSpans, tc.wantAddr)
			}
		})
	}
}

func TestStorageRawBody(t *testing.T) {
	captured := []byte(`{"messages":[{"content":"leak@example.com"}]}`)
	redacted := []byte(`{"messages":[{"content":"[REDACTED]"}]}`)
	cases := []struct {
		name     string
		captured []byte
		action   string
		redacted []byte
		want     []byte
	}{
		{"empty action keeps captured", captured, "", redacted, captured},
		{"keep keeps captured", captured, "keep", redacted, captured},
		{"redact uses only the redacted copy", captured, "redact", redacted, redacted},
		{"redact with no redacted copy drops the raw body", captured, "redact", nil, nil},
		{"drop-content never stores raw bytes", captured, "drop-content", redacted, nil},
		{"unknown action fails closed", captured, "bogus", redacted, nil},
		// Capture disabled (or bodyless request): the storage policy must
		// never resurrect bytes the capture config chose not to store.
		{"nil captured stays nil under keep", nil, "keep", redacted, nil},
		{"nil captured stays nil under redact", nil, "redact", redacted, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StorageRawBody(tc.captured, tc.redacted, tc.action)
			if string(got) != string(tc.want) {
				t.Errorf("StorageRawBody(%q) = %q, want %q", tc.action, got, tc.want)
			}
		})
	}
}

func TestMarshalSpans(t *testing.T) {
	if got := MarshalSpans(nil); got != nil {
		t.Errorf("nil spans → nil, got %q", got)
	}
	if got := MarshalSpans([]normalize.TransformSpan{}); got != nil {
		t.Errorf("empty spans → nil, got %q", got)
	}
	spans := []normalize.TransformSpan{
		{Action: normalize.ActionRedact, ContentAddress: "messages.0.content.0", Start: 1, End: 4, Replacement: "[X]", SourceID: "r-1"},
	}
	got := MarshalSpans(spans)
	var decoded []normalize.TransformSpan
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if len(decoded) != 1 || decoded[0].SourceID != "r-1" || decoded[0].End != 4 {
		t.Errorf("round-trip = %+v, want original span", decoded)
	}
}

// failMarshal swaps the marshal seam for one that always errors and
// returns a restore function. Asserts the package-wide fail-safe: a
// marshal failure stores nothing, never the original bytes.
func failMarshal(t *testing.T) {
	t.Helper()
	orig := marshalJSON
	marshalJSON = func(any) ([]byte, error) { return nil, errMarshalBoom{} }
	t.Cleanup(func() { marshalJSON = orig })
}

// failMarshalOnce makes only the FIRST marshal call fail, so the
// degradation placeholder produced afterwards still marshals.
func failMarshalOnce(t *testing.T) {
	t.Helper()
	orig := marshalJSON
	calls := 0
	marshalJSON = func(v any) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, errMarshalBoom{}
		}
		return json.Marshal(v)
	}
	t.Cleanup(func() { marshalJSON = orig })
}

type errMarshalBoom struct{}

func (errMarshalBoom) Error() string { return "marshal failed" }

func TestApplyStorageAction_RedactMarshalFailureDegradesToNil(t *testing.T) {
	// Both the patched-payload marshal AND the placeholder marshal fail:
	// the result must be nil (SQL NULL), never the matched content. The
	// diagnostic spans survive — they carry no content.
	failMarshal(t)
	p := normalize.NormalizedPayload{
		Kind: normalize.KindAIChat,
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "leak@example.com"}}},
		},
	}
	raw, _ := json.Marshal(p)
	spans := []normalize.TransformSpan{
		{SourceID: "r-1", Action: normalize.ActionRedact, ContentAddress: "messages.0.content.0", Start: 0, End: 4, Replacement: "[X]"},
	}
	got, gotSpans := ApplyStorageAction(raw, "redact", spans, []string{"r-1"}, nil)
	if got != nil {
		t.Errorf("marshal failure must store nothing, got %q", got)
	}
	if len(gotSpans) != 1 {
		t.Errorf("degradation preserves the original spans, got %v", gotSpans)
	}
}

func TestApplyStorageAction_RedactMarshalFailureStampsCause(t *testing.T) {
	// Only the patched-payload marshal fails; the placeholder must carry
	// cause "marshal-failed".
	failMarshalOnce(t)
	p := normalize.NormalizedPayload{
		Kind: normalize.KindAIChat,
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "leak@example.com"}}},
		},
	}
	raw, _ := json.Marshal(p)
	spans := []normalize.TransformSpan{
		{SourceID: "r-1", Action: normalize.ActionRedact, ContentAddress: "messages.0.content.0", Start: 0, End: 4, Replacement: "[X]"},
	}
	got, _ := ApplyStorageAction(raw, "redact", spans, []string{"r-1"}, nil)
	var decoded normalize.NormalizedPayload
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("placeholder unmarshal: %v", err)
	}
	if strings.Contains(string(got), "leak@example.com") {
		t.Fatalf("marshal failure must not persist content, got %q", got)
	}
	if decoded.RedactedDetail == nil || decoded.RedactedDetail.Cause != normalize.DegradeCauseMarshalFailed {
		t.Errorf("redactedDetail = %+v, want cause %q", decoded.RedactedDetail, normalize.DegradeCauseMarshalFailed)
	}
}

func TestApplyStorageAction_RedetectMarshalFailureDegrades(t *testing.T) {
	// The re-detected payload's marshal fails → the redetect path reports
	// failure and the caller degrades (placeholder marshals fine on the
	// second call).
	failMarshalOnce(t)
	p := normalize.NormalizedPayload{
		Kind: normalize.KindAIChat,
		Messages: []normalize.Message{
			{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: "mail alice@example.com"}}},
		},
	}
	raw, _ := json.Marshal(p)
	spans := []normalize.TransformSpan{
		{Source: normalize.SourceHook, SourceID: "email", Action: normalize.ActionRedact, ContentAddress: "messages.6.content.0", Start: 0, End: 5, Replacement: "[REDACTED_email]"},
	}
	got, _ := ApplyStorageAction(raw, "redact", spans, []string{"email"}, piiRedetector())
	if strings.Contains(string(got), "alice@example.com") {
		t.Fatalf("must not persist content on marshal failure, got %q", got)
	}
	var decoded normalize.NormalizedPayload
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("placeholder unmarshal: %v", err)
	}
	if !decoded.Redacted || decoded.RedactedReason != normalize.RedactedReasonDegraded {
		t.Errorf("want degraded placeholder, got %q", got)
	}
}

func TestApplyStorageAction_ReportsStorageOutcomes(t *testing.T) {
	// Every degradation and every redetect rescue must surface exactly one
	// outcome event with its cause; clean paths (keep, drop-content, a
	// redaction whose spans all resolve) emit nothing.
	type event struct{ outcome, cause string }
	chatPayload := func(text string) json.RawMessage {
		raw, err := json.Marshal(normalize.NormalizedPayload{
			Kind: normalize.KindAIChat,
			Messages: []normalize.Message{
				{Role: normalize.RoleUser, Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: text}}},
			},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return raw
	}
	resolvingSpan := normalize.TransformSpan{Source: normalize.SourceHook, SourceID: "email", Action: normalize.ActionRedact, ContentAddress: "messages.0.content.0", Start: 0, End: 4, Replacement: "[X]"}
	failingSpan := normalize.TransformSpan{Source: normalize.SourceHook, SourceID: "email", Action: normalize.ActionRedact, ContentAddress: "messages.6.content.0", Start: 0, End: 5, Replacement: "[REDACTED_email]"}

	cases := []struct {
		name string
		run  func(t *testing.T)
		want []event
	}{
		{
			name: "keep, drop-content and fully-resolved redact emit nothing",
			run: func(t *testing.T) {
				ApplyStorageAction(chatPayload("mail alice@example.com"), "keep", nil, nil, nil)
				ApplyStorageAction(chatPayload("mail alice@example.com"), "drop-content", nil, []string{"email"}, nil)
				ApplyStorageAction(chatPayload("mail alice@example.com"), "redact", []normalize.TransformSpan{resolvingSpan}, []string{"email"}, nil)
			},
			want: nil,
		},
		{
			name: "redact without spans degrades with no-spans",
			run: func(t *testing.T) {
				ApplyStorageAction(chatPayload("x"), "redact", nil, []string{"keyword"}, nil)
			},
			want: []event{{StorageOutcomeDegraded, normalize.DegradeCauseNoSpans}},
		},
		{
			name: "unparsable payload degrades with payload-unmarshal",
			run: func(t *testing.T) {
				ApplyStorageAction(json.RawMessage(`{not json`), "redact", []normalize.TransformSpan{resolvingSpan}, []string{"email"}, nil)
			},
			want: []event{{StorageOutcomeDegraded, normalize.DegradeCausePayloadUnmarshal}},
		},
		{
			name: "unresolved span without redetector degrades with spans-unresolved",
			run: func(t *testing.T) {
				ApplyStorageAction(chatPayload("no detectable content"), "redact", []normalize.TransformSpan{failingSpan}, []string{"email"}, nil)
			},
			want: []event{{StorageOutcomeDegraded, normalize.DegradeCauseSpansUnresolved}},
		},
		{
			name: "patched-payload marshal failure degrades with marshal-failed",
			run: func(t *testing.T) {
				failMarshalOnce(t)
				ApplyStorageAction(chatPayload("mail alice@example.com"), "redact", []normalize.TransformSpan{resolvingSpan}, []string{"email"}, nil)
			},
			want: []event{{StorageOutcomeDegraded, normalize.DegradeCauseMarshalFailed}},
		},
		{
			name: "redetect rescue reports rescued with the recovered cause",
			run: func(t *testing.T) {
				ApplyStorageAction(chatPayload("mail alice@example.com"), "redact", []normalize.TransformSpan{failingSpan}, []string{"email"}, piiRedetector())
			},
			want: []event{{StorageOutcomeRescued, normalize.DegradeCauseSpansUnresolved}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var events []event
			OnStorageOutcome = func(outcome, cause string) { events = append(events, event{outcome, cause}) }
			t.Cleanup(func() { OnStorageOutcome = nil })
			tc.run(t)
			if len(events) != len(tc.want) {
				t.Fatalf("events = %v, want %v", events, tc.want)
			}
			for i := range tc.want {
				if events[i] != tc.want[i] {
					t.Errorf("event[%d] = %v, want %v", i, events[i], tc.want[i])
				}
			}
		})
	}
}

func TestDropContentPlaceholder_MarshalFailureStoresNothing(t *testing.T) {
	failMarshal(t)
	raw := json.RawMessage(`{"kind":"ai-chat"}`)
	if got, _ := ApplyStorageAction(raw, "drop-content", nil, []string{"r-1"}, nil); got != nil {
		t.Errorf("placeholder marshal failure must yield nil, got %q", got)
	}
}

func TestMarshalSpans_MarshalFailureYieldsNil(t *testing.T) {
	failMarshal(t)
	spans := []normalize.TransformSpan{{SourceID: "r-1"}}
	if got := MarshalSpans(spans); got != nil {
		t.Errorf("marshal failure must yield nil, got %q", got)
	}
}

func TestCollectRuleIDs(t *testing.T) {
	if got := CollectRuleIDs(nil); got != nil {
		t.Errorf("nil spans → nil, got %v", got)
	}
	spans := []normalize.TransformSpan{
		{SourceID: "pii-email"},
		{SourceID: ""},          // no attribution — skipped
		{SourceID: "pii-email"}, // duplicate — deduped
		{SourceID: "pii-phone"},
	}
	got := CollectRuleIDs(spans)
	if len(got) != 2 || got[0] != "pii-email" || got[1] != "pii-phone" {
		t.Errorf("CollectRuleIDs = %v, want [pii-email pii-phone]", got)
	}
}
