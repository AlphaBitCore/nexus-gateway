package cursor

import (
	"context"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// Normalize implements normalize.Normalizer for the Cursor IDE backend.
// Cursor speaks two wire formats — JSON on relay paths and Connect-RPC +
// protobuf on native chat endpoints. JSON is handed to the multi-spec probe
// via extract.NormalizeForAdapter; protobuf is decoded by
// extract.ConnectRPCProtobufDetector (which also handles the same wire shape
// on any host without a dedicated adapter) and the adapterID is stamped onto
// the result for precise Tier 1 audit attribution.
//
// Protobuf bodies are not text — the BodyView is overridden with a BinaryRef
// so the UI's Raw tab shows size + Content-Type metadata only.
func (a *Adapter) Normalize(_ context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	if len(raw) == 0 {
		return normalize.NormalizedPayload{}, normalize.ErrUnsupported
	}

	// JSON relay path — Cursor's HTTP/JSON endpoints sometimes carry
	// OpenAI-compatible chat shapes. Hand off to the multi-spec probe.
	if looksLikeJSON(raw) {
		return extract.NormalizeForAdapter(raw, meta, extract.AdapterSpecHint{
			AdapterID:     adapterID,
			ReqSpecIDs:    []string{"openai-chat"},
			RespSpecIDs:   []string{"openai-chat-nonstream", "openai-chat-sse"},
			MinConfidence: 0.5,
		})
	}

	// Protobuf / Connect-RPC path — delegate to the Tier-2 detector
	// for the actual byte-level decode, then stamp adapter identity.
	d := extract.ConnectRPCProtobufDetector{}
	det, ok := d.Decode(raw, string(meta.Direction))
	if !ok {
		return normalize.NormalizedPayload{}, normalize.ErrUnsupported
	}
	det.SpecID = adapterID
	payload := extract.BuildPayload(det, raw, "")

	// Protobuf bodies aren't text — override the auto-set BodyView.Text
	// with a BinaryRef so the UI's Raw tab shows size + Content-Type
	// metadata instead of dumping unreadable bytes.
	payload.HTTP = &normalize.HTTPPayload{
		BodyView: &normalize.HTTPBodyView{
			BinaryRef: &normalize.BinaryRef{
				Size:        int64(len(raw)),
				ContentType: "application/connect+proto",
			},
		},
	}
	return payload, nil
}

// Sanity: keep the Normalize signature in lock-step with the
// normalize.Normalizer interface contract.
var _ normalize.Normalizer = (*Adapter)(nil)
