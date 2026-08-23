package core

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

// ModelMatchesAllowedRefs reports whether a model is permitted by the virtual
// key's allowed-models list. Empty refs = unrestricted.
//
// Comparison is EXACT, deliberately. It used to glob ref.ModelID, and a glob
// here is not merely unused — it makes the admin UI misreport the key.
//
// The model-access picker writes concrete model UUIDs on tick and decides a
// checkbox by exact equality (VirtualKeyCreate.tsx isRefSelected). A ref like
// {providerId: "openai", modelId: "gpt-*"} — reachable only through the admin
// API — therefore matches NO checkbox: every OpenAI model renders unticked and
// the footer reads "0 model(s) selected", while the gateway permits all of
// them. The UI shows LESS access than the key has, an admin "fixing" it by
// ticking boxes only APPENDS refs, and the glob survives. For an authorisation
// boundary, invisible-but-active is worse than anything it buys.
//
// The genuine need behind a pattern — "authorise every model from this
// provider" — is answered by a per-provider select-all in the picker, which an
// admin can see and audit, not by a pattern language the picker renders as
// nothing. MatchGlob itself stays: routing-rule match conditions glob virtual
// key NAMES, which is a documented, UI-exposed feature.
//
// The two-identifier comparison is NOT globbing and remains: a ref may name
// either the catalog model id or the provider's own model id.
func ModelMatchesAllowedRefs(modelID, providerModelID, providerID string, refs []store.AllowedModelRef) bool {
	if len(refs) == 0 {
		return true
	}
	for _, ref := range refs {
		if ref.ProviderID != providerID {
			continue
		}
		if ref.ModelID == modelID || ref.ModelID == providerModelID {
			return true
		}
	}
	return false
}
