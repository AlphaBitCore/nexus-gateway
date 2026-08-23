package canonicalext

import (
	"strings"
	"testing"
)

// Absolute-cost measurement for Strip at realistic egress body sizes.
//
// Strip runs on EVERY egress body, so the question is not "is it faster than
// the alternative" but "what does it cost in nanoseconds against a request that
// takes hundreds of milliseconds upstream". These benchmarks answer that at the
// two sizes that bracket real traffic: a 2 KB chat completion and a 200 KB one.
//
// Three shapes are measured because they take three different paths:
//   - clean:  no `"nexus"` token anywhere      -> one substring scan, no parse
//   - carrier: the root namespace is present   -> scan + ValidBytes + delete
//   - decoy:  the token is present but the ROOT key is not -> scan + ValidBytes
//     + a delete that removes nothing. This is the wasted-parse case.
//
// The `word` shape is measured too, and it is the answer to a question worth
// pinning: prose containing the WORD nexus cannot trip the pre-check, because
// rootKey is the QUOTED token and a JSON string escapes its quotes as `\"`,
// which puts a backslash between the quote and the `n`. So the case usually
// imagined as pathological is in fact identical to clean, and the real decoy
// has to be a caller-authored `"nexus"` KEY nested somewhere the delete will
// not touch (a tool/json_schema property — the shape the package's own test
// TestStrip_LeavesCallerAuthoredNexusKeysAlone protects).

// hotpathSink defeats dead-store elimination of the returned slice.
var hotpathSink []byte

// hotpathBody builds a chat-completion body of approximately payloadBytes of
// assistant content in one of four shapes.
func hotpathBody(payloadBytes int, shape string) []byte {
	filler := strings.Repeat("lorem ipsum dolor sit amet consectetur ", payloadBytes/39+1)

	var b strings.Builder
	b.WriteString(`{"id":"chatcmpl-hp","object":"chat.completion","created":1750000000,`)
	b.WriteString(`"model":"gpt-5.4","choices":[{"index":0,"message":{"role":"assistant","content":"`)
	if shape == "word" {
		// The bare word inside a string value, escaped quotes included, to show
		// that neither form reaches the parse.
		b.WriteString(`what is nexus for? the docs call it \"nexus\" sometimes. `)
	}
	b.WriteString(filler)
	b.WriteString(`"},"finish_reason":"stop"}],`)
	if shape == "decoy" {
		// A caller-authored `nexus` property inside a tool schema. The pre-check
		// hits, the body is fully validated, and the root delete finds nothing.
		b.WriteString(`"tools":[{"type":"function","function":{"name":"lookup","parameters":` +
			`{"type":"object","properties":{"nexus":{"type":"string"},"q":{"type":"string"}},` +
			`"required":["nexus"]}}}],`)
	}
	b.WriteString(`"usage":{"prompt_tokens":1200,"completion_tokens":800,"total_tokens":2000}`)
	if shape == "carrier" {
		b.WriteString(`,"nexus":{"ext":{"openai":{"responses":{"id":"resp_1","status":"completed"}}}}`)
	}
	b.WriteString(`}`)
	return []byte(b.String())
}

func benchStripShape(b *testing.B, payloadBytes int, shape string) {
	body := hotpathBody(payloadBytes, shape)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		hotpathSink = Strip(body)
	}
	// Assert the path actually taken, so a benchmark cannot silently measure
	// the cheap branch while claiming to measure the expensive one.
	got := string(Strip(body))
	switch shape {
	case "clean", "word":
		if got != string(body) {
			b.Fatalf("%s body was rewritten; this arm must be the no-op path", shape)
		}
	case "decoy":
		if got != string(body) {
			b.Fatalf("decoy body was rewritten: the caller's nested key was deleted")
		}
	case "carrier":
		if strings.Contains(got, `"nexus"`) {
			b.Fatalf("carrier body still carries the namespace; the delete did not run")
		}
	}
}

func BenchmarkStripHotpath_Clean_2KB(b *testing.B)   { benchStripShape(b, 2<<10, "clean") }
func BenchmarkStripHotpath_Clean_200KB(b *testing.B) { benchStripShape(b, 200<<10, "clean") }
func BenchmarkStripHotpath_Word_2KB(b *testing.B)    { benchStripShape(b, 2<<10, "word") }
func BenchmarkStripHotpath_Word_200KB(b *testing.B)  { benchStripShape(b, 200<<10, "word") }
func BenchmarkStripHotpath_Carrier_2KB(b *testing.B) { benchStripShape(b, 2<<10, "carrier") }
func BenchmarkStripHotpath_Carrier_200KB(b *testing.B) {
	benchStripShape(b, 200<<10, "carrier")
}
func BenchmarkStripHotpath_Decoy_2KB(b *testing.B)   { benchStripShape(b, 2<<10, "decoy") }
func BenchmarkStripHotpath_Decoy_200KB(b *testing.B) { benchStripShape(b, 200<<10, "decoy") }

// The shapes must actually differ in which branch they take, or the numbers
// above describe one path measured four times.
func TestHotpathBodyShapesTakeTheIntendedBranches(t *testing.T) {
	for _, tc := range []struct {
		shape       string
		tripsScan   bool
		isRewritten bool
	}{
		{"clean", false, false},
		{"word", false, false},
		{"decoy", true, false},
		{"carrier", true, true},
	} {
		body := hotpathBody(2<<10, tc.shape)
		if got := strings.Contains(string(body), `"nexus"`); got != tc.tripsScan {
			t.Errorf("%s: contains quoted token = %v, want %v — the arm is measuring the wrong branch",
				tc.shape, got, tc.tripsScan)
		}
		if got := string(Strip(body)) != string(body); got != tc.isRewritten {
			t.Errorf("%s: rewritten = %v, want %v", tc.shape, got, tc.isRewritten)
		}
	}
}
