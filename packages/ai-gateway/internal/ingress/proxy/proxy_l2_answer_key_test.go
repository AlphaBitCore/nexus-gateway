package proxy

import (
	"testing"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func f64(v float64) *float64 { return &v }
func iptr(v int) *int        { return &v }

func chatPayload(p *normcore.SamplingParam, tools ...normcore.ToolDef) *normcore.NormalizedPayload {
	return &normcore.NormalizedPayload{
		Kind:   normcore.KindAIChat,
		Params: p,
		Tools:  tools,
		Messages: []normcore.Message{{
			Role:    normcore.RoleUser,
			Content: []normcore.ContentBlock{{Type: normcore.ContentText, Text: "what is the capital of France"}},
		}},
	}
}

// The defect: identical text with different sampling settings shared one L2
// entry, so a caller who pinned temperature=0 for determinism could be served
// an answer generated at temperature=2.
func TestAnswerKey_TemperatureSeparatesOtherwiseIdenticalRequests(t *testing.T) {
	deterministic := answerKey(chatPayload(&normcore.SamplingParam{Temperature: f64(0)}))
	creative := answerKey(chatPayload(&normcore.SamplingParam{Temperature: f64(2)}))

	if deterministic == "" || creative == "" {
		t.Fatalf("a sampling parameter must produce a key: %q / %q", deterministic, creative)
	}
	if deterministic == creative {
		t.Error("temperature=0 and temperature=2 share a key; the deterministic caller can be served a sampled answer")
	}
}

// Every parameter in the canonical's sampling set changes the answer, so each
// must move the key on its own. A table here rather than one case per field
// because the failure is identical in shape and the point is coverage of the
// whole set.
func TestAnswerKey_EverySamplingFieldMovesTheKey(t *testing.T) {
	base := answerKey(chatPayload(&normcore.SamplingParam{Temperature: f64(0.7)}))
	for name, params := range map[string]*normcore.SamplingParam{
		"top_p":      {Temperature: f64(0.7), TopP: f64(0.5)},
		"top_k":      {Temperature: f64(0.7), TopK: iptr(40)},
		"max_tokens": {Temperature: f64(0.7), MaxTokens: iptr(256)},
		"stop":       {Temperature: f64(0.7), Stop: []string{"\n\n"}},
	} {
		if got := answerKey(chatPayload(params)); got == base {
			t.Errorf("adding %s did not change the key; two requests that answer differently share an entry", name)
		}
	}
}

// A tool declaration changes the answer even when no tool is invoked — the
// model answers differently knowing the capability exists.
func TestAnswerKey_ToolDeclarationsSeparate(t *testing.T) {
	none := answerKey(chatPayload(nil))
	withTool := answerKey(chatPayload(nil, normcore.ToolDef{
		Name:                 "get_weather",
		ParametersJSONSchema: map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
	}))
	if none == withTool {
		t.Error("declaring a tool did not change the key")
	}

	// Same tool name, different schema, is a different capability.
	otherSchema := answerKey(chatPayload(nil, normcore.ToolDef{
		Name:                 "get_weather",
		ParametersJSONSchema: map[string]any{"type": "object", "properties": map[string]any{"lat": map[string]any{"type": "number"}}},
	}))
	if withTool == otherSchema {
		t.Error("same tool name with a different parameter schema shares a key")
	}
}

// Go randomizes map iteration. A key that varied between processes would make
// every entry unreachable by the next request that wrote it, so the digest
// must be byte-stable for the same input.
func TestAnswerKey_IsStableAcrossRepeatedComputation(t *testing.T) {
	payload := chatPayload(
		&normcore.SamplingParam{Temperature: f64(0.7), Stop: []string{"a", "b"}},
		normcore.ToolDef{Name: "t", ParametersJSONSchema: map[string]any{
			"z": 1, "a": 2, "m": 3, "q": 4, "b": 5, "y": 6, "c": 7, "x": 8,
		}},
	)
	first := answerKey(payload)
	for i := range 64 {
		if got := answerKey(payload); got != first {
			t.Fatalf("iteration %d produced %q, want the stable %q — map ordering leaked into the digest", i, got, first)
		}
	}
}

// The overwhelming majority of traffic sends no sampling parameters. Those
// requests must keep the empty key so their validity tag is unchanged and
// entries written before this existed stay reachable — a fix that cold-started
// the whole cache would cost more than the bug.
func TestAnswerKey_NoParameters_IsEmpty(t *testing.T) {
	if got := answerKey(chatPayload(nil)); got != "" {
		t.Errorf("key=%q, want empty so default-parameter traffic keeps reaching existing entries", got)
	}
	if got := answerKey(chatPayload(&normcore.SamplingParam{})); got != "" {
		t.Errorf("key=%q for an all-nil SamplingParam, want empty", got)
	}
	if got := answerKey(nil); got != "" {
		t.Errorf("key=%q for a nil payload, want empty", got)
	}
}

// The write and read paths must agree by construction, not by two matching
// derivations. l2CanonicalFrom is the single seam; this pins that it returns
// exactly what answerKey computes for the same payload.
func TestL2CanonicalFrom_CarriesTheSameKeyAsTheReadPath(t *testing.T) {
	payload := chatPayload(&normcore.SamplingParam{Temperature: f64(0.3), MaxTokens: iptr(512)})

	writeSide := l2CanonicalFrom(payload)
	readSide := answerKey(payload)

	if writeSide.answerKey != readSide {
		t.Errorf("write key %q != read key %q; the writer would store entries the reader can never find",
			writeSide.answerKey, readSide)
	}
	if len(writeSide.msgs) != len(payload.Messages) {
		t.Errorf("messages len=%d want %d", len(writeSide.msgs), len(payload.Messages))
	}

	if empty := l2CanonicalFrom(nil); empty.answerKey != "" || empty.msgs != nil {
		t.Errorf("nil payload must yield the zero value, got %+v", empty)
	}
}
