// requested_model.go — what the caller asked for, as the routing pipeline sees
// it: the raw model string, what the catalogue resolves it to, and the one
// answer to "which providers could serve this".
package core

// RequestedModel identifies the model requested by the client.
//
// ID holds the raw `model` string sent in the request body (a Model.code like
// "gpt-4o" or a request-side sentinel like "auto"); it is not a UUID despite
// the field name. CandidateIDs lists every Model.id that ID resolves to via
// Model.code or Model.aliases; matchConditions.models intersects it. Both are
// populated by Resolver.hydrateRequestedModel.
type RequestedModel struct {
	ID   string
	Type string // chat | embedding | image | audio
	// Per-provider facts, populated only when ID resolved unambiguously — a
	// code several providers serve has no single answer for any of them.
	ProviderID      string
	ProviderName    string
	ProviderModelID string
	CandidateIDs    []string
	// CandidateProviderIDs lists the distinct providers serving those
	// candidates. matchConditions.providers intersects it.
	CandidateProviderIDs []string
	// HydrationFailed: the caller named a model and the catalogue lookup ERRORED,
	// so the candidate fields are empty for a reason that is not "nothing was
	// named". Conditions that treat emptiness as inapplicable must not: during a
	// catalogue blip a provider-scoped rule would match models on OTHER
	// providers and redirect them there.
	HydrationFailed bool
}

// ProviderIDs is every provider that could serve what the caller named. One
// place answers this, because two would disagree exactly when it matters — a
// caller reading only ProviderID sees a two-provider code as belonging to
// whichever row came back first. Empty when nothing was named, which conditions
// treat as inapplicable rather than false.
func (rm RequestedModel) ProviderIDs() []string {
	if len(rm.CandidateProviderIDs) > 0 {
		return rm.CandidateProviderIDs
	}
	if rm.ProviderID != "" {
		return []string{rm.ProviderID}
	}
	return nil
}

// ProviderIsUnknowable reports that the caller named a model and we could NOT
// find out which providers serve it.
//
// Distinct from "named nothing", which is the inapplicable case: a condition
// that cannot be evaluated must not be read as satisfied. Collapsing the two
// into an empty slice turned a catalogue read failure into a provider-scoped
// rule matching every provider — fail-open, in the one direction that
// redirects traffic somewhere the admin excluded.
func (rm RequestedModel) ProviderIsUnknowable() bool {
	return rm.HydrationFailed && len(rm.CandidateProviderIDs) == 0
}
