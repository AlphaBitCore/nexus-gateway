package codec

import (
	"bytes"
	"reflect"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

var rnTarget = provcore.CallTarget{ProviderModelID: "provider-real-model"}

// plain returns an empty-contract codec — the shape every quirk-free
// sibling constructs.
func plain() *identityCodec { return newIdentity(Contract{}) }

// quirky returns a codec with a chat+responses contract gated on the
// "quirk-" model prefix, mirroring the real reasoning contract's shape
// without depending on the rewrites package (which imports this one).
func quirky() *identityCodec {
	return newIdentity(Contract{
		Chat:      testRules(),
		Responses: []FieldRule{{Applies: quirkGate, Field: "temperature"}, {Applies: quirkGate, Field: "top_p"}},
	})
}

func TestRewriteNative_Chat_StampsAlias(t *testing.T) {
	res, err := plain().RewriteNative(typology.WireShapeOpenAIChat,
		[]byte(`{"model":"alias","messages":[]}`), rnTarget, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(res.Body, "model").String() != "provider-real-model" {
		t.Fatalf("model not stamped: %s", res.Body)
	}
	if res.Rewrites != nil {
		t.Fatalf("stamp is not a coercion; rewrites must be empty, got %v", res.Rewrites)
	}
}

func TestRewriteNative_Embeddings_StampsAlias(t *testing.T) {
	res, err := plain().RewriteNative(typology.WireShapeOpenAIEmbeddings,
		[]byte(`{"model":"alias","input":"x"}`), rnTarget, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(res.Body, "model").String() != "provider-real-model" {
		t.Fatalf("embeddings model not stamped: %s", res.Body)
	}
}

func TestRewriteNative_StreamingConformant_Verbatim(t *testing.T) {
	body := []byte(`{"model":"provider-real-model","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)
	res, err := plain().RewriteNative(typology.WireShapeOpenAIChat, body, rnTarget, true)
	if err != nil {
		t.Fatal(err)
	}
	if &res.Body[0] != &body[0] {
		t.Fatal("conformant streaming body must return the same slice")
	}
}

func TestRewriteNative_StreamingNonConformant_StampsAndInjects(t *testing.T) {
	res, err := plain().RewriteNative(typology.WireShapeOpenAIChat,
		[]byte(`{"model":"alias","messages":[],"temperature":0.5}`), rnTarget, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, probe := range []string{`"model":"provider-real-model"`, `"stream":true`, `"include_usage":true`, `"temperature":0.5`} {
		if !bytes.Contains(res.Body, []byte(probe)) {
			t.Fatalf("streaming differential lost %s: %s", probe, res.Body)
		}
	}
}

func TestRewriteNative_StreamingMalformed_Errors(t *testing.T) {
	if _, err := plain().RewriteNative(typology.WireShapeOpenAIChat,
		[]byte(`{"model":`), rnTarget, true); err == nil {
		t.Fatal("malformed streaming body needing a rewrite must error (map decode)")
	}
}

func TestRewriteNative_StreamingEmptyBody_Verbatim(t *testing.T) {
	res, err := plain().RewriteNative(typology.WireShapeOpenAIChat, nil, rnTarget, true)
	if err != nil || res.Body != nil {
		t.Fatalf("empty streaming body must pass through: %s err=%v", res.Body, err)
	}
}

func TestRewriteNative_StreamingNonObjectStreamOptions_Replaced(t *testing.T) {
	res, err := plain().RewriteNative(typology.WireShapeOpenAIChat,
		[]byte(`{"model":"provider-real-model","stream":true,"stream_options":"bogus","messages":[]}`), rnTarget, true)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(res.Body, "stream_options.include_usage").Bool() != true {
		t.Fatalf("non-object stream_options must be replaced so usage extraction survives: %s", res.Body)
	}
}

func TestRewriteNative_UnknownShape_Verbatim(t *testing.T) {
	body := []byte(`{"anything":true}`)
	res, err := quirky().RewriteNative(typology.WireShapeAnthropicMessages, body,
		provcore.CallTarget{ProviderModelID: "quirk-1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if &res.Body[0] != &body[0] {
		t.Fatal("shapes without a body-root model must pass through verbatim")
	}
}

func TestRewriteNative_ChatQuirk_SurgicalStripPreservesLayout(t *testing.T) {
	// The contract edit is surgical: untouched bytes — key order, spacing,
	// unknown fields — survive exactly, unlike a decode round-trip.
	body := []byte(`{"zz_first":1,"model":"quirk-1","messages":[],"temperature":0,"max_tokens":64}`)
	res, err := quirky().RewriteNative(typology.WireShapeOpenAIChat, body,
		provcore.CallTarget{ProviderModelID: "quirk-1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(res.Body, []byte(`{"zz_first":1,`)) {
		t.Fatalf("surgical edit must preserve the caller's layout: %s", res.Body)
	}
	if gjson.GetBytes(res.Body, "temperature").Exists() || gjson.GetBytes(res.Body, "max_tokens").Exists() {
		t.Fatalf("due fields must be gone: %s", res.Body)
	}
	if gjson.GetBytes(res.Body, "max_completion_tokens").Int() != 64 {
		t.Fatalf("rename must land: %s", res.Body)
	}
	want := []string{"max_tokens→max_completion_tokens", "temperature→removed"}
	if !reflect.DeepEqual(res.Rewrites, want) {
		t.Fatalf("rewrites %v, want %v", res.Rewrites, want)
	}
}

func TestRewriteNative_ChatQuirkConformant_Verbatim(t *testing.T) {
	// The post-bridge re-entry shape: a quirk-family model whose body is
	// already conformant must forward the same slice — presence-based
	// probing, never model-keyed decoding.
	body := []byte(`{"model":"quirk-1","messages":[{"role":"user","content":"hi"}]}`)
	res, err := quirky().RewriteNative(typology.WireShapeOpenAIChat, body,
		provcore.CallTarget{ProviderModelID: "quirk-1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if &res.Body[0] != &body[0] {
		t.Fatal("conformant quirk-model body must return the same slice")
	}
	if res.Rewrites != nil {
		t.Fatalf("nothing was rewritten: %v", res.Rewrites)
	}
}

func TestRewriteNative_ChatQuirkDupKey_MapDoorStillStrips(t *testing.T) {
	// A duplicated due key forces the decode door, where last-wins
	// semantics are exact and the strip still lands.
	body := []byte(`{"model":"quirk-1","temperature":0.1,"temperature":0.9,"messages":[]}`)
	res, err := quirky().RewriteNative(typology.WireShapeOpenAIChat, body,
		provcore.CallTarget{ProviderModelID: "quirk-1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(res.Body, "temperature").Exists() {
		t.Fatalf("map door must strip the deduplicated field: %s", res.Body)
	}
	if len(res.Rewrites) != 1 || res.Rewrites[0] != "temperature→removed" {
		t.Fatalf("map door must report the strip: %v", res.Rewrites)
	}
}

func TestRewriteNative_StreamingQuirkFieldPresent_MapDoorStripsAndInjects(t *testing.T) {
	body := []byte(`{"model":"quirk-1","stream":true,"stream_options":{"include_usage":true},"temperature":0.4,"messages":[]}`)
	res, err := quirky().RewriteNative(typology.WireShapeOpenAIChat, body,
		provcore.CallTarget{ProviderModelID: "quirk-1"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(res.Body, "temperature").Exists() {
		t.Fatalf("streaming quirk strip must land: %s", res.Body)
	}
	if gjson.GetBytes(res.Body, "stream_options.include_usage").Bool() != true {
		t.Fatalf("usage injection must survive the same decode: %s", res.Body)
	}
	if len(res.Rewrites) != 1 || res.Rewrites[0] != "temperature→removed" {
		t.Fatalf("strip must be reported: %v", res.Rewrites)
	}
}

func TestRewriteNative_StreamingQuirkConformant_Verbatim(t *testing.T) {
	body := []byte(`{"model":"quirk-1","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)
	res, err := quirky().RewriteNative(typology.WireShapeOpenAIChat, body,
		provcore.CallTarget{ProviderModelID: "quirk-1"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if &res.Body[0] != &body[0] {
		t.Fatal("conformant streaming quirk-model body must return the same slice")
	}
}

// The two doors of the /v1/responses wire share one body: RewriteNative and
// EncodeRequest must produce identical bytes and identical coercion reports
// for the same input — the wire-gap class (200 on one door, 400 on the
// other) structurally cannot come back.
func TestRewriteNative_Responses_EqualsEncodeRequest(t *testing.T) {
	c := quirky()
	reasoning := provcore.CallTarget{ProviderModelID: "quirk-9"}
	for _, tc := range []struct {
		name   string
		body   string
		target provcore.CallTarget
	}{
		{"plain model stamp", `{"model":"alias","input":"x"}`, rnTarget},
		{"reasoning sampling strip", `{"model":"alias","input":"x","temperature":0,"top_p":0.9}`, reasoning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viaRewrite, err := c.RewriteNative(typology.WireShapeOpenAIResponses, []byte(tc.body), tc.target, false)
			if err != nil {
				t.Fatal(err)
			}
			viaEncode, err := c.EncodeRequest(typology.WireShapeOpenAIResponses, []byte(tc.body), tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(viaRewrite.Body, viaEncode.Body) {
				t.Fatalf("two doors diverge:\n rewrite=%s\n  encode=%s", viaRewrite.Body, viaEncode.Body)
			}
			if !reflect.DeepEqual(viaRewrite.Rewrites, viaEncode.Rewrites) {
				t.Fatalf("coercion reports diverge: %v vs %v", viaRewrite.Rewrites, viaEncode.Rewrites)
			}
		})
	}
}

// The same equivalence for the chat wire: EncodeRequest (canonical door)
// and non-streaming RewriteNative (native door) delegate to one shared
// differential, so for any OpenAI-shape body — where canonicalize(B) = B —
// the two doors are the same function.
func TestChat_TwoDoors_OneBody(t *testing.T) {
	c := quirky()
	for _, tc := range []struct {
		name   string
		body   string
		target provcore.CallTarget
	}{
		{"plain stamp", `{"model":"alias","messages":[]}`, rnTarget},
		{"quirk strip+rename", `{"model":"alias","messages":[],"temperature":0,"max_tokens":32}`, provcore.CallTarget{ProviderModelID: "quirk-1"}},
		{"quirk conformant", `{"model":"quirk-1","messages":[]}`, provcore.CallTarget{ProviderModelID: "quirk-1"}},
		{"dup-key decode door", `{"model":"quirk-1","temperature":0.1,"temperature":0.9}`, provcore.CallTarget{ProviderModelID: "quirk-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viaRewrite, err := c.RewriteNative(typology.WireShapeOpenAIChat, []byte(tc.body), tc.target, false)
			if err != nil {
				t.Fatal(err)
			}
			viaEncode, err := c.EncodeRequest(typology.WireShapeOpenAIChat, []byte(tc.body), tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(viaRewrite.Body, viaEncode.Body) {
				t.Fatalf("two doors diverge:\n rewrite=%s\n  encode=%s", viaRewrite.Body, viaEncode.Body)
			}
			if !reflect.DeepEqual(viaRewrite.Rewrites, viaEncode.Rewrites) {
				t.Fatalf("coercion reports diverge: %v vs %v", viaRewrite.Rewrites, viaEncode.Rewrites)
			}
		})
	}
}

// RewriteNative ∘ RewriteNative = RewriteNative, with no duplicate rewrite
// reporting on the second pass: the differential only ever applies what is
// still due, so re-entry (e.g. cache-prep then executor) cannot double-edit
// or double-report.
func TestRewriteNative_Idempotent(t *testing.T) {
	c := quirky()
	cases := []struct {
		name   string
		shape  typology.WireShape
		body   string
		target provcore.CallTarget
		stream bool
	}{
		{"chat stamp", typology.WireShapeOpenAIChat, `{"model":"alias","messages":[]}`, rnTarget, false},
		{"chat quirk", typology.WireShapeOpenAIChat, `{"model":"alias","temperature":0,"max_tokens":32,"messages":[]}`, provcore.CallTarget{ProviderModelID: "quirk-1"}, false},
		{"chat quirk dup", typology.WireShapeOpenAIChat, `{"model":"quirk-1","temperature":0.1,"temperature":0.9}`, provcore.CallTarget{ProviderModelID: "quirk-1"}, false},
		{"streaming", typology.WireShapeOpenAIChat, `{"model":"alias","messages":[]}`, rnTarget, true},
		{"streaming quirk", typology.WireShapeOpenAIChat, `{"model":"quirk-1","stream":true,"temperature":0.2,"messages":[]}`, provcore.CallTarget{ProviderModelID: "quirk-1"}, true},
		{"responses quirk", typology.WireShapeOpenAIResponses, `{"model":"quirk-9","input":"x","temperature":0}`, provcore.CallTarget{ProviderModelID: "quirk-9"}, false},
		{"embeddings", typology.WireShapeOpenAIEmbeddings, `{"model":"alias","input":"x"}`, rnTarget, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, err := c.RewriteNative(tc.shape, []byte(tc.body), tc.target, tc.stream)
			if err != nil {
				t.Fatal(err)
			}
			second, err := c.RewriteNative(tc.shape, first.Body, tc.target, tc.stream)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first.Body, second.Body) {
				t.Fatalf("not idempotent:\n first=%s\nsecond=%s", first.Body, second.Body)
			}
			if len(second.Rewrites) != 0 {
				t.Fatalf("second pass must report nothing (already applied): %v", second.Rewrites)
			}
		})
	}
}

func TestRewriteNative_Responses_StripReported(t *testing.T) {
	res, err := quirky().RewriteNative(typology.WireShapeOpenAIResponses,
		[]byte(`{"model":"quirk-9","input":"x","temperature":0}`),
		provcore.CallTarget{ProviderModelID: "quirk-9"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(res.Body, []byte("temperature")) {
		t.Fatalf("reasoning sampling param must be stripped: %s", res.Body)
	}
	if len(res.Rewrites) != 1 || res.Rewrites[0] != "temperature→removed" {
		t.Fatalf("strip must be reported for x-nexus-coerced, got %v", res.Rewrites)
	}
}

func TestRewriteNative_CompletionsLegacy_StampOnlyNoRules(t *testing.T) {
	// max_tokens is the correct name on the legacy wire: even a
	// quirk-family model keeps it there.
	res, err := quirky().RewriteNative(typology.WireShapeOpenAICompletionsLegacy,
		[]byte(`{"model":"alias","prompt":"x","max_tokens":10,"temperature":0.5}`),
		provcore.CallTarget{ProviderModelID: "quirk-1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(res.Body, "max_tokens").Int() != 10 || gjson.GetBytes(res.Body, "temperature").Float() != 0.5 {
		t.Fatalf("legacy wire must not get chat rules: %s", res.Body)
	}
	if gjson.GetBytes(res.Body, "model").String() != "quirk-1" {
		t.Fatalf("legacy wire still stamps: %s", res.Body)
	}
}

func TestRewriteNative_EmbeddingsQuirk_StripsViaContract(t *testing.T) {
	c := newIdentity(Contract{
		Embeddings: []FieldRule{{Applies: quirkGate, Field: "dimensions", Label: "dimensions→removed (test)"}},
	})
	res, err := c.RewriteNative(typology.WireShapeOpenAIEmbeddings,
		[]byte(`{"model":"alias","input":"x","dimensions":256}`),
		provcore.CallTarget{ProviderModelID: "quirk-embed"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(res.Body, "dimensions").Exists() {
		t.Fatalf("native embeddings door must apply contract strips: %s", res.Body)
	}
	if gjson.GetBytes(res.Body, "model").String() != "quirk-embed" {
		t.Fatalf("stamp must land too: %s", res.Body)
	}
	if len(res.Rewrites) != 1 || res.Rewrites[0] != "dimensions→removed (test)" {
		t.Fatalf("strip must be reported: %v", res.Rewrites)
	}
}
