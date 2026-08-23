package provbuiltins

import (
	"sort"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// A field the caller set either reaches the wire, or is a drop somebody decided
// on. There is no third acceptable state, and the third state is what shipped:
// `cache_control` was rebuilt away by a projection, nothing recorded it, and it
// took a production cost anomaly to notice — a caller paying full price for a
// prefix they had asked to cache.
//
// THE ORACLE, and why it is a list rather than a table of expectations. Nothing
// in the gateway can currently tell a dropped field from an unsupported one:
// Response.Coerced is populated by the rule-table appliers only, so the
// projection encoders report nothing, and canonicalext.ScanUnsupported walks
// TOP-LEVEL keys with a process-global dedup, so it can never see a field nested
// in a content part. So the oracle lives here, as an EXCEPTION list: these
// drops are known and reasoned, everything else is a defect.
//
// The direction of the default is the whole point. A table of expectations
// ("these fields should survive") is green for anything nobody thought to list —
// the fixture validating itself. An exception list is red for anything nobody
// thought to list, and extending it costs a written reason a reviewer sees. The
// repo already runs this shape for coverage and for file size.
//
// Its second output is evidence: how large the legitimate-drop surface really is
// per wire, which is what pricing a runtime emit-set declaration needs.

// callerBody is one canonical request carrying the fields a caller can set. The
// content part carries cache_control deliberately — the nesting is the level the
// existing machinery is blind at.
const callerBody = `{
  "model":"m",
  "messages":[{"role":"user","content":[
    {"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}
  ]}],
  "max_tokens":64,
  "temperature":0.5,
  "top_p":0.9,
  "stop":["END"],
  "stream":false,
  "tools":[{"type":"function","function":{"name":"f","description":"d",
    "parameters":{"type":"object","properties":{"a":{"type":"string"}}}}}],
  "tool_choice":"auto"
}`

// callerFields are the paths checked for survival. Each is something a caller
// wrote and can reasonably expect to matter; a path is judged present if the
// wire carries an equivalent under ANY name (a rename is a survival, not a drop
// — the rename is the translation doing its job).
var callerFields = []struct {
	name string
	// present reports whether the emitted wire body still carries this
	// caller's intent, under whatever name that wire spells it.
	present func(wire gjson.Result) bool
}{
	{"messages", func(w gjson.Result) bool {
		return w.Get("messages").IsArray() || w.Get("contents").IsArray() || w.Get("input").Exists()
	}},
	{"max_tokens", func(w gjson.Result) bool {
		return w.Get("max_tokens").Exists() || w.Get("max_completion_tokens").Exists() ||
			w.Get("maxTokens").Exists() || w.Get("generationConfig.maxOutputTokens").Exists()
	}},
	{"temperature", func(w gjson.Result) bool {
		return w.Get("temperature").Exists() || w.Get("generationConfig.temperature").Exists()
	}},
	{"top_p", func(w gjson.Result) bool {
		return w.Get("top_p").Exists() || w.Get("p").Exists() ||
			w.Get("generationConfig.topP").Exists()
	}},
	{"stop", func(w gjson.Result) bool {
		return w.Get("stop").Exists() || w.Get("stop_sequences").Exists() ||
			w.Get("stopSequences").Exists() || w.Get("generationConfig.stopSequences").Exists()
	}},
	{"tools", func(w gjson.Result) bool {
		return w.Get("tools").IsArray() || w.Get("toolConfig").Exists()
	}},
	{"tool_choice", func(w gjson.Result) bool {
		return w.Get("tool_choice").Exists() || w.Get("toolConfig.functionCallingConfig").Exists()
	}},
	{"cache_control", func(w gjson.Result) bool {
		// Nested, on purpose: this is the level nothing else can see.
		return strings.Contains(w.Raw, `"cache_control"`)
	}},
}

// legalDrops names, per wire shape, the caller fields that wire legitimately
// does not carry — each with the reason. An empty reason is not accepted; a drop
// without a reason is indistinguishable from a drop nobody noticed.
//
// ADDING AN ENTRY IS THE POINT OF REVIEW. If a field starts being dropped and
// the fix is to list it here, the reason has to say why the caller is not
// harmed — or the entry is documenting a defect rather than excusing one.
var legalDrops = map[typology.WireShape]map[string]string{
	typology.WireShapeGeminiGenerateContent: {
		"cache_control": "Gemini's context cache is a separate addressable resource created " +
			"and referenced by name, not an inline per-part marker, so this wire has no field " +
			"to carry the caller's marker into. Dropping it costs the caller nothing they " +
			"could have had on this wire.",
	},
}

// The first version of this map was written from assumption and listed five
// drops across three wires. The stale-exemption check below caught four of them
// immediately: those fields DO reach the wire. That is the check earning its
// place — an exemption for a drop that is not happening is an exemption sitting
// ready to excuse the next drop that is.
//
// What the measurement actually shows, and it is the evidence a runtime
// emit-set declaration has to be priced against: across four wires and eight
// caller fields, the legitimate-drop surface is ONE entry. Anthropic carries
// every field including the nested cache_control; Gemini carries everything but
// that marker, which it has nowhere to put; the OpenAI-family wires are the
// identity. A cross-cutting runtime mechanism would be defending a surface of
// one.

func encodeForWire(t *testing.T, shape typology.WireShape) ([]byte, bool) {
	t.Helper()
	f, ok := shapeFormat[shape]
	if !ok {
		t.Fatalf("shape %s has no adapter format mapping", shape)
	}
	adapters := registeredAdapters(t)
	adapter, ok := adapters[f]
	if !ok {
		t.Fatalf("format %s is not registered", f)
	}
	out, err := prepareCrossFormat(adapter, f, shape, callerBody)
	if err != nil {
		t.Logf("%s: PrepareBody refused this body (%v); the wire cannot be judged", shape, err)
		return nil, false
	}
	return out, true
}

// The ratchet. A caller field that does not reach the wire must be on the list
// for that wire, with a reason.
func TestCallerFieldSurvival_EveryDropIsOneSomebodyDecided(t *testing.T) {
	for _, shape := range []typology.WireShape{
		typology.WireShapeAnthropicMessages,
		typology.WireShapeGeminiGenerateContent,
		typology.WireShapeCohereChat,
		typology.WireShapeOpenAIChat,
	} {
		t.Run(string(shape), func(t *testing.T) {
			wireBytes, ok := encodeForWire(t, shape)
			if !ok {
				return
			}
			wire := gjson.ParseBytes(wireBytes)
			allowed := legalDrops[shape]

			var undecided []string
			for _, f := range callerFields {
				if f.present(wire) {
					continue
				}
				reason, listed := allowed[f.name]
				if !listed {
					undecided = append(undecided, f.name)
					continue
				}
				if strings.TrimSpace(reason) == "" {
					t.Errorf("%s drops %q and the list carries no reason — a drop without a "+
						"reason cannot be told from one nobody noticed", shape, f.name)
				}
			}
			if len(undecided) > 0 {
				sort.Strings(undecided)
				t.Errorf("%s silently drops caller field(s) %v.\n"+
					"Nothing reports this to the caller: Coerced covers the rule-table "+
					"appliers only, and ScanUnsupported is top-level and process-deduped. "+
					"Either carry the field, or add it to legalDrops with a reason saying why "+
					"the caller is not harmed.\nwire=%s", shape, undecided, truncate(wireBytes, 400))
			}
		})
	}
}

// The list must not outlive what it excuses. An entry for a field that now
// survives is an exemption quietly covering whatever breaks next.
func TestCallerFieldSurvival_NoStaleExemptions(t *testing.T) {
	for shape, allowed := range legalDrops {
		wireBytes, ok := encodeForWire(t, shape)
		if !ok {
			continue
		}
		wire := gjson.ParseBytes(wireBytes)
		for _, f := range callerFields {
			reason, listed := allowed[f.name]
			if !listed {
				continue
			}
			if f.present(wire) {
				t.Errorf("%s lists %q as a legal drop (%q) but the field now reaches the wire "+
					"— remove the entry, or it will silently excuse the next real drop",
					shape, f.name, reason)
			}
		}
	}
}

// The drop surface is the evidence a runtime emit-set declaration would be
// priced against, so it is measured FROM THE WIRE BODIES and asserted, not
// printed.
//
// The previous version printed len(legalDrops[shape]) against len(callerFields)
// — two literals from this same file — through t.Logf. It reported the size of
// the EXEMPTION LIST rather than of the real surface (if a wire began dropping
// three more fields tomorrow it would still have printed "1"), and t.Logf is
// suppressed without -v, which CI does not pass. A constant, restated in a form
// nobody reads.
//
// Asserting the number is what makes it evidence: the surface can only change
// with someone editing this line, and the number in the failure message is the
// real one.
func TestCallerFieldSurvival_TheDropSurfaceIsOne(t *testing.T) {
	// The pricing question: across every wire and every caller field, how many
	// (wire, field) pairs actually fail to survive? A cross-cutting runtime
	// mechanism would be defending exactly this many.
	const wantDrops = 1

	var actual []string
	for _, shape := range []typology.WireShape{
		typology.WireShapeAnthropicMessages,
		typology.WireShapeGeminiGenerateContent,
		typology.WireShapeCohereChat,
		typology.WireShapeOpenAIChat,
	} {
		wireBytes, ok := encodeForWire(t, shape)
		if !ok {
			continue
		}
		wire := gjson.ParseBytes(wireBytes)
		for _, f := range callerFields {
			if !f.present(wire) {
				actual = append(actual, string(shape)+"/"+f.name)
			}
		}
	}
	sort.Strings(actual)

	if len(actual) != wantDrops {
		t.Errorf("the real drop surface is %d pair(s): %v — want %d.\n"+
			"This number is the evidence a cross-cutting emit-set mechanism is priced "+
			"against; it moving is the signal that the answer may have changed.",
			len(actual), actual, wantDrops)
	}
}
