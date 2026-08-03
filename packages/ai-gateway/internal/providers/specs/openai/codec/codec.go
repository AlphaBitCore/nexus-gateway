// Package codec implements the OpenAI-family identity SchemaCodec. It is
// an internal sub-package of specs/openai; the root package re-exports
// New (as NewIdentityCodec) and ErrorNormalizerInstance via bridge.go.
package codec

import (
	"fmt"

	specerrors "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/errors"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// New returns an OpenAI-family identity SchemaCodec carrying the calling
// sibling's wire-rule Contract. The contract is a required argument by
// design: each of the OpenAI-compatible siblings (OpenAI itself, Azure,
// DeepSeek, Moonshot, Groq, …) constructs its own instance and states its
// per-model rules — or their absence, the zero Contract — on its own
// wiring line. There is no registry or shared default a new sibling could
// silently inherit "supports everything" (or "strips nothing") from.
//
// Family-wide wire behaviour is NOT contract content: the streaming
// stream_options.include_usage injection every OpenAI-chat sibling needs
// for usage extraction lives in the codec type itself, keyed on wire
// shape, so a sibling constructed with an empty contract keeps it.
func New(c Contract) provcore.SchemaCodec { return newIdentity(c) }

// ErrorNormalizerInstance returns the shared OpenAI-style error
// normaliser so OpenAI-compat sibling adapters (DeepSeek, Azure) can
// reuse it without importing unexported symbols.
func ErrorNormalizerInstance() provcore.ErrorNormalizer { return specerrors.ErrorNormalizer{} }

// identityCodec is the SchemaCodec for the OpenAI wire family. OpenAI is
// the canonical wire format, so neither entry point translates: both owe
// only the same-spec differential — the resolved-model stamp, the
// contract's per-model rules, and the streaming back-fills. EncodeRequest
// (canonical door) and RewriteNative (native door) delegate to one shared
// differential per wire shape, so the §3a two-door equivalence holds by
// construction.
//
// DecodeResponse is identity for all endpoints; its only real job is to
// extract token usage so the executor's ExecutionResult.Usage is populated
// consistently across every OpenAI-compat sibling. Usage extraction
// delegates to provcore.ExtractUsage (shared/normalize Tier-1 normalizer).
// The alias chain (Kimi flat cached_tokens, DeepSeek prompt_cache_hit_tokens,
// Moonshot prompt_cache_tokens, OpenAI Responses-shape fallbacks) lives in
// shared/normalize/openai_chat.go's extractCanonicalUsage method.
type identityCodec struct {
	chat           ruleSet
	responses      ruleSet
	embeddings     ruleSet
	chatStructural []StructuralRule
}

func newIdentity(c Contract) *identityCodec {
	return &identityCodec{
		chat:           newRuleSet(c.Chat),
		responses:      newRuleSet(c.Responses),
		embeddings:     newRuleSet(c.Embeddings),
		chatStructural: c.ChatStructural,
	}
}

func (c *identityCodec) EncodeRequest(endpoint typology.WireShape, canonicalBody []byte, target provcore.CallTarget) (provcore.EncodeResult, error) {
	switch endpoint {
	case typology.WireShapeOpenAIChat:
		// Two doors, one body: canonicalize(B) = B for OpenAI-family
		// input, so this door owes exactly what RewriteNative owes — the
		// resolved-model stamp (the canonical bridge does not stamp for
		// OpenAI-family ingress) and the contract's chat rules. Streaming
		// back-fills stay on the native door: this signature carries no
		// streaming intent, and the streaming legs re-enter dispatch
		// through RewriteNative.
		return c.chatDifferential(canonicalBody, target)
	case typology.WireShapeOpenAIResponses:
		// The native /v1/responses body owes the same two things the chat
		// wire gets — the resolved-model stamp and the per-model wire
		// rules (both wires serve the same reasoning models and reject
		// the same sampling parameters; a rule applied on only one of
		// them makes the same request 200 on chat and 400 here — §3a
		// Rule 3 keeps the rule with the adapter that talks to this
		// wire). encodeResponsesNative is the ONE body behind both entry
		// points, so this door and RewriteNative cannot diverge.
		return c.encodeResponsesNative(canonicalBody, target)
	case typology.WireShapeOpenAIEmbeddings:
		return c.encodeEmbeddings(canonicalBody, target)
	case typology.WireShapeNone, typology.WireShapeOpenAICompletionsLegacy:
		return provcore.EncodeResult{Body: canonicalBody, ContentType: "application/json"}, nil
	}
	return provcore.EncodeResult{}, fmt.Errorf("openai: unsupported endpoint %q", endpoint)
}

// DecodeResponse is identity for all endpoints. OpenAI response shape IS
// the canonical shape, so no structural translation is needed. For
// EndpointEmbeddings the response is forwarded verbatim; the canonical
// bridge layer handles any ingress-format projection upstream. Usage is
// extracted via the shared Tier-1 normalizer so the audit pipeline sees
// consistent token counts.
func (c *identityCodec) DecodeResponse(_ typology.WireShape, nativeBody []byte, _ string, _ provcore.DecodeContext) (provcore.DecodeResult, error) {
	return provcore.DecodeResult{
		CanonicalBody: nativeBody,
		Usage:         provcore.ExtractUsage(nativeBody, provcore.FormatOpenAI),
	}, nil
}
