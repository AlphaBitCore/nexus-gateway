// Package realtimeproxy holds the pure session machinery for the realtime
// voice WebSocket relay: classification of the few server events the relay
// taps, usage extraction from response.done, per-session accounting state,
// and the close-reason → audit-error-code mapping. It performs no I/O and
// holds no HTTP handler state — the relay handler in the proxy package
// drives it.
//
// The relay is parse-and-forward: frames are forwarded verbatim FIRST and
// the tap runs on the already-forwarded copy, so nothing in this package
// may mutate a frame or block the pump on anything but its own locks.
package realtimeproxy

import (
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// TapKind names the provider→client event types the relay taps. Every
// other event — and ALL client→provider frames — forwards opaquely.
type TapKind int

const (
	// TapNone — not a tapped event; forward opaque.
	TapNone TapKind = iota
	// TapResponseCreated — starts the per-response wall clock.
	TapResponseCreated
	// TapResponseDone — carries usage; drives the per-response metering row.
	TapResponseDone
	// TapError — provider error event; its code (never the free-text
	// message) is recorded on the session accounting state.
	TapError
)

// ClassifyServerEvent inspects a provider→client frame's "type" field.
// A malformed frame (not JSON, missing type) classifies TapNone — the
// relay forwards it verbatim regardless; classification is metering-only.
func ClassifyServerEvent(frame []byte) TapKind {
	switch gjson.GetBytes(frame, "type").String() {
	case "response.created":
		return TapResponseCreated
	case "response.done":
		return TapResponseDone
	case "error":
		return TapError
	}
	return TapNone
}

// maxTokenMagnitude caps every extracted token count. A hostile or
// compromised upstream must not poison cost rows or quota counters with
// absurd magnitudes; 100M tokens is far beyond any real 60-minute session.
const maxTokenMagnitude = 100_000_000

// Usage is one response's token split, mapped from response.done.usage's
// input/output_token_details. All fields are clamped to [0, 100M]; the
// uncached fields are derived (total − cached, clamped ≥ 0).
type Usage struct {
	TextInput       int // UNCACHED text input tokens
	CachedTextRead  int
	AudioInput      int // UNCACHED audio input tokens
	CachedAudioRead int
	TextOutput      int
	AudioOutput     int
}

// ExtractUsage reads the token splits from a response.done frame. ok is
// false when the frame carries no usage object at all (the caller WARNs,
// deduped, and skips metering — fail-open on metering, never on relay).
//
// When the provider omits cached_tokens_details, ALL cached tokens
// attribute to the TEXT cache: attributing to audio would overbill
// (audio cache rates are ≥ text cache rates); text-attribution underbills
// at most the audio/text cache-rate delta.
func ExtractUsage(frame []byte) (Usage, bool) {
	usage := gjson.GetBytes(frame, "response.usage")
	if !usage.Exists() {
		return Usage{}, false
	}
	in := usage.Get("input_token_details")
	out := usage.Get("output_token_details")

	textIn := clampTokens(in.Get("text_tokens").Int())
	audioIn := clampTokens(in.Get("audio_tokens").Int())
	cached := clampTokens(in.Get("cached_tokens").Int())

	var cachedText, cachedAudio int
	if det := in.Get("cached_tokens_details"); det.Exists() {
		cachedText = clampTokens(det.Get("text_tokens").Int())
		cachedAudio = clampTokens(det.Get("audio_tokens").Int())
	} else {
		cachedText, cachedAudio = cached, 0
	}

	return Usage{
		TextInput:       clampFloor(textIn - cachedText),
		CachedTextRead:  cachedText,
		AudioInput:      clampFloor(audioIn - cachedAudio),
		CachedAudioRead: cachedAudio,
		TextOutput:      clampTokens(out.Get("text_tokens").Int()),
		AudioOutput:     clampTokens(out.Get("audio_tokens").Int()),
	}, true
}

// TotalInput is the row's aggregate prompt-token stamp: total input
// including cached — the same meaning PromptTokens carries on every other
// traffic_event row.
func (u Usage) TotalInput() int {
	return u.TextInput + u.CachedTextRead + u.AudioInput + u.CachedAudioRead
}

// TotalOutput is the row's aggregate completion-token stamp.
func (u Usage) TotalOutput() int { return u.TextOutput + u.AudioOutput }

// ExtractErrorCode returns the provider error's machine identity — code
// when present, else type — truncated to 128 bytes. The free-text message
// is NEVER returned: provider error messages routinely quote user input,
// and the audit error columns sit outside the body-capture redaction
// machinery.
func ExtractErrorCode(frame []byte) string {
	code := gjson.GetBytes(frame, "error.code").String()
	if code == "" {
		code = gjson.GetBytes(frame, "error.type").String()
	}
	if len(code) > 128 {
		// Trim on a rune boundary: a naive code[:128] can split a multi-byte
		// UTF-8 sequence and leave an invalid trailing fragment in the audit
		// column. Drop trailing bytes until the prefix is valid UTF-8.
		code = code[:128]
		for len(code) > 0 && !utf8.ValidString(code) {
			code = code[:len(code)-1]
		}
	}
	return code
}

func clampTokens(v int64) int {
	if v < 0 {
		return 0
	}
	if v > maxTokenMagnitude {
		return maxTokenMagnitude
	}
	return int(v)
}

func clampFloor(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
