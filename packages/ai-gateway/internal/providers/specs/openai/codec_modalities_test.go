package openai

import (
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// `modalities` is a legitimate field on this wire — the audio models REQUIRE it
// — and a hard 400 on every other model. So the rule has two halves, and the
// second is the dangerous one: strip it from an audio model and a working
// request becomes "This model requires that either input content or output
// modality contain audio", a worse failure than the one being fixed.
//
// Both halves are asserted here, through the codec the spec SHIPS, on both
// doors. The strip alone was already covered by the contract-shape test; the
// carve-out was not, and a mutation that deleted it left every test green.
func sendModalities(t *testing.T, door, model string) []byte {
	t.Helper()
	spec := NewSpec(nil)
	tgt := provcore.CallTarget{ProviderModelID: model}
	body := `{"model":"` + model + `","modalities":["text"],` +
		`"messages":[{"role":"user","content":"hi"}]}`
	var res provcore.EncodeResult
	var err error
	if door == "encode" {
		res, err = spec.SchemaCodec.EncodeRequest(typology.WireShapeOpenAIChat, []byte(body), tgt)
	} else {
		res, err = spec.SchemaCodec.RewriteNative(typology.WireShapeOpenAIChat, []byte(body), tgt, false)
	}
	if err != nil {
		t.Fatalf("%s door refused a request carrying modalities: %v", door, err)
	}
	return res.Body
}

// Measured by replaying production 4xx with their original payloads: 17 came
// back "unknown_parameter Unknown parameter: 'modalities'." on exactly these
// families. Real callers, real bodies, and the field was doing nothing for
// those requests.
func TestModalities_StrippedForTheModelsThatRejectIt(t *testing.T) {
	for _, model := range []string{"gpt-5.4", "gpt-5.5", "gpt-5.6-terra", "o1", "o3", "o4-mini"} {
		for _, door := range []string{"encode", "native"} {
			if gjson.GetBytes(sendModalities(t, door, model), "modalities").Exists() {
				t.Errorf("%s door forwarded modalities to %s, which answers 400 for it", door, model)
			}
		}
	}
}

// THE CARVE-OUT. Stripping here would break the audio path outright, and this
// is the assertion whose absence let a mutation delete the carve-out and stay
// green.
func TestModalities_KeptForTheAudioModelsThatRequireIt(t *testing.T) {
	// gpt-6-audio is in this list deliberately. Today's names ("gpt-audio-1.5")
	// carry no digit after "gpt-", so the generation test never matches them and
	// the carve-out changes nothing — which is exactly why a mutation deleting
	// it stayed green. The moment OpenAI names an audio model inside the
	// generation series, the strip WOULD match and take away the one field that
	// model cannot run without. That is the case the guard exists for, so that
	// is the case the test has to name.
	for _, model := range []string{"gpt-audio-1.5", "gpt-audio-mini", "gpt-6-audio"} {
		for _, door := range []string{"encode", "native"} {
			got := gjson.GetBytes(sendModalities(t, door, model), "modalities")
			if !got.Exists() {
				t.Errorf("%s door stripped modalities from %s — this model REQUIRES it, and "+
					"without it the request fails with \"requires that either input content or "+
					"output modality contain audio\"", door, model)
				continue
			}
			if got.String() != `["text"]` && len(got.Array()) != 1 {
				t.Errorf("%s door altered the caller's modalities for %s: %s", door, model, got.Raw)
			}
		}
	}
}

// A model outside both sets keeps what the caller sent: gpt-4o accepts the
// field, and stripping it there would discard a caller's choice for nothing.
func TestModalities_UntouchedOnModelsWithNoEvidenceEitherWay(t *testing.T) {
	if !gjson.GetBytes(sendModalities(t, "native", "gpt-4o"), "modalities").Exists() {
		t.Error("modalities was stripped from gpt-4o, which has no measured rejection")
	}
}
