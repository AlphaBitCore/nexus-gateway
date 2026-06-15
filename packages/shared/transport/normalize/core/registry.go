package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// Registry is a goroutine-safe, freezable registry of Normalizer
// implementations indexed by an opaque routing key. Adapters consult
// the registry with a (adapter-type, content-type, endpoint-path) hint;
// the registry returns the most specific normalizer that matches.
//
// Lookup is layered:
//
//  1. Exact key match (e.g. "openai:application/json:/v1/chat/completions").
//  2. AdapterType+endpoint match (e.g. "openai::/v1/chat/completions").
//  3. AdapterType-only match (e.g. "openai").
//  4. Content-type match (e.g. ":application/json:").
//  5. Generic HTTP fallback (registered under key "*:*:*").
//
// Implementations may register themselves under multiple keys when
// they handle several endpoints (e.g. OpenAI Chat handles both
// /v1/chat/completions and /v1/chat/completions/legacy).
type Registry struct {
	mu      sync.RWMutex
	entries map[string]Normalizer
	// tier2 is the pattern-based extraction fallback. It runs AFTER all
	// candidate-keyed lookups (Tier 1) have either ErrUnsupported'd or
	// returned low-Confidence payloads, and BEFORE the *:*:* generic-http
	// catch-all (Tier 3). Nil-safe: when unset, Normalize skips Tier 2
	// and falls straight from Tier 1 to Tier 3.
	tier2  Normalizer
	frozen bool

	// sniffers is the Tier-1.5 walk: Normalizers that also implement
	// Sniffer, offered (in registration order) traffic whose keyed
	// Tier-1 candidates all missed or declined. Most-specific wire
	// shapes register first so a distinctive framing (e.g. an Anthropic
	// `event: message_start` stream) is probed before looser JSON-field
	// discriminators.
	sniffers []Normalizer

	// confidenceThreshold sets the minimum payload.Confidence value a
	// normalizer must report for the Coordinator to claim its output as
	// final. Below the threshold, Normalize falls through to the next
	// tier (or to Tier 2 / Tier 3). Default 0.7. A payload that does NOT
	// set Confidence (zero value) is treated as fully confident (1.0) so
	// normalizers that do not report Confidence terminate the walk on success.
	confidenceThreshold float64
}

// NewRegistry creates an empty registry with the default 0.7 confidence
// threshold for tier-fall-through. Set a different value via
// SetConfidenceThreshold before any Normalize call.
func NewRegistry() *Registry {
	return &Registry{
		entries:             make(map[string]Normalizer),
		confidenceThreshold: 0.7,
	}
}

// SetConfidenceThreshold overrides the per-tier confidence cutoff used by
// the Coordinator (Normalize). Out-of-range values are clamped to [0, 1].
// Concurrency-safe at any point — Normalize reads via atomic-free RLock
// hold, but the threshold is a single float64 read so the rare race is
// benign.
func (r *Registry) SetConfidenceThreshold(t float64) {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		panic("normalize: SetConfidenceThreshold on frozen registry")
	}
	r.confidenceThreshold = t
}

// RegisterTier2 installs the pattern-based fallback normalizer. Called
// once at startup from RegisterDefaultAIBuiltins; idempotent within a
// single registry (overwrite allowed pre-Freeze). Tier 2's job is to
// recognise common chat shapes (OpenAI/Anthropic/Gemini/ChatGPT-web/...)
// by byte-level multi-spec probe when no per-adapter Tier 1 normalizer
// matched the body (or matched but returned low confidence).
func (r *Registry) RegisterTier2(n Normalizer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		panic("normalize: RegisterTier2 on frozen registry")
	}
	r.tier2 = n
}

// RegisterSniffer enrolls n in the Tier-1.5 sniff walk: after every
// keyed Tier-1 candidate has missed or declined a body, the registry
// asks each registered sniffer whether the leading bytes look like its
// wire format, and on a match runs the Tier-1 claim contract with hard
// errors demoted to soft fall-through (sniff evidence is weaker than a
// routing key). Registration order is probe order — register the
// most distinctive wire shape first. n must also implement Sniffer;
// panics otherwise, and on a frozen registry. Re-registering the same
// normalizer is a no-op so builders can wire aliases without
// double-walking.
func (r *Registry) RegisterSniffer(n Normalizer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		panic("normalize: RegisterSniffer on frozen registry")
	}
	if _, ok := n.(Sniffer); !ok {
		panic(fmt.Sprintf("normalize: RegisterSniffer(%q): normalizer does not implement Sniffer", n.ID()))
	}
	for _, existing := range r.sniffers {
		if existing == n {
			return
		}
	}
	r.sniffers = append(r.sniffers, n)
}

// Register adds a normalizer under the given routing key. Panics if
// the registry is frozen or the key is already registered. Use
// Replace to override.
func (r *Registry) Register(key string, n Normalizer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		panic(fmt.Sprintf("normalize: Register(%q) on frozen registry", key))
	}
	if _, exists := r.entries[key]; exists {
		panic(fmt.Sprintf("normalize: duplicate registration for %q", key))
	}
	r.entries[key] = n
}

// Replace overrides an existing normalizer; safe to call on a not-yet-
// frozen registry even when the key was never registered.
func (r *Registry) Replace(key string, n Normalizer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		panic(fmt.Sprintf("normalize: Replace(%q) on frozen registry", key))
	}
	r.entries[key] = n
}

// Freeze prevents further registration.
func (r *Registry) Freeze() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frozen = true
}

// candidateKeys returns the layered lookup keys in priority order.
//
//  1. adapterType + contentType + path — most specific
//  2. adapterType + path
//  3. adapterType only
//  4. path only — catches "/v1/messages", "/v1/chat/completions" etc.
//     regardless of the adapter label, which is critical for cp/agent
//     traffic where the adapter field is a host or a tool name rather
//     than a wire-format identifier
//  5. contentType only
//  6. "*:*:*" generic-http fallback
func (r *Registry) candidateKeys(meta Meta) []string {
	keys := make([]string, 0, 6)
	if meta.AdapterType != "" {
		if meta.ContentType != "" && meta.EndpointPath != "" {
			keys = append(keys, fmt.Sprintf("%s:%s:%s", meta.AdapterType, meta.ContentType, meta.EndpointPath))
		}
		if meta.EndpointPath != "" {
			keys = append(keys, fmt.Sprintf("%s::%s", meta.AdapterType, meta.EndpointPath))
		}
		keys = append(keys, meta.AdapterType)
	}
	if meta.EndpointPath != "" {
		keys = append(keys, fmt.Sprintf("::%s", meta.EndpointPath))
	}
	if meta.ContentType != "" {
		keys = append(keys, fmt.Sprintf(":%s:", meta.ContentType))
	}
	keys = append(keys, "*:*:*")
	return keys
}

// Resolve picks the most specific normalizer for the given meta. The
// returned normalizer may be nil when nothing matches; callers should
// treat that case as ErrUnsupported. Resolve returns the FIRST matching
// entry; if the matched normalizer rejects the body with ErrUnsupported,
// callers should use Normalize() (or call Resolve repeatedly) to walk
// the candidate chain.
func (r *Registry) Resolve(meta Meta) Normalizer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, key := range r.candidateKeys(meta) {
		if n, ok := r.entries[key]; ok {
			return n
		}
	}
	return nil
}

// Normalize is the Coordinator entry point for all callers (Hub audit
// consumer, ai-gateway L3 normalize, ingestors). Internally it runs a
// three-tier model:
//
//	Tier 1 — candidate-keyed lookup (per-adapter normalizers). The
//	         most specific entry wins; falls through on ErrUnsupported
//	         OR when the returned payload's Confidence is below the
//	         registry threshold. Confidence == 0 is treated as 1.0
//	         (normalizers that do not set Confidence terminate the walk
//	         on success).
//
//	Tier 1.5 — sniff walk. Normalizers enrolled via RegisterSniffer are
//	         offered the body (in registration order) when every keyed
//	         candidate missed or declined: each sniffer's LooksLike
//	         probes the leading bytes, and a match runs the Tier-1
//	         claim contract with one difference — a hard parse error is
//	         demoted to soft fall-through (sniff evidence is weaker
//	         than a routing key; see tryClaim). This is how
//	         capture-side traffic whose AdapterType carries a host or
//	         tool name (so no key resolves) still lands on the
//	         full-fidelity codec instead of the pattern probe.
//
//	Tier 2 — pattern-based extraction. Multi-spec probe + SSE walker +
//	         JSON-patch accumulator. Recognises common chat shapes by
//	         byte-level pattern regardless of the producer's adapter_type
//	         label. Wins when the probe returns confidence >= threshold.
//	         Optional — skipped when RegisterTier2 was never called.
//
//	Tier 3 — verbatim catch-all. The *:*:* generic-http entry runs
//	         last and always succeeds (text / json / form / binary
//	         projection). Confidence=1.0 by construction.
//
// Auto-decompresses gzip / zlib / zstd bodies before any tier runs.
// Producers (cp/agent) sometimes capture responses before the transport
// layer decompresses them, which would otherwise leave normalizers
// staring at compressed magic bytes.
func (r *Registry) Normalize(ctx context.Context, raw []byte, meta Meta) (NormalizedPayload, error) {
	if decoded, ok := maybeGunzip(raw); ok {
		raw = decoded
	}

	r.mu.RLock()
	tried := make(map[Normalizer]bool, 4)
	var tier1 []Normalizer
	for _, key := range r.candidateKeys(meta) {
		// "*:*:*" is the Tier-3 catch-all entry; exclude it from Tier-1
		// walk so it always runs in its own dedicated step below.
		if key == "*:*:*" {
			continue
		}
		if n, ok := r.entries[key]; ok && !tried[n] {
			tried[n] = true
			tier1 = append(tier1, n)
		}
	}
	sniffers := r.sniffers
	tier2 := r.tier2
	tier3 := r.entries["*:*:*"]
	threshold := r.confidenceThreshold
	r.mu.RUnlock()

	// stamp NormalizeVersion if a normalizer forgot it.
	stamp := func(p NormalizedPayload) NormalizedPayload {
		if p.NormalizeVersion == "" {
			p.NormalizeVersion = SchemaVersion
		}
		return p
	}
	// effectiveConfidence returns payload.Confidence with zero promoted
	// to 1.0 (normalizers that do not set Confidence claim the result).
	effConf := func(p NormalizedPayload) float64 {
		if p.Confidence == 0 {
			return 1.0
		}
		return p.Confidence
	}

	// bestPartial tracks the highest-confidence soft-fall-through
	// payload seen so far, so if NO tier ever claimed at >= threshold
	// we still return the closest match rather than a blank
	// Unsupported.
	var bestPartial NormalizedPayload
	var bestConf float64

	// tryClaim runs one normalizer under the shared Tier-1 / Tier-1.5
	// claim contract: claim at >= threshold, soft fall-through below it
	// (tracking bestPartial), keep walking on ErrUnsupported. Hard
	// (non-ErrUnsupported) errors split by evidence strength:
	//
	//   - Keyed Tier-1 (demoteHardError=false): stop the whole walk. The
	//     routing key is strong evidence the body IS this normalizer's
	//     wire, so a parse failure means the bytes themselves are broken
	//     — further tiers can't do better with the same malformed bytes.
	//   - Tier-1.5 sniff (demoteHardError=true): demote to soft
	//     fall-through. A LooksLike byte probe is weak evidence — a
	//     foreign protocol that happens to carry the probed marker, or a
	//     truncated body, would otherwise abort the walk and lose the
	//     Tier-2 / Tier-3 structural projection the row could still get.
	//     The errored payload is kept as bestPartial only when it
	//     carries explicit confidence (no zero→1.0 promotion: an errored
	//     zero payload must not outrank real partials).
	//
	// done=true means the walk ends with (payload, err) as the final
	// answer.
	tryClaim := func(n Normalizer, claimMsg string, demoteHardError bool) (NormalizedPayload, error, bool) {
		payload, err := n.Normalize(ctx, raw, meta)
		payload = stamp(payload)
		if err == nil {
			c := effConf(payload)
			// Host selection evidence bypasses the confidence threshold:
			// the threshold exists to catch a routing key that lied about
			// the wire, but an adapter resolved by interception-domain
			// host match IS the source of truth for "this is adapter X
			// traffic" — its decode coverage may legitimately sit below
			// the threshold (single-prompt consumer-web specs extract the
			// prompt and nothing else, ~0.6 by design) and the honest
			// coverage value must reach the row instead of an inflated
			// floor. The payload's SelectionEvidence field carries the
			// same fact to the UI, which renders a host-matched label in
			// place of the numeral.
			if c >= threshold || payload.SelectionEvidence == SelectionEvidenceHost {
				slog.Info(claimMsg,
					"adapter", meta.AdapterType,
					"direction", meta.Direction,
					"path", meta.EndpointPath,
					"protocol", payload.Protocol,
					"detectedSpec", payload.DetectedSpec,
					"kind", payload.Kind,
					"confidence", c,
					"threshold", threshold,
				)
				return payload, nil, true
			}
			// Per-Normalize tier-walk diagnostics are Debug to keep Info
			// volume bounded — at 1k QPS with N tiers walked per call the
			// Info channel would otherwise carry tens of thousands of
			// "below threshold" lines per second. The CLAIM line above
			// (when a tier wins) stays Info because it's the one signal
			// admins act on.
			slog.Debug("normalize: tier1 below threshold, soft fall-through",
				"adapter", meta.AdapterType,
				"direction", meta.Direction,
				"protocol", payload.Protocol,
				"confidence", c,
				"threshold", threshold,
			)
			if c > bestConf {
				bestPartial = payload
				bestConf = c
			}
			// soft fall-through: keep walking this tier, then the next.
			return NormalizedPayload{}, nil, false
		}
		if errors.Is(err, ErrUnsupported) {
			// hard miss: not this normalizer's shape. Keep walking.
			slog.Debug("normalize: tier1 ErrUnsupported, continue walk",
				"adapter", meta.AdapterType,
				"direction", meta.Direction,
			)
			return NormalizedPayload{}, nil, false
		}
		if demoteHardError {
			slog.Warn("normalize: tier1.5 sniff hard error, demoting to fall-through",
				"adapter", meta.AdapterType,
				"direction", meta.Direction,
				"normalizer", n.ID(),
				"error", err,
			)
			if payload.Confidence > bestConf {
				bestPartial = payload
				bestConf = payload.Confidence
			}
			return NormalizedPayload{}, nil, false
		}
		slog.Warn("normalize: tier1 hard error, stopping walk",
			"adapter", meta.AdapterType,
			"direction", meta.Direction,
			"error", err,
		)
		return payload, err, true
	}

	// Tier 1: keyed lookups
	for _, n := range tier1 {
		if payload, err, done := tryClaim(n, "normalize: tier1 CLAIM", false); done {
			return payload, err
		}
	}

	// Tier 1.5: sniff walk. Codecs that recognise their own wire shape
	// claim traffic whose keys resolved nothing usable. Skips
	// normalizers the keyed walk already ran — a sniff cannot improve
	// on the same Normalize call that just declined.
	for _, n := range sniffers {
		if tried[n] {
			continue
		}
		if !n.(Sniffer).LooksLike(raw, meta) {
			continue
		}
		tried[n] = true
		if payload, err, done := tryClaim(n, "normalize: tier1.5 CLAIM (sniff)", true); done {
			return payload, err
		}
	}

	// Tier 2: pattern-based extract
	if tier2 != nil {
		payload, err := tier2.Normalize(ctx, raw, meta)
		payload = stamp(payload)
		if err == nil {
			c := effConf(payload)
			if c >= threshold {
				slog.Info("normalize: tier2 CLAIM (pattern-extract)",
					"adapter", meta.AdapterType,
					"direction", meta.Direction,
					"detectedSpec", payload.DetectedSpec,
					"kind", payload.Kind,
					"confidence", c,
					"threshold", threshold,
				)
				return payload, nil
			}
			slog.Debug("normalize: tier2 below threshold",
				"adapter", meta.AdapterType,
				"direction", meta.Direction,
				"detectedSpec", payload.DetectedSpec,
				"confidence", c,
				"threshold", threshold,
			)
			if c > bestConf {
				bestPartial = payload
				bestConf = c
			}
		} else if !errors.Is(err, ErrUnsupported) {
			slog.Warn("normalize: tier2 hard error", "error", err)
			return payload, err
		}
	}

	// Tier 3: verbatim catch-all (generic-http)
	if tier3 != nil {
		payload, err := tier3.Normalize(ctx, raw, meta)
		payload = stamp(payload)
		if err == nil {
			// generic-http always claims; this is the terminal answer
			// unless an earlier tier had higher confidence (shouldn't
			// happen — generic-http stamps Confidence=1.0 explicitly —
			// but guard anyway).
			if effConf(payload) >= bestConf {
				slog.Info("normalize: tier3 CLAIM (generic-http catch-all)",
					"adapter", meta.AdapterType,
					"direction", meta.Direction,
					"contentType", meta.ContentType,
					"kind", payload.Kind,
				)
				return payload, nil
			}
		} else if !errors.Is(err, ErrUnsupported) {
			slog.Warn("normalize: tier3 hard error", "error", err)
			return payload, err
		}
	}

	// No tier produced a final answer. If any tier had partial output
	// (low confidence but non-error) return that as the best-effort
	// audit row. Otherwise admit Unsupported.
	if bestConf > 0 {
		slog.Debug("normalize: no tier above threshold, returning bestPartial",
			"adapter", meta.AdapterType,
			"direction", meta.Direction,
			"bestConf", bestConf,
			"threshold", threshold,
		)
		return bestPartial, nil
	}
	slog.Warn("normalize: ALL tiers missed, returning ErrUnsupported",
		"adapter", meta.AdapterType,
		"direction", meta.Direction,
		"contentType", meta.ContentType,
		"path", meta.EndpointPath,
	)
	return NormalizedPayload{
			Kind:             KindUnsupported,
			NormalizeVersion: SchemaVersion,
		}, fmt.Errorf("no normalizer for adapter_type=%q content_type=%q path=%q: %w",
			meta.AdapterType, meta.ContentType, meta.EndpointPath, ErrUnsupported)
}

// All returns a snapshot of registered keys (for diagnostics).
func (r *Registry) All() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.entries))
	for k := range r.entries {
		out = append(out, k)
	}
	return out
}
