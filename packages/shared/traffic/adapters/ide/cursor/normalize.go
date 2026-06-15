package cursor

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// Normalize implements normalize.Normalizer for the Cursor IDE backend.
// Cursor speaks two wire formats — JSON on relay paths and Connect-RPC +
// protobuf on native chat endpoints. JSON is OpenAI Chat shaped and
// delegates to the shared full-fidelity OpenAI Chat codec (DetectedSpec
// re-stamped with the adapter ID for per-host provenance); protobuf is
// decoded by extract.ConnectRPCProtobufDetector (which also handles the
// same wire shape on any host without a dedicated adapter) and the
// adapterID is stamped onto the result for precise Tier 1 audit
// attribution.
//
// Protobuf bodies are not text — the BodyView is overridden with a BinaryRef
// so the UI's Raw tab shows size + Content-Type metadata only.
func (a *Adapter) Normalize(ctx context.Context, raw []byte, meta normalize.Meta) (normalize.NormalizedPayload, error) {
	if len(raw) == 0 {
		return normalize.NormalizedPayload{}, normalize.ErrUnsupported
	}

	// JSON relay path — Cursor's HTTP/JSON endpoints carry
	// OpenAI-compatible chat shapes. Delegate to the shared codec;
	// decode failures propagate so the Coordinator falls through to
	// Tier 2 / Tier 3.
	if looksLikeJSON(raw) {
		p, err := codecs.SharedOpenAIChat().Normalize(ctx, raw, meta)
		if err != nil {
			return p, err
		}
		p.DetectedSpec = adapterID
		return p, nil
	}

	// Agent-service path (/agent.v1.AgentService/Run) — the conversation rides
	// as OpenAI-compat / Lexical JSON embedded in per-frame gzip-compressed
	// connect-RPC frames, a shape the generic GetChatRequest/StreamChatResponse
	// detector does not understand (it walked the compressed bytes and produced
	// a stray "j"). Decode it here; only fall through to the generic detector
	// when nothing was recovered.
	if isAgentRunPath(meta.EndpointPath) {
		if conv, ok := decodeAgentRunBody(raw); ok {
			return buildAgentRunPayload(conv, raw, meta), nil
		}
	}

	// Protobuf / Connect-RPC path — delegate to the Tier-2 detector
	// for the actual byte-level decode, then stamp adapter identity.
	// Decode is intentionally NOT gated behind LooksLike: the bare-protobuf
	// response fallback (StreamChatResponse without envelope framing) is a
	// legitimate wire shape LooksLike rejects. Feeding arbitrary binary into
	// the frame walk is safe because ReadConnectRPCFrame caps the declared
	// frame length before allocating.
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

// buildAgentRunPayload turns a decoded agent-service conversation into a
// Tier-1 ai-chat payload. The structured messages come from the embedded JSON;
// the BodyView stays a BinaryRef because the wire body is gzip-compressed
// protobuf, not text. Direction selects request vs response framing so the
// audit row reads correctly on both sides.
func buildAgentRunPayload(conv agentRunConversation, raw []byte, meta normalize.Meta) normalize.NormalizedPayload {
	det := extract.ChatDetection{
		SpecID:          adapterID,
		IsResponse:      meta.Direction == normalize.DirectionResponse,
		IsStream:        true,
		Model:           conv.Model,
		MessageRoles:    conv.Roles,
		MessageContents: conv.Contents,
		Confidence:      1.0,
	}
	payload := extract.BuildPayload(det, raw, "")
	payload.DetectedSpec = adapterID
	payload.HTTP = &normalize.HTTPPayload{
		BodyView: &normalize.HTTPBodyView{
			BinaryRef: &normalize.BinaryRef{
				Size:        int64(len(raw)),
				ContentType: "application/connect+proto",
			},
		},
	}
	return payload
}

// Sanity: keep the Normalize signature in lock-step with the
// normalize.Normalizer interface contract.
var _ normalize.Normalizer = (*Adapter)(nil)
