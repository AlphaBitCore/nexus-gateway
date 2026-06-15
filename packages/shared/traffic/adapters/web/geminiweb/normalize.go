package geminiweb

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// Normalize implements normalize.Normalizer for gemini.google.com
// (Gemini consumer web). Three wire formats are handled:
//
//  1. Google batchexecute envelope (f.req= form-urlencoded request /
//     )]}'-prefixed chunked JSON response) — decoded by
//     extract.BatchExecuteDetector.
//  2. JSON-shape Gemini-API-compatible bodies — delegated to the shared
//     full-fidelity Gemini codec with DetectedSpec re-stamped for
//     per-host provenance.
//  3. Anything else — ErrUnsupported, Coordinator falls to Tier 3.
func (a *Adapter) Normalize(ctx context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	if len(raw) == 0 {
		return normalize.NormalizedPayload{}, normalize.ErrUnsupported
	}

	// Try the batchexecute detector first — gemini.google.com's
	// primary chat surface speaks this format on every request.
	d := extract.BatchExecuteDetector{}
	if d.LooksLike(raw) {
		if det, ok := d.Decode(raw, string(meta.Direction)); ok {
			det.SpecID = adapterID
			payload := extract.BuildPayload(det, raw, "")
			// Form-encoded request bodies and chunked response bodies
			// ARE human-readable; keep BodyView.Text intact (set by
			// BuildPayload from raw). The Raw tab can show the
			// encoded form / chunked stream for ops verification.
			return payload, nil
		}
	}

	// JSON-shape Gemini-API-compatible fallback (defensive — if
	// Gemini ever migrates the web client to a JSON body). Decode
	// failures propagate so the Coordinator falls through.
	if looksLikeJSON(raw) {
		p, err := codecs.SharedGeminiGenerate().Normalize(ctx, raw, meta)
		if err != nil {
			return p, err
		}
		p.DetectedSpec = adapterID
		return p, nil
	}

	return normalize.NormalizedPayload{}, normalize.ErrUnsupported
}

// looksLikeJSON is a cheap byte sniff — first non-whitespace byte is
// `{` or `[`.
func looksLikeJSON(raw []byte) bool {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

var _ normalize.Normalizer = (*Adapter)(nil)
