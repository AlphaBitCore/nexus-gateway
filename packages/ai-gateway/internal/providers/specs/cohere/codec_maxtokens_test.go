package cohere

import (
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// The defect, measured across a cross-ingress production run: the same Cohere
// model succeeded over /v1/chat/completions and failed over /v1/responses with
// HTTP 422 "unknown field: parameter 'max_completion_tokens' is not a valid
// field" — ten rows, five models × stream and non-stream. Only the ingress
// differed, which is precisely the asymmetry a single-ingress test cannot see.
func TestEncodeRequest_MaxCompletionTokensReachesCohereAsMaxTokens(t *testing.T) {
	in := []byte(`{"model":"command-a-03-2025","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":64}`)

	got, err := codec{}.EncodeRequest(typology.WireShapeCohereChat, in, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest returned %v", err)
	}
	if v := gjson.GetBytes(got.Body, "max_tokens"); !v.Exists() || v.Int() != 64 {
		t.Errorf("max_tokens = %v, want 64 — the budget must survive the rename", v.Raw)
	}
	if gjson.GetBytes(got.Body, "max_completion_tokens").Exists() {
		t.Error("max_completion_tokens still on the wire — Cohere rejects unknown fields with a 422")
	}
}

// max_completion_tokens is the newer spelling and wins, matching what the
// anthropic and gemini codecs already do. Disagreeing per-provider would make
// the same request mean different things depending on where it routed.
func TestEncodeRequest_NewerSpellingWinsWhenBothArePresent(t *testing.T) {
	in := []byte(`{"model":"command-r-08-2024","messages":[],"max_tokens":16,"max_completion_tokens":128}`)

	got, err := codec{}.EncodeRequest(typology.WireShapeCohereChat, in, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest returned %v", err)
	}
	if v := gjson.GetBytes(got.Body, "max_tokens"); v.Int() != 128 {
		t.Errorf("max_tokens = %d, want 128", v.Int())
	}
	if gjson.GetBytes(got.Body, "max_completion_tokens").Exists() {
		t.Error("max_completion_tokens must not reach Cohere")
	}
}

// The common path must not move: a body that already speaks Cohere's spelling
// is forwarded untouched.
func TestEncodeRequest_PlainMaxTokensIsLeftAlone(t *testing.T) {
	in := []byte(`{"model":"command-r-08-2024","messages":[],"max_tokens":32}`)

	got, err := codec{}.EncodeRequest(typology.WireShapeCohereChat, in, provcore.CallTarget{})
	if err != nil {
		t.Fatalf("EncodeRequest returned %v", err)
	}
	if v := gjson.GetBytes(got.Body, "max_tokens"); v.Int() != 32 {
		t.Errorf("max_tokens = %d, want 32 unchanged", v.Int())
	}
}

// The fold must survive the other rewrite on this path — model injection
// round-trips the body through encode/decode, which is where a naive
// implementation loses an earlier edit.
func TestEncodeRequest_FoldSurvivesModelInjection(t *testing.T) {
	in := []byte(`{"messages":[{"role":"user","content":"hi"}],"max_completion_tokens":48}`)

	got, err := codec{}.EncodeRequest(typology.WireShapeCohereChat, in,
		provcore.CallTarget{ProviderModelID: "command-a-03-2025"})
	if err != nil {
		t.Fatalf("EncodeRequest returned %v", err)
	}
	if v := gjson.GetBytes(got.Body, "model"); v.Str != "command-a-03-2025" {
		t.Errorf("model = %q, want the target's provider model id", v.Str)
	}
	if v := gjson.GetBytes(got.Body, "max_tokens"); v.Int() != 48 {
		t.Errorf("max_tokens = %d, want 48 — model injection must not undo the fold", v.Int())
	}
	if gjson.GetBytes(got.Body, "max_completion_tokens").Exists() {
		t.Error("max_completion_tokens survived model injection")
	}
}

// A body that already speaks Cohere's spellings passes through the projection
// unchanged — the rename step must not perturb a request that needed no rename.
func TestProjectToCohereChat_AlreadyCohereShapedBodiesAreUnchanged(t *testing.T) {
	for _, in := range []string{
		`{"model":"x","messages":[]}`,
		`{"max_tokens":8}`,
	} {
		if got := string(projectToCohereChat([]byte(in))); got != in {
			t.Errorf("projectToCohereChat(%q) = %q; a body needing no rename must be untouched", in, got)
		}
	}
}
