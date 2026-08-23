package proxy

import (
	"net/http"
	"sort"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/capability"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// floorGuard rejects a request that a model REQUIRES something from and does
// not carry.
//
// The distinction from the modality guard beside it is the direction of the
// question. That one asks "can this model serve this endpoint kind" and passes
// gpt-audio-mini for a plain text chat, correctly — it IS type=chat. The floor
// asks the opposite: the model needs audio, and this request has none, so it
// will 400 upstream no matter how well-formed everything else is.
//
// The resolver has applied this since capability.RequiredModalities existed
// (strategies/strategy_smart_sizing.go, firstFloorMiss). The explicit-model
// passthrough never did, so addressing such a model by name skipped the guard
// entirely — the same shape as every other defect this class produces: a rule
// that holds on one selection path and not the other.
//
// Cost. The floor needs to know which modalities the request carries, and the
// only honest source is the canonical. Materialising it here would undo the
// lazy-canonical work — except that the comment on firstFloorMiss records the
// fact that settles it: 198 of 200 catalog models declare no floor at all. So
// the snapshot is consulted first, and the canonical is materialised only for
// the models that actually have one. Roughly one request in a hundred pays,
// and no second content scanner has to be written and then kept in sync with
// the codecs — which is precisely how the finish_reason mappings drifted into
// five disagreeing copies.
func (h *Handler) floorGuard(modelID, requestedModel string, canonical canonicalFn) error {
	if h.deps == nil || h.deps.CapCache == nil || canonical == nil {
		// Matches the resolver's own nil-tolerance: a deployment without a
		// capability snapshot skips the check rather than refusing traffic.
		return nil
	}
	cap := h.deps.CapCache.Load().Get(modelID)
	if cap == nil || len(cap.RequiredModalities) == 0 {
		return nil // the 198-of-200 case: nothing required, nothing to check
	}

	missing := capability.MissingFloor(cap.RequiredModalities, capability.CarriedModalities(canonical()))
	if len(missing) == 0 {
		return nil
	}
	return &routingFallbackError{
		status: http.StatusBadRequest,
		code:   "MODEL_REQUIRED_MODALITY_MISSING",
		message: "model " + requestedModel + " requires " + strings.Join(missing, ", ") +
			" in the request, which carries none",
		hint: "Send the required content, or address a model that does not require it",
	}
}

// namedModelModalityGuard applies the modality FLOOR + input-modality CEILING
// to a model the caller NAMED — but only when EnforceNamedModelModality is set.
//
// This is the #297 policy at the explicit-model passthrough. Both guards read
// the gateway's OWN capability catalogue, which has been wrong (mislabelled
// video, reasoning). When the caller names the model, they own its limits, so
// by default the gateway defers the modality verdict to the upstream and only
// enforces it locally when an operator opts in with the flag. The `auto`/smart
// path — where the GATEWAY chose the model — enforces unconditionally in the
// resolver; this method is the named-path counterpart of that same flag.
//
// The embeddings capability guard is deliberately NOT part of this method: it
// checks parameter compatibility (dimensions, encoding format, input_type), not
// modality, so the "our catalogue may be wrong about modality" rationale does
// not apply, and it stays unconditional at the call site.
func (h *Handler) namedModelModalityGuard(modelID, requestedModel string, canonical canonicalFn) error {
	if h.deps == nil || !h.deps.EnforceNamedModelModality {
		return nil
	}
	if err := h.floorGuard(modelID, requestedModel, canonical); err != nil {
		return err
	}
	return h.inputModalityGuard(modelID, requestedModel, canonical)
}

// inputModalityGuard is the floor's mirror image: the floor asks whether the
// model needs something the request lacks, this asks whether the request
// carries something the model cannot take. An image sent to a text-only model
// 400s upstream with the provider's wording; ours names the modality and says
// what to do about it.
//
// The subtlety is what an ABSENT modality means, and the answer depends on
// whether the catalog describes that modality anywhere at all. A modality no row
// declares tells us nothing about the row in front of us: refusing on it would
// reject every request carrying it, including lanes that serve such requests
// correctly today. So the guard refuses only on modalities some row describes,
// and starts enforcing a new one the moment the catalog learns it — no code
// change, and no guessing in the meantime.
//
// That threshold has already been crossed once. This comment used to record
// that no row declared "file"; the catalog now declares it on 36 of 203 models,
// so document requests are enforced against the models that do not. The rule
// held across the change, which is the point of writing it as a rule rather than
// as a list of modalities.
//
// Text is never refused here. The ceiling predicate admits it unconditionally,
// so the four transcription models that omit "text" from their declaration —
// whisper-1 and the gpt-4o-transcribe family — still accept the prompt text an
// audio request carries alongside its audio.
func (h *Handler) inputModalityGuard(modelID, requestedModel string, canonical canonicalFn) error {
	if h.deps == nil || h.deps.CapCache == nil || canonical == nil {
		return nil // same nil-tolerance as the floor beside it
	}
	snap := h.deps.CapCache.Load()
	cap := snap.Get(modelID)
	if cap == nil || len(cap.InputModalities) == 0 {
		return nil // no declared data is not a constraint
	}

	carried := capability.CarriedModalities(canonical())
	if len(carried) == 0 {
		return nil
	}
	described := snap.DescribedInputModalities()

	var rejected []string
	for _, m := range carried {
		if described[m] && !capability.DeclarationAcceptsModality(cap.InputModalities, m) {
			rejected = append(rejected, m)
		}
	}
	if len(rejected) == 0 {
		return nil
	}
	sort.Strings(rejected) // stable message for a multi-modality request
	return &routingFallbackError{
		status: http.StatusBadRequest,
		code:   "MODEL_INPUT_MODALITY_UNSUPPORTED",
		message: "model " + requestedModel + " does not accept " + strings.Join(rejected, ", ") +
			" input; it accepts " + strings.Join(cap.InputModalities, ", "),
		hint: "Send this content to a model that accepts it, or remove it from the request",
	}
}

// embeddingCapabilityGuard refuses an embeddings request the addressed model
// cannot serve — a dimensions value it does not support, an encoding format it
// does not emit, an input_type it does not know.
//
// The resolver has applied this since the capability pre-filter was added
// (routing/resolver.go applyCapabilityFilter). The explicit-model passthrough
// never did, so naming such a model directly walked past the check and the
// caller got the provider's own rejection instead of ours — or, worse, a 200
// computed under parameters they did not ask for.
//
// Same cost discipline as the floor beside it: the snapshot is consulted
// first, and the request body is parsed only when there is a declared
// embeddings capability to compare against.
func (h *Handler) embeddingCapabilityGuard(modelID, requestedModel string, endpointKind typology.EndpointKind, rawBody func() []byte) error {
	if endpointKind != typology.EndpointKindEmbeddings {
		return nil
	}
	if h.deps == nil || h.deps.CapCache == nil || rawBody == nil {
		return nil // same nil-tolerance as the resolver's own pre-filter
	}
	cap := h.deps.CapCache.Load().Get(modelID)
	if cap == nil || cap.Embeddings == nil {
		// Nothing declared, nothing to compare. Matches the resolver, which
		// treats an absent Embeddings block as "no capability data" rather
		// than as a constraint.
		return nil
	}

	params := parseEmbeddingRequest(rawBody())
	if params == nil {
		return nil
	}
	ok, reason, _ := capability.Compatible(&capability.EmbeddingRequest{
		Dimensions:     params.Dimensions,
		BatchSize:      params.BatchSize,
		EncodingFormat: params.EncodingFormat,
		InputType:      params.InputType,
		TaskType:       params.TaskType,
	}, cap)
	if ok {
		return nil
	}
	return &routingFallbackError{
		status:  http.StatusBadRequest,
		code:    "MODEL_CAPABILITY_MISMATCH",
		message: "model " + requestedModel + " cannot serve this embeddings request: " + reason,
		hint:    "Adjust the parameter named above, or address a model that supports it",
	}
}
