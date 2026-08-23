package responses

import (
	"strings"
	"testing"
)

func TestExceedsInputVocabulary(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string // "" = does not exceed
	}{
		{
			// The production case: 21 rows, model gpt-audio-mini, upstream
			// rejects the part name. The same model takes audio on
			// /v1/chat/completions, so reporting this lets the gateway send it
			// there instead of forwarding the caller to a refusal.
			name: "audio",
			body: `{"model":"gpt-audio-mini","input":[{"role":"user","content":[` +
				`{"type":"input_text","text":"what is said?"},` +
				`{"type":"input_audio","input_audio":{"data":"AAA","format":"wav"}}]}]}`,
			want: "input_audio",
		},
		{
			// Not observed in production, and covered anyway — the accept-list
			// shape is what makes an unsent part type land OUTSIDE the rule
			// rather than inside a denylist's blind spot.
			name: "video",
			body: `{"input":[{"role":"user","content":[{"type":"input_video","input_video":{}}]}]}`,
			want: "input_video",
		},
		{"text and image and file all served", `{"input":[{"role":"user","content":[` +
			`{"type":"input_text","text":"hi"},` +
			`{"type":"input_image","image_url":"data:image/png;base64,AAA"},` +
			`{"type":"input_file","file_data":"AAA"}]}]}`, ""},
		{
			// A string input is the common single-prompt form. It carries text
			// and nothing else, so it can never exceed.
			name: "string input",
			body: `{"model":"gpt-5.4","input":"hello"}`,
			want: "",
		},
		{
			// Echoed items from a previous turn are not a request for the wire
			// to carry new content. Treating one as unservable would downgrade
			// a conversation that was working.
			name: "assistant output_text is not an input part",
			body: `{"input":[{"role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`,
			want: "",
		},
		{
			name: "function_call_output has no content array",
			body: `{"input":[{"type":"function_call_output","call_id":"c1","output":"42"}]}`,
			want: "",
		},
		{"empty", ``, ""},
		{"not json", `nonsense`, ""},
		{"no input field", `{"model":"gpt-5.4"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, exceeds := ExceedsInputVocabulary([]byte(tc.body))
			if tc.want == "" {
				if exceeds {
					t.Errorf("reported %q as unservable; this wire carries it, and a false "+
						"positive downgrades a request that was working", got)
				}
				return
			}
			if !exceeds {
				t.Fatalf("did not report %q — the request would be forwarded to a wire "+
					"with no part for it and refused upstream", tc.want)
			}
			if got != tc.want {
				t.Errorf("reported %q, want %q", got, tc.want)
			}
		})
	}
}

// The accept-list must be exactly what the vendor enumerates as INPUT parts.
// Restating it here means a future edit that widens the list has to be
// deliberate rather than incidental.
func TestInputVocabulary_IsTheVendorsInputPartList(t *testing.T) {
	want := map[string]bool{"input_text": true, "input_image": true, "input_file": true}
	if len(responsesInputParts) != len(want) {
		t.Fatalf("accept-list = %v, want %v", responsesInputParts, want)
	}
	for k := range want {
		if !responsesInputParts[k] {
			t.Errorf("accept-list is missing %q, which the wire does carry", k)
		}
	}
}

// ExceedsInputVocabulary sits on the /v1/responses hot path and runs once per
// ServesResponses resolution — the routing stage, the cache-prep stage, the
// bridge, and ONCE PER TARGET in the executor's failover loop. So a request
// with two failover targets pays for it five times, and the cost has to be
// known rather than assumed.
//
// The shape that matters is a body carrying a large base64 media part: gjson
// walks the input items, and a 32 KB audio blob is 43 KB of JSON to step over.

func benchVocabBody(b *testing.B, parts string) {
	body := []byte(`{"model":"gpt-audio-mini","input":[{"role":"user","content":[` + parts + `]}]}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _ = ExceedsInputVocabulary(body)
	}
}

func BenchmarkExceedsInputVocabulary_TextOnly(b *testing.B) {
	benchVocabBody(b, `{"type":"input_text","text":"hello there"}`)
}

// The unservable part is found on the SECOND element, so this measures the
// walk rather than an immediate hit.
func BenchmarkExceedsInputVocabulary_AudioAfterText(b *testing.B) {
	blob := strings.Repeat("A", 43000) // ~32 KB of wav, base64
	benchVocabBody(b, `{"type":"input_text","text":"what is said?"},`+
		`{"type":"input_audio","input_audio":{"data":"`+blob+`","format":"wav"}}`)
}

// The worst case for the walk: a large SERVED part, so every byte is stepped
// over and the answer is still "nothing to do".
func BenchmarkExceedsInputVocabulary_LargeServedImage(b *testing.B) {
	blob := strings.Repeat("A", 43000)
	benchVocabBody(b, `{"type":"input_image","image_url":"data:image/png;base64,`+blob+`"}`)
}

// The single-prompt form, which is what most /v1/responses traffic looks like.
func BenchmarkExceedsInputVocabulary_StringInput(b *testing.B) {
	body := []byte(`{"model":"gpt-5.4","input":"hello there"}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, _ = ExceedsInputVocabulary(body)
	}
}

// The lexical pre-scan may fire on a caller's own text, and the walk must then
// answer correctly anyway. This is the hazard that makes the pre-scan a COST
// optimisation rather than a filter: a body whose text merely mentions the
// token must not be downgraded.
func TestExceedsInputVocabulary_CallerTextMentioningTheTokenIsNotAPart(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":[` +
		`{"type":"input_text","text":"why does \"input_audio\" not work on this endpoint?"}]}]}`)
	if !hasUnservedInputToken(body) {
		t.Fatal("the pre-scan did not fire; this case then proves nothing about the walk " +
			"correcting it")
	}
	if got, exceeds := ExceedsInputVocabulary(body); exceeds {
		t.Errorf("reported %q from the caller's own prose — a question about audio is not "+
			"an audio part, and downgrading on it changes the wire for a working request", got)
	}
}
