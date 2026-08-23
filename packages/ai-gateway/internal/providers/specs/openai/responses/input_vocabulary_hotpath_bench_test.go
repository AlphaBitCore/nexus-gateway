package responses

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// Absolute-cost measurement for ExceedsInputVocabulary, plus the adversarial
// input the existing claims do not cover.
//
// The predicate is documented as running up to five times per request (routing,
// cache-prep, bridge, once per failover target), so every number below is
// multiplied by ~5 before it is compared against a request budget.
//
// The recorded claims under verification: text-only 798ns -> 186ns and a 43 KB
// served image 94µs -> 1.48µs. Both are before/after pairs, so the "before"
// has to exist to verify them: vocabularyWalkOnly below is the same walk with
// the lexical gate removed, which is what the function was before the gate was
// added. It lives here only.

var (
	vocabSinkType  string
	vocabSinkFound bool
)

// vocabularyWalkOnly is ExceedsInputVocabulary without the lexical pre-scan —
// the shape the "before" numbers were measured on.
func vocabularyWalkOnly(body []byte) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	if !gjson.ValidBytes(body) {
		return "", false
	}
	items := gjson.GetBytes(body, "input")
	if !items.IsArray() {
		return "", false
	}
	var found string
	items.ForEach(func(_, item gjson.Result) bool {
		content := item.Get("content")
		if !content.IsArray() {
			return true
		}
		content.ForEach(func(_, part gjson.Result) bool {
			t := part.Get("type").String()
			if len(t) < 6 || t[:6] != "input_" {
				return true
			}
			if !responsesInputParts[t] {
				found = t
				return false
			}
			return true
		})
		return found == ""
	})
	return found, found != ""
}

// textOnlyBody is the single-turn prompt: the overwhelmingly common shape.
func textOnlyBody() []byte {
	return []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":[` +
		`{"type":"input_text","text":"Summarise the quarterly report and list the three ` +
		`largest variances against plan, with a one-line explanation for each."}]}],` +
		`"max_output_tokens":1024,"temperature":0.7}`)
}

// servedImageBody carries a base64 image of decoded size `imageBytes` as an
// input_image part — a part this wire DOES serve, so the answer is "nothing to
// do" and the only question is what it costs to say so.
func servedImageBody(imageBytes int) []byte {
	raw := strings.Repeat("\x89PNGimagepayloadbytes", imageBytes/21+1)
	b64 := base64.StdEncoding.EncodeToString([]byte(raw)[:imageBytes])
	var b strings.Builder
	b.WriteString(`{"model":"gpt-5.4","input":[{"role":"user","content":[`)
	b.WriteString(`{"type":"input_text","text":"What is in this image?"},`)
	b.WriteString(`{"type":"input_image","image_url":"data:image/png;base64,`)
	b.WriteString(b64)
	b.WriteString(`"}]}]}`)
	return []byte(b.String())
}

// adversarialBody trips the lexical gate and gives the walk nothing to find:
// the caller's own TEXT quotes an unserved part name. Inside a JSON string the
// quote is escaped as `\"`, and the gate's token is `"input_` — the escape puts
// a backslash BEFORE the quote, not between the quote and the name, so the
// token still matches and the name read runs to the next quote. This is the
// only shape that pays the scan AND the full parse AND returns false.
func adversarialBody(padBytes int) []byte {
	pad := strings.Repeat("context line about the audio pipeline. ", padBytes/39+1)
	var b strings.Builder
	b.WriteString(`{"model":"gpt-5.4","input":[{"role":"user","content":[{"type":"input_text",`)
	b.WriteString(`"text":"The provider rejected the request with Invalid value: \"input_audio\". `)
	b.WriteString(pad)
	b.WriteString(` Why?"}]}]}`)
	return []byte(b.String())
}

// manyPartsBody is a long multi-turn conversation: n input parts spread over
// items. When decoy is false every part is a SERVED type, and the measured
// consequence is that the gate answers from the bytes and the walk never runs —
// so this arm measures the scan over a many-part body, not the walk. The decoy
// variant is what forces the walk to traverse all n parts, and it is the only
// honest "many input parts" cost for the authoritative path.
func manyPartsBody(n int, decoy bool) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"gpt-5.4","input":[`)
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		text := `turn text with a moderate amount of content so the part is not degenerate`
		if decoy && i == 0 {
			// One quoted unserved name in the caller's own prose. The gate hits
			// on part 0 and the walk then traverses every one of the n parts to
			// conclude there is nothing to do — the worst case for this input.
			text = `it failed with Invalid value: \"input_audio\" and I want to know why`
		}
		b.WriteString(`{"role":"` + role + `","content":[{"type":"input_text","text":"` +
			text + `"}]}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func benchVocabulary(b *testing.B, body []byte, wantFound bool, fn func([]byte) (string, bool)) {
	if _, found := fn(body); found != wantFound {
		b.Fatalf("arm precondition failed: found = %v, want %v — the benchmark is measuring "+
			"a different branch than its name claims", found, wantFound)
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		vocabSinkType, vocabSinkFound = fn(body)
	}
}

func BenchmarkVocabulary_TextOnly(b *testing.B) {
	benchVocabulary(b, textOnlyBody(), false, ExceedsInputVocabulary)
}
func BenchmarkVocabulary_TextOnly_WalkOnly(b *testing.B) {
	benchVocabulary(b, textOnlyBody(), false, vocabularyWalkOnly)
}

func BenchmarkVocabulary_ServedImage43KB(b *testing.B) {
	benchVocabulary(b, servedImageBody(43<<10), false, ExceedsInputVocabulary)
}
func BenchmarkVocabulary_ServedImage43KB_WalkOnly(b *testing.B) {
	benchVocabulary(b, servedImageBody(43<<10), false, vocabularyWalkOnly)
}

// The adversarial arm: gate hits, walk finds nothing, both costs paid.
func BenchmarkVocabulary_Adversarial_2KB(b *testing.B) {
	benchVocabulary(b, adversarialBody(2<<10), false, ExceedsInputVocabulary)
}
func BenchmarkVocabulary_Adversarial_2KB_WalkOnly(b *testing.B) {
	benchVocabulary(b, adversarialBody(2<<10), false, vocabularyWalkOnly)
}
func BenchmarkVocabulary_Adversarial_43KB(b *testing.B) {
	benchVocabulary(b, adversarialBody(43<<10), false, ExceedsInputVocabulary)
}

// All parts served: the gate answers and the walk never runs. 0 allocs/op is
// the proof, and it is why this arm is NOT a measurement of the walk.
func BenchmarkVocabulary_ManyParts_100_GateOnly(b *testing.B) {
	benchVocabulary(b, manyPartsBody(100, false), false, ExceedsInputVocabulary)
}
func BenchmarkVocabulary_ManyParts_100_WalkOnly(b *testing.B) {
	benchVocabulary(b, manyPartsBody(100, false), false, vocabularyWalkOnly)
}

// Many parts AND the gate tripped: the full walk over 100 parts, which is the
// real worst case for a long conversation.
func BenchmarkVocabulary_ManyParts_100_Adversarial(b *testing.B) {
	benchVocabulary(b, manyPartsBody(100, true), false, ExceedsInputVocabulary)
}
func BenchmarkVocabulary_ManyParts_400_Adversarial(b *testing.B) {
	benchVocabulary(b, manyPartsBody(400, true), false, ExceedsInputVocabulary)
}

// The true positive, for scale: an actually-unserved part.
func BenchmarkVocabulary_UnservedAudio(b *testing.B) {
	body := []byte(`{"model":"gpt-audio-mini","input":[{"role":"user","content":[` +
		`{"type":"input_text","text":"transcribe"},` +
		`{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`)
	benchVocabulary(b, body, true, ExceedsInputVocabulary)
}

// The adversarial body must genuinely trip the gate — otherwise the arm above
// is a second measurement of the cheap path wearing an alarming name.
func TestAdversarialBodyTripsTheGateAndTheWalkFindsNothing(t *testing.T) {
	body := adversarialBody(2 << 10)
	if !hasUnservedInputToken(body) {
		t.Fatal("the adversarial body does not trip the lexical gate; the arm measures the " +
			"cheap path and its ns/op means nothing")
	}
	if part, found := ExceedsInputVocabulary(body); found {
		t.Fatalf("the walk found %q; this arm must be the gate-hits-walk-finds-nothing case", part)
	}
	// And the served-image body must NOT trip it, or the two arms are the same.
	if hasUnservedInputToken(servedImageBody(4 << 10)) {
		t.Error("the served-image body trips the gate; its arm is not the fast path it claims")
	}
	if hasUnservedInputToken(textOnlyBody()) {
		t.Error("the text-only body trips the gate")
	}
	// The all-served many-part body must NOT reach the walk, and the decoy
	// variant must. Without this the two ManyParts arms could be the same
	// measurement under two names.
	if hasUnservedInputToken(manyPartsBody(100, false)) {
		t.Error("the all-served many-part body trips the gate; the GateOnly arm is misnamed")
	}
	if !hasUnservedInputToken(manyPartsBody(100, true)) {
		t.Fatal("the many-part decoy does not trip the gate; its arm does not measure the walk")
	}
	if part, found := ExceedsInputVocabulary(manyPartsBody(100, true)); found {
		t.Errorf("the many-part decoy walk found %q; it must find nothing", part)
	}
}
