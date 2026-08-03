package canonicalbridge

// rerank.go — the rerank-kind lane of the bridge. Unlike every other lane,
// the canonical shape is NOT OpenAI: OpenAI ships no rerank API, so the
// de-facto industry standard (Cohere /v2/rerank: query + documents) is the
// canonical, and the sole rerank ingress (/v1/rerank) carries a Cohere-shaped
// body (see provider-adapter-architecture.md §3a for the documented
// exception). Canonicalize is therefore validation + identity over a Cohere
// body; the Voyage leg translates canonical → Voyage wire (top_n → top_k).

import (
	"fmt"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// rerankMaxDocuments bounds the canonical `documents` array (billing-DoS
// guard: one request cannot multiply upstream rerank spend without bound).
// 1000 aligns with the observed provider ceilings (Cohere/Voyage clamp near
// this) and the generative request-size discipline.
const rerankMaxDocuments = 1000

// RerankRoutable reports whether rerank traffic from ingress may route to a
// target. The rerank canonical ingress is Cohere-shaped only. v1 opens two
// target legs:
//   - FormatCohere — native passthrough (canonical IS the Cohere wire);
//   - FormatVoyage — the self-built canonical→Voyage translation codec.
//
// Every other format is deliberately NOT routable — each would be a dead
// failover leg the dispatch cannot serve (no rerank encode case), and
// advertising it silently poisons failover.
func (b *Bridge) RerankRoutable(ingress, target provcore.Format) bool {
	if ingress != provcore.FormatCohere {
		return false
	}
	switch target {
	case provcore.FormatCohere:
		return true
	case provcore.FormatVoyage:
		_, ok := b.codecs[target]
		return ok
	}
	return false
}

// RerankWireShapeForTarget returns the wire shape the rerank kind dispatches
// to for a target format: the Cohere rerank wire for a Cohere target (native
// passthrough), the Voyage rerank wire for a Voyage target. Formats with no
// opened rerank leg return WireShapeNone — RerankRoutable gates them out of
// routing, so a None reaching a codec is a programming error surfaced loudly
// by the codec's shape gate. Consumed by BOTH the prepare stage (primary
// target) and the executor call-time wire-shape rewrite (every failover
// target), so the two sites cannot drift.
func (b *Bridge) RerankWireShapeForTarget(target provcore.Format) typology.WireShape {
	switch target {
	case provcore.FormatCohere:
		return typology.WireShapeCohereRerank
	case provcore.FormatVoyage:
		return typology.WireShapeVoyageRerank
	}
	return typology.WireShapeNone
}

// IngressRerankToCanonical converts a client ingress rerank body to the
// canonical Cohere-shaped rerank JSON. The only rerank ingress is
// Cohere-shaped, so this is validation + identity. It runs on the
// cross-format lane (prepare stage for the primary target, IngressRerankToWire
// for failover targets).
//
// Validation (each rejection names the field and the resolved provider/model
// — routing can rewrite the model mid-flight, so an anonymous 400 is a support
// ticket):
//   - `model` must be a non-empty string.
//   - `query` must be a non-empty string.
//   - `documents` must be an array of 1..rerankMaxDocuments non-empty strings
//     (object documents are not accepted at v1). Every element is scannable
//     text; the compliance extractor scans each, so a non-string shape that
//     the codec would have to guess how to collapse is rejected here.
//   - `top_n`, if present, must be a positive integer (clamped to the document
//     count downstream — providers clamp, we mirror, so over-length is not a
//     400).
func (b *Bridge) IngressRerankToCanonical(ingress provcore.Format, body []byte, ct provcore.CallTarget) ([]byte, error) {
	if ingress != provcore.FormatCohere {
		return nil, fmt.Errorf("canonicalbridge: ingress format %q has no rerank canonical (the rerank ingress is Cohere-shaped only)", ingress)
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, fmt.Errorf("rerank body must be a JSON object")
	}
	resolved := fmt.Sprintf("%s (%s)", ct.Format, ct.ProviderModelID)
	if m := root.Get("model"); m.Type != gjson.String || m.Str == "" {
		return nil, fmt.Errorf("field %q must be a non-empty string for the resolved rerank provider %s", "model", resolved)
	}
	if q := root.Get("query"); q.Type != gjson.String || q.Str == "" {
		return nil, fmt.Errorf("field %q must be a non-empty string for the resolved rerank provider %s", "query", resolved)
	}
	docs := root.Get("documents")
	if !docs.IsArray() {
		return nil, fmt.Errorf("field %q must be an array of strings for the resolved rerank provider %s", "documents", resolved)
	}
	elems := docs.Array()
	if len(elems) < 1 || len(elems) > rerankMaxDocuments {
		return nil, fmt.Errorf("field %q must have 1..%d entries for the resolved rerank provider %s", "documents", rerankMaxDocuments, resolved)
	}
	for i, d := range elems {
		if d.Type != gjson.String || d.Str == "" {
			return nil, fmt.Errorf("field %q[%d] must be a non-empty string for the resolved rerank provider %s", "documents", i, resolved)
		}
	}
	if n := root.Get("top_n"); n.Exists() {
		f := n.Float()
		v := int(f)
		if n.Type != gjson.Number || float64(v) != f || v < 1 {
			return nil, fmt.Errorf("field %q must be a positive integer for the resolved rerank provider %s", "top_n", resolved)
		}
	}
	return body, nil
}

// IngressRerankToWire mirrors [Bridge.IngressImagesToWire] for the rerank
// kind: canonical → target wire via the target codec. It internally invokes
// IngressRerankToCanonical FIRST, so the validation above binds on the
// executor failover lane too — without this a request rejected on a Cohere
// primary could reach the Voyage wire unvalidated via failover.
func (b *Bridge) IngressRerankToWire(ingress, target provcore.Format, body []byte, ct provcore.CallTarget) (wireBody []byte, rewrites []string, err error) {
	if ingress == target {
		return body, nil, nil
	}
	canon, err := b.IngressRerankToCanonical(ingress, body, ct)
	if err != nil {
		return nil, nil, err
	}
	codec, ok := b.codecs[target]
	if !ok || codec == nil {
		return nil, nil, fmt.Errorf("canonicalbridge: no codec for format %q", target)
	}
	encRes, err := codec.EncodeRequest(b.RerankWireShapeForTarget(target), canon, ct)
	if err != nil {
		return nil, nil, err
	}
	return encRes.Body, encRes.Rewrites, nil
}
