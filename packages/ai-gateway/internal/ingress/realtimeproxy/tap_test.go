// Tests for the realtime tap: event classification, usage extraction with
// clamping and cached-attribution rules, and error-code trimming.
//
// Named failure modes: malformed frames classify TapNone (relay is
// verbatim regardless); a response.done without usage reports ok=false
// (metering skipped, never fabricated); hostile negative/absurd token
// magnitudes clamp; absent cached_tokens_details attributes cached tokens
// to TEXT (audio-attribution would overbill); error messages never leak —
// only code/type, trimmed.
package realtimeproxy

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClassifyServerEvent(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  TapKind
	}{
		{"response_created", `{"type":"response.created","response":{}}`, TapResponseCreated},
		{"response_done", `{"type":"response.done","response":{"usage":{}}}`, TapResponseDone},
		{"error", `{"type":"error","error":{"code":"x"}}`, TapError},
		{"audio_delta_opaque", `{"type":"response.output_audio.delta","delta":"aGk="}`, TapNone},
		{"session_created_opaque", `{"type":"session.created"}`, TapNone},
		{"missing_type", `{"foo":1}`, TapNone},
		{"not_json", `not json at all`, TapNone},
		{"empty", ``, TapNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyServerEvent([]byte(tc.frame)); got != tc.want {
				t.Fatalf("ClassifyServerEvent(%q) = %v, want %v", tc.frame, got, tc.want)
			}
		})
	}
}

func TestExtractUsage_FullSplit(t *testing.T) {
	frame := `{"type":"response.done","response":{"usage":{
		"total_tokens":9999,
		"input_tokens":3800,
		"output_tokens":1700,
		"input_token_details":{"text_tokens":1500,"audio_tokens":2300,"cached_tokens":800,
			"cached_tokens_details":{"text_tokens":500,"audio_tokens":300}},
		"output_token_details":{"text_tokens":200,"audio_tokens":1500}}}}`
	u, ok := ExtractUsage([]byte(frame))
	if !ok {
		t.Fatal("usage present but ok=false")
	}
	want := Usage{
		TextInput: 1000, CachedTextRead: 500, // 1500 total text − 500 cached
		AudioInput: 2000, CachedAudioRead: 300, // 2300 total audio − 300 cached
		TextOutput: 200, AudioOutput: 1500,
	}
	if u != want {
		t.Fatalf("usage = %+v, want %+v", u, want)
	}
	if u.TotalInput() != 3800 {
		t.Fatalf("TotalInput = %d, want 3800 (aggregate prompt stamp = total incl. cached)", u.TotalInput())
	}
	if u.TotalOutput() != 1700 {
		t.Fatalf("TotalOutput = %d, want 1700", u.TotalOutput())
	}
}

func TestExtractUsage_NoCachedDetails_AttributesToText(t *testing.T) {
	frame := `{"type":"response.done","response":{"usage":{
		"input_token_details":{"text_tokens":1000,"audio_tokens":2000,"cached_tokens":600},
		"output_token_details":{"text_tokens":10,"audio_tokens":20}}}}`
	u, ok := ExtractUsage([]byte(frame))
	if !ok {
		t.Fatal("ok=false")
	}
	// All 600 cached tokens go to the TEXT cache bucket (never audio).
	if u.CachedTextRead != 600 || u.CachedAudioRead != 0 {
		t.Fatalf("cached split = text %d / audio %d, want 600 / 0", u.CachedTextRead, u.CachedAudioRead)
	}
	if u.TextInput != 400 || u.AudioInput != 2000 {
		t.Fatalf("uncached split = text %d / audio %d, want 400 / 2000", u.TextInput, u.AudioInput)
	}
}

func TestExtractUsage_MissingUsage(t *testing.T) {
	for _, frame := range []string{
		`{"type":"response.done","response":{}}`,
		`{"type":"response.done"}`,
		`garbage`,
	} {
		if _, ok := ExtractUsage([]byte(frame)); ok {
			t.Fatalf("frame %q: ok=true for absent usage; metering must be skipped, not fabricated", frame)
		}
	}
}

func TestExtractUsage_HostileMagnitudes(t *testing.T) {
	frame := `{"type":"response.done","response":{"usage":{
		"input_token_details":{"text_tokens":-500,"audio_tokens":999999999999,"cached_tokens":-1},
		"output_token_details":{"text_tokens":200000001,"audio_tokens":-7}}}}`
	u, ok := ExtractUsage([]byte(frame))
	if !ok {
		t.Fatal("ok=false")
	}
	want := Usage{
		TextInput: 0, CachedTextRead: 0, // negative totals clamp to 0
		AudioInput: maxTokenMagnitude, CachedAudioRead: 0,
		TextOutput: maxTokenMagnitude, AudioOutput: 0,
	}
	if u != want {
		t.Fatalf("hostile usage = %+v, want clamped %+v", u, want)
	}
}

func TestExtractUsage_CachedExceedsTotal_ClampsUncached(t *testing.T) {
	// Inconsistent upstream: cached_text > text total. Uncached must clamp
	// to 0, never negative (a negative unit would corrupt the cost row).
	frame := `{"type":"response.done","response":{"usage":{
		"input_token_details":{"text_tokens":100,"audio_tokens":50,"cached_tokens":400,
			"cached_tokens_details":{"text_tokens":300,"audio_tokens":100}},
		"output_token_details":{}}}}`
	u, _ := ExtractUsage([]byte(frame))
	if u.TextInput != 0 || u.AudioInput != 0 {
		t.Fatalf("uncached = text %d / audio %d, want 0 / 0 (clamped)", u.TextInput, u.AudioInput)
	}
	if u.CachedTextRead != 300 || u.CachedAudioRead != 100 {
		t.Fatalf("cached = text %d / audio %d, want 300 / 100", u.CachedTextRead, u.CachedAudioRead)
	}
}

func TestExtractErrorCode(t *testing.T) {
	t.Run("code_wins_over_type", func(t *testing.T) {
		frame := `{"type":"error","error":{"type":"invalid_request_error","code":"rate_limit_exceeded","message":"secret user text"}}`
		if got := ExtractErrorCode([]byte(frame)); got != "rate_limit_exceeded" {
			t.Fatalf("got %q, want code field", got)
		}
	})
	t.Run("type_fallback", func(t *testing.T) {
		frame := `{"type":"error","error":{"type":"server_error","message":"boom"}}`
		if got := ExtractErrorCode([]byte(frame)); got != "server_error" {
			t.Fatalf("got %q, want type fallback", got)
		}
	})
	t.Run("never_message", func(t *testing.T) {
		frame := `{"type":"error","error":{"message":"the user said: my SSN is 123"}}`
		if got := ExtractErrorCode([]byte(frame)); got != "" {
			t.Fatalf("got %q, want empty — message text must never surface", got)
		}
	})
	t.Run("trims_128_ascii", func(t *testing.T) {
		long := strings.Repeat("x", 300)
		frame := `{"type":"error","error":{"code":"` + long + `"}}`
		if got := ExtractErrorCode([]byte(frame)); len(got) != 128 {
			t.Fatalf("len = %d, want 128", len(got))
		}
	})
	t.Run("trims_on_rune_boundary", func(t *testing.T) {
		// 127 ASCII + a 3-byte rune straddling the 128-byte cut: a naive
		// code[:128] would leave one byte of the rune (invalid UTF-8); the
		// rune-safe trim drops it, yielding the 127-byte valid prefix.
		code := strings.Repeat("x", 127) + "世" // 127 + 3 = 130 bytes
		frame := `{"type":"error","error":{"code":"` + code + `"}}`
		got := ExtractErrorCode([]byte(frame))
		if got != strings.Repeat("x", 127) {
			t.Fatalf("got %q (len %d); want the 127-byte prefix with the split rune dropped", got, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("result is not valid UTF-8: %q", got)
		}
	})
}
