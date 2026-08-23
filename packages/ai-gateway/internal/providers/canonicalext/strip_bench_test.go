package canonicalext

import (
	"strings"
	"testing"
)

// Strip runs on the response hot path, once per non-stream response, so its
// cost on a body that carries NOTHING to remove is the number that matters —
// that is the overwhelmingly common case now that the codecs no longer write
// the namespace into responses. The pre-check is what bounds it: a body without
// the namespace is scanned once and returned byte-identical, with no parse and
// no allocation.
//
// The with-namespace case is measured too, so the claim "the removal is rare
// and the scan is the cost" rests on both halves being known rather than on the
// cheap half being the only one reported.

func benchBody(payloadBytes int, withNamespace bool) []byte {
	var b strings.Builder
	b.WriteString(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-5.4","choices":[{"index":0,"message":{"role":"assistant","content":"`)
	b.WriteString(strings.Repeat("lorem ipsum dolor sit amet ", payloadBytes/26+1))
	b.WriteString(`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}`)
	if withNamespace {
		b.WriteString(`,"nexus":{"ext":{"openai":{"responses":{"id":"resp_1","status":"completed"}}}}`)
	}
	b.WriteString(`}`)
	return []byte(b.String())
}

func benchStrip(b *testing.B, payloadBytes int, withNamespace bool) {
	body := benchBody(payloadBytes, withNamespace)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = Strip(body)
	}
}

func BenchmarkStrip_Clean_512B(b *testing.B)  { benchStrip(b, 512, false) }
func BenchmarkStrip_Clean_4KB(b *testing.B)   { benchStrip(b, 4<<10, false) }
func BenchmarkStrip_Clean_64KB(b *testing.B)  { benchStrip(b, 64<<10, false) }
func BenchmarkStrip_Carrier_4KB(b *testing.B) { benchStrip(b, 4<<10, true) }

// The pre-check must not change the answer, only the cost.
func TestStrip_CleanBodyIsByteIdentical(t *testing.T) {
	for _, in := range []string{
		`{"id":"x","choices":[]}`,
		`{}`,
		`not json at all`,
		``,
		// A caller's own text containing the word must survive: the pre-check
		// looks for the quoted key, not the word.
		`{"choices":[{"message":{"content":"what is nexus for?"}}]}`,
	} {
		if got := string(Strip([]byte(in))); got != in {
			t.Errorf("Strip(%q) = %q, want it untouched", in, got)
		}
	}
}

// A `nexus` property inside a caller-authored schema is the caller's data, at
// any depth. Only the root key is ours.
func TestStrip_LeavesCallerAuthoredNexusKeysAlone(t *testing.T) {
	in := []byte(`{"model":"m","nexus":{"ext":{"openai":{"a":1}}},` +
		`"tools":[{"function":{"parameters":{"type":"object","properties":` +
		`{"nexus":{"type":"string"},"nexus_id":{"type":"string"}},"required":["nexus"]}}}]}`)
	out := Strip(in)
	if strings.Contains(string(out), `"ext"`) {
		t.Errorf("root namespace survived: %s", out)
	}
	for _, want := range []string{`"nexus":{"type":"string"}`, `"nexus_id"`, `"required":["nexus"]`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("caller schema lost %s: %s", want, out)
		}
	}
}

// A body that carries the token but is not JSON reaches the delete, which
// cannot resolve a path in it. Returning the body unchanged is the right
// answer: this function removes our key, it does not repair a malformed
// request, and a caller who sent unparseable bytes gets the upstream's own
// verdict on them rather than ours.
func TestStrip_AMalformedBodyCarryingTheTokenIsReturnedUnchanged(t *testing.T) {
	for _, in := range []string{
		`{"nexus":{"ext":{"a":1}}`, // truncated
		`"nexus" is all there is`,  // not an object at all
		`{"nexus":}`,               // syntactically broken value
	} {
		if got := string(Strip([]byte(in))); got != in {
			t.Errorf("Strip(%q) = %q — a malformed body must come back byte-identical", in, got)
		}
	}
}

// A real unsupported field on a real body warns exactly once.
//
// This used to open with an empty-body arm asserting silence, and that arm could
// not fail: gjson.ParseBytes(nil).ForEach yields no keys whether the guard it
// existed to protect is there or not, and removing the guard leaves both the
// warning count AND the allocation count unchanged. An assertion with no input
// that can break it is not a weak test, it is not a test — so it is gone rather
// than rewritten.
func TestScanUnsupported_AnUnsupportedFieldWarnsOnce(t *testing.T) {
	ResetWarnSeenForTest()
	ScanUnsupported("openai", []byte(`{"model":"m","surprise":1}`), map[string]struct{}{"model": {}})
	if n := warnCount(); n != 1 {
		t.Errorf("an unsupported field recorded %d warning(s), want 1", n)
	}
	ResetWarnSeenForTest()
}

func warnCount() int {
	n := 0
	seenWarn.Range(func(_, _ any) bool { n++; return true })
	return n
}
