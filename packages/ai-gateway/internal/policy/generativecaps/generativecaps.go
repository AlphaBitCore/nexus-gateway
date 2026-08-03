// Package generativecaps holds the built-in, per-endpoint-kind resource caps
// for expensive generative endpoints (image, TTS, video today; realtime as it
// lands). It implements e88 NFR-4: "Expensive generative endpoints carry
// built-in (not admin-knob) stricter rate/concurrency/size caps."
//
// The caps are code defaults, env-overridable for operators who must tune them,
// never per-VK or per-route admin config — a single leaked or abusive virtual
// key must not turn a per-call-priced generative endpoint into an unbounded
// billing-DoS out of the box. One registry keyed by typology.EndpointKind means
// every generative kind inherits the mechanism by adding one row, not a new
// per-modality gate.
package generativecaps

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Caps is the built-in resource bound for one generative endpoint kind.
type Caps struct {
	// MaxConcurrentPerVK bounds simultaneous in-flight requests of this kind
	// per virtual key. 0 = unlimited (no concurrency cap for the kind).
	MaxConcurrentPerVK int
	// MaxRequestBytes is a per-kind request-body ceiling, tighter than the
	// global MaxRequestBytes. 0 = fall back to the global cap.
	MaxRequestBytes int64
}

// defaults are the built-in per-kind caps. Rationale (see e88-s4 SDD):
//   - image bodies are small JSON prompts, so 256 KiB is >100× a real prompt
//     yet shuts a body-flood; 4 concurrent bounds a leaked VK's parallel spend
//     while allowing a real batch UI.
//   - tts is cheaper per call → higher concurrency, same small-JSON ceiling.
//   - video is the most expensive per call → tightest concurrency. Its
//     MaxRequestBytes is MULTIPART-appropriate (16 MiB): the submit body may
//     carry an optional input_reference image file part alongside the small
//     text fields, so the small-JSON 256 KiB ceiling would reject legitimate
//     submits. The row ships ahead of the /v1/videos route (latent until the
//     route registers); the contract is the stt one — the parallel video
//     handler enforces this ceiling itself (http.MaxBytesReader before the
//     multipart parse), never the JSON readBody clamp. The concurrency row
//     bounds in-flight HTTP requests, not provider renders — the render
//     bound is the video handler's non-terminal-jobs cap.
//
// realtime counts long-lived WebSocket SESSIONS, not HTTP requests: the
// relay handler acquires at upgrade and releases at session close, so the
// concurrency row bounds simultaneous live voice sessions per VK (the most
// expensive billing surface — $32–64/1M audio tokens, 60-min sessions;
// default 2 matches the most-expensive existing tier, video). Its
// MaxRequestBytes is NOT an HTTP body cap — the upgrade has no body — but
// the per-WS-FRAME ceiling the relay enforces via SetReadLimit on both the
// client and provider connections (the protocol allows ≤15 MB
// input_audio_buffer.append events; 16 MiB leaves envelope headroom). The
// row ships ahead of the GET /v1/realtime route — latent until the relay
// mounts, like video and stt shipped latent. An env override of 0 for the
// frame ceiling means UNLIMITED frames (a memory-DoS surface): the relay
// treats 0 like the concurrency-disable arm — honored, but loud.
//
// stt carries a MULTIPART-appropriate MaxRequestBytes (26 MiB, just over
// OpenAI's 25 MB whisper limit) rather than the small-JSON 256 KiB ceiling — its
// request body IS the audio, not a JSON prompt. The parallel sttproxy handler
// enforces this ceiling itself mid-stream (http.MaxBytesReader before the
// multipart parse); the JSON readBody clamp is gated by generativeJSONWireShape
// and excludes multipart by design, so the row's MaxRequestBytes is the
// authoritative STT upload bound. Concurrency 4 bounds a leaked VK's parallel
// transcription spend while allowing a real batch. The row ships with the STT
// route (unreachable until the route registers, like video shipped latent).
// guardrail is not a generative kind (it produces no content) but it invokes
// the paid AI-Guard LLM judge on caller-supplied text, so a leaked VK could turn
// it into a judge-budget-DoS exactly like a per-call-priced generative endpoint.
// It reuses this registry's per-VK concurrency mechanism as its primary
// budget-exhaustion bound (v1 has no hard spend ceiling — see e90-s1). Concurrency
// 4 bounds a leaked VK's parallel judge spend while allowing a real batch; the
// 1 MiB body ceiling matches the classify endpoint (a guardrail payload may be a
// full completion but is bounded, and the judge prompt is separately truncated by
// the classify path). The row ships with the /v1/guardrail route.
var defaults = map[typology.EndpointKind]Caps{
	typology.EndpointKindImageGeneration: {MaxConcurrentPerVK: 4, MaxRequestBytes: 256 << 10},
	typology.EndpointKindTTS:             {MaxConcurrentPerVK: 8, MaxRequestBytes: 256 << 10},
	typology.EndpointKindVideoGeneration: {MaxConcurrentPerVK: 2, MaxRequestBytes: 16 << 20},
	typology.EndpointKindSTT:             {MaxConcurrentPerVK: 4, MaxRequestBytes: 26 << 20},
	typology.EndpointKindGuardrail:       {MaxConcurrentPerVK: 4, MaxRequestBytes: 1 << 20},
	typology.EndpointKindRealtime:        {MaxConcurrentPerVK: 2, MaxRequestBytes: 16 << 20},
}

var (
	resolveOnce sync.Once
	resolved    map[typology.EndpointKind]Caps
)

// resolve builds the effective cap table once per process, applying env
// overrides on top of the built-in defaults. Override keys follow the
// perf-toggle convention:
//
//	AI_GATEWAY_GENERATIVE_CAP_<KIND>_CONCURRENCY   (int, >=0)
//	AI_GATEWAY_GENERATIVE_CAP_<KIND>_MAX_BYTES     (int64, >=0)
//
// where <KIND> is the uppercased EndpointKind (e.g. IMAGE_GENERATION). A garbage
// value keeps the built-in default and logs a WARN — a typo must never silently
// disable a cap (the same posture as the admission gate's env parse).
func resolve() map[typology.EndpointKind]Caps {
	resolveOnce.Do(func() {
		out := make(map[typology.EndpointKind]Caps, len(defaults))
		for kind, c := range defaults {
			key := strings.ToUpper(string(kind))
			c.MaxConcurrentPerVK = envInt("AI_GATEWAY_GENERATIVE_CAP_"+key+"_CONCURRENCY", c.MaxConcurrentPerVK)
			c.MaxRequestBytes = envInt64("AI_GATEWAY_GENERATIVE_CAP_"+key+"_MAX_BYTES", c.MaxRequestBytes)
			out[kind] = c
		}
		resolved = out
	})
	return resolved
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		slog.Warn("generativecaps: env override is not a non-negative int; using built-in default",
			"key", key, "value", v, "default", def)
		return def
	}
	if n == 0 && def > 0 {
		// 0 = unlimited: an operator can disable the cap, but disabling a
		// billing-DoS control is loud, not silent.
		slog.Warn("generativecaps: concurrency cap explicitly disabled (0 = unlimited) — billing-DoS protection off for this kind",
			"key", key)
	}
	return n
}

func envInt64(key string, def int64) int64 {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		slog.Warn("generativecaps: env override is not a non-negative int64; using built-in default",
			"key", key, "value", v, "default", def)
		return def
	}
	return n
}

// Lookup returns the effective caps for an endpoint kind and whether the kind
// is a capped generative kind at all. A non-generative kind (chat, embeddings,
// responses, …) returns (Caps{}, false) so callers skip the concurrency/size
// logic entirely — zero added cost on the dominant path.
func Lookup(kind typology.EndpointKind) (Caps, bool) {
	c, ok := resolve()[kind]
	return c, ok
}
