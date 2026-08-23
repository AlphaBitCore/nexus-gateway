package executor

import (
	"errors"
	"net/http"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	cfgpolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

type errClass int

// The classes fall into three treatments, and which one a class gets follows
// from WHOSE fault the failure is:
//
//	the REQUEST's           → abort; no other target would answer differently
//	this TARGET or PROVIDER → eliminate it and keep walking
//	the upstream's state    → deprioritise it; another turn may succeed
//
// The distinction is not academic. Authentication failure and invalid-request
// shared one class, so a single provider's expired key sank requests that
// healthy targets behind it were waiting to serve.
//
// A class also earns its name a second way: it is what an operator reads on a
// traffic row. Two failures that need different diagnoses may not share a name
// even when the executor treats them alike — which is why unclassified is its
// own class rather than more network.
const (
	classSuccess errClass = iota

	// Abort — nothing another target could do would help.
	classBadRequest  // the caller's body; every model rejects it identically
	classNoCandidate // our own judgement that nothing can serve it
	classClientGone  // the caller stopped listening; there is no one to serve

	// Eliminate — this target or provider is at fault, permanently for this
	// request. Retrying it in place spends a call to learn what we know.
	classAuthFailed        // this PROVIDER's credentials
	classPermissionDenied  // this TARGET is not licensed to this key
	classTargetUnsupported // this TARGET cannot serve this endpoint at all
	classContextOverflow   // this MODEL's window; the same prompt overflows every time
	classLocalProcessing   // the upstream answered and billed; WE could not read it
	// classProviderQuotaExhausted — this PROVIDER's account budget is spent.
	// Account-scoped like classAuthFailed, so every model behind it is equally
	// unusable and the provider goes; unlike classRate429 it does not clear in
	// seconds, so deprioritising and coming back would spend the request's
	// remaining turns on a target that cannot answer any of them.
	classProviderQuotaExhausted

	// Deprioritise — the upstream's current state is at fault.
	classNetwork
	classTimeout
	classRate429
	class5xx
	classUnclassified // we do not know; treated as network, reported as itself

	// classCount is the sentinel that makes the set countable. A class added
	// anywhere above it changes this number, which is what lets a test assert
	// that every class has been considered rather than that the classes someone
	// remembered have been.
	classCount
)

// abortsRequest reports whether the class means no other target can help.
// chargesCredential reports whether this failure is evidence about the API KEY
// that made the call, and therefore whether the credential breaker should count
// it toward opening a circuit.
//
// The breaker reads raw status codes and derives its own labels from them, so
// it cannot make this judgement: a client-gone 499 reads as an ordinary 4xx and
// a local-processing 502 as an upstream fault. Charging either one takes a
// working key out of rotation for something it did not do — measured once as a
// key disabled by a burst of cancelled streams.
//
// Naming the classes that DO charge, rather than the ones that do not, is the
// point. A class added later is not charged until someone says it should be,
// and `TestErrClass_EveryClassSaysWhetherItChargesTheCredential` fails until
// they do — the failure mode of the opposite spelling is silence.
func (c errClass) chargesCredential() bool {
	switch c {
	case classSuccess, classAuthFailed, classRate429, class5xx,
		classNetwork, classTimeout, classUnclassified, classBadRequest,
		classNoCandidate, classTargetUnsupported, classContextOverflow:
		return true
	case classClientGone, classLocalProcessing, classPermissionDenied,
		classProviderQuotaExhausted:
		// classClientGone: the caller hung up; the key was never refused.
		// classLocalProcessing: the upstream answered and billed, so the key
		// worked — the failure is ours, downstream of it.
		// classPermissionDenied: 403 is this TARGET not being licensed to this
		// key, which the provider had already accepted to tell us so.
		// classProviderQuotaExhausted: the key authenticated fine and the
		// account's budget is what ran out. Charging it opens the key's
		// circuit and takes a working key out of rotation for something the
		// customer fixes by raising a limit.
		return false
	}
	return true
}

// name is what an operator reads on the traffic row, and it is deliberately
// the SAME string `code` carries whenever the two describe one failure.
//
// The attempt publishes both. Spelling them differently — `upstream_error`
// beside `5xx`, `invalid_request` beside `bad-request` — puts two vocabularies
// on one JSON object and makes the reader work out whether they are one fact or
// two. The canonical `ProviderError.Code` set is the repo's established
// spelling for "what the failure was", so the class reuses it and adds a name
// only where the class draws a distinction the code cannot:
//
//   - permission_denied — 401 and 403 both normalise to `auth_failed`, and the
//     difference is expensive: one is the provider's credential, the other is
//     this model not being licensed to it.
//   - network — a transport error produces no ProviderError at all, so `code`
//     is empty and this is the only thing on the row.
//   - unknown_provider_code — a ProviderError whose code the classifier does
//     not know. NOT the metrics `unclassified`, which means the opposite (an
//     attempt carrying no code); naming them alike had an operator reading a
//     dial refusal as one and an unknown code as the other.
//
// The `retryOn` spelling ("429", "5xx") stays where it is used, on classify's
// second return value, and is never published — the retry policy is admin
// config, not a description of what happened.
//
// Total over the class set, with no default arm: `classCount` and
// `TestErrClass_EveryClassIsNamedAndDistinct` mean a class added later has to
// be given a name here rather than silently reporting as nothing.
func (c errClass) name() string {
	switch c {
	case classSuccess:
		return ""
	case classBadRequest:
		return provcore.CodeInvalidRequest
	case classNoCandidate:
		return provcore.CodeNoCompatibleProvider
	case classClientGone:
		return provcore.CodeClientGone
	case classAuthFailed:
		return provcore.CodeAuthFailed
	case classPermissionDenied:
		return "permission_denied"
	case classTargetUnsupported:
		return provcore.CodeEndpointUnsupported
	case classContextOverflow:
		return provcore.CodeContextOverflow
	case classLocalProcessing:
		return provcore.CodeLocalProcessing
	case classNetwork:
		return "network"
	case classTimeout:
		return provcore.CodeTimeout
	case classRate429:
		return provcore.CodeRateLimited
	case classProviderQuotaExhausted:
		return provcore.CodeProviderQuotaExhausted
	case class5xx:
		return provcore.CodeUpstreamError
	case classUnclassified:
		return "unknown_provider_code"
	}
	return ""
}

// recordsProviderHealth reports whether this failure is evidence about the
// PROVIDER, and therefore whether the health tracker should count it toward
// marking that provider unavailable.
//
// It is a different question from chargesCredential, and the two disagree. A
// 403 says the key is fine and this model is not licensed to it: the key is not
// at fault, but the provider did refuse the call, so the provider hears about
// it and the credential does not.
//
// The classes that say nothing about the provider are the ones whose failure
// happened on OUR side of the call or on the caller's. A decode fault after the
// upstream answered and billed is ours; a malformed body and a request no
// candidate can serve are the caller's. Counting any of them pushes a provider
// that did its job toward unavailable, and the requests then routed away are
// other people's.
//
// Named rather than inferred from which arm a class lands in, and total over
// the class set, so a class added later has to be decided rather than
// inheriting whatever the surrounding branch happened to do.
func (c errClass) recordsProviderHealth() bool {
	switch c {
	case classSuccess, classAuthFailed, classPermissionDenied, classTargetUnsupported,
		classContextOverflow, classNetwork, classTimeout, classRate429, class5xx,
		classUnclassified,
		// classProviderQuotaExhausted: the provider genuinely cannot serve
		// anything right now, which is what the health signal is for. The
		// window decays, so a customer who raises their limit is picked up on
		// the next turn rather than shunned indefinitely.
		classProviderQuotaExhausted:
		return true
	case classClientGone, classLocalProcessing, classBadRequest, classNoCandidate:
		// classClientGone: the caller hung up; the provider may have been about
		// to answer perfectly.
		// classLocalProcessing: the upstream answered and billed, so it worked
		// — the failure is ours, downstream of it.
		// classBadRequest / classNoCandidate: the caller's own body, or our own
		// judgement that nothing can serve it. Neither is the provider failing.
		return false
	}
	return true
}

func (c errClass) abortsRequest() bool {
	return c == classBadRequest || c == classNoCandidate || c == classClientGone
}

// eliminatesTarget reports whether the failed target is out for this request
// rather than merely pushed back.
func (c errClass) eliminatesTarget() bool {
	return c == classAuthFailed || c == classPermissionDenied ||
		c == classTargetUnsupported || c == classContextOverflow ||
		c == classLocalProcessing || c == classProviderQuotaExhausted
}

// surfacesUpstreamEnvelope reports whether the upstream's own status, headers
// and body are the right thing to return when this class ends the walk.
func (c errClass) surfacesUpstreamEnvelope() bool {
	return c.abortsRequest() || c.eliminatesTarget()
}

// classify maps an adapter.Execute outcome to (errClass, cfgpolicy.ErrorClass).
// The first return is the executor's branching key; the second is the
// matching cfgpolicy.ErrorClass for RetryOn membership checks (empty
// when not retryable).
func classify(resp *provcore.Response, err error) (errClass, cfgpolicy.ErrorClass) {
	if err == nil && resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return classSuccess, ""
	}

	var pe *provcore.ProviderError
	if errors.As(err, &pe) {
		switch pe.Code {
		case provcore.CodeRateLimited:
			return classRate429, cfgpolicy.ErrorClassRate429
		case provcore.CodeTimeout:
			return classTimeout, cfgpolicy.ErrorClassTimeout
		case provcore.CodeUpstreamError:
			return class5xx, cfgpolicy.ErrorClass5xx
		case provcore.CodeContextOverflow:
			// Target-permanent: the same model always overflows, so a
			// same-target retry is pointless — but a larger-context
			// sibling target can serve the request unchanged.
			return classContextOverflow, ""
		case provcore.CodeInvalidRequest:
			return classBadRequest, ""
		case provcore.CodeNoCompatibleProvider:
			return classNoCandidate, ""
		case provcore.CodeLocalProcessing:
			// A 2xx that we then failed to read or decode. The upstream has
			// already produced — and charged for — the answer, so asking it
			// again buys a second one. Never retried in place; for endpoints
			// whose output is generated fresh each time, not retried at all.
			return classLocalProcessing, ""
		case provcore.CodeClientGone:
			// The caller's context was cancelled. The transport error that
			// followed is evidence about the client, not the provider — so no
			// health failure is recorded against it, and no further target is
			// dispatched for a request nobody is waiting on.
			return classClientGone, ""
		case provcore.CodeAuthFailed:
			// Every normalizer folds 401 and 403 into this one code, but they
			// scope differently and the difference is expensive. A 401 says the
			// key is not accepted: every target on that provider shares it, so
			// the provider goes. A 403 says the key is fine and THIS MODEL is
			// not licensed to it — a tier the org has not bought, a preview
			// nobody enabled. Treating that as a credential failure eliminates
			// the provider's working models for the request, and feeds a
			// counter that opens the key's circuit; enough of them and every
			// model on that key stops resolving for everyone.
			//
			// The status separates them without touching a single normalizer.
			if pe.Status == http.StatusForbidden {
				return classPermissionDenied, ""
			}
			return classAuthFailed, ""
		case provcore.CodeProviderQuotaExhausted:
			// The account, not the key and not the request. Walk to another
			// provider; this one answers nothing until its window resets or
			// the customer raises the limit.
			return classProviderQuotaExhausted, ""
		case provcore.CodeEndpointUnsupported, provcore.CodeNotImplemented:
			// Scoped to this target: another one may serve the endpoint.
			return classTargetUnsupported, ""
		}
		// A ProviderError carrying a code this switch does not know. Calling
		// it a network fault would send an operator to look at the network.
		return classUnclassified, cfgpolicy.ErrorClassNetwork
	}

	if err != nil {
		// A transport error with no provider envelope: the request did not
		// complete, which is what network means.
		return classNetwork, cfgpolicy.ErrorClassNetwork
	}

	// No error, and a status the success branch rejected — an upstream HTTP
	// failure the adapter did not classify. This is the case that most
	// deserves its own name: reporting it as a network fault is a lie the
	// operator will act on.
	return classUnclassified, cfgpolicy.ErrorClassNetwork
}

// generatesFreshOutput reports whether an endpoint produces new content on
// every call, so a repeat call is a repeat charge for something the caller may
// never receive.
//
// Chat and embeddings are billed too, but their output is a function of their
// input: re-asking after a local failure costs one ordinary failover. An image,
// a video, or a spoken track is generated anew each time, and a decode fault
// that repeats across a plan would buy one from every target in it.
func generatesFreshOutput(kind typology.EndpointKind) bool {
	switch kind {
	case typology.EndpointKindImageGeneration,
		typology.EndpointKindVideoGeneration,
		typology.EndpointKindTTS:
		return true
	}
	return false
}

// wantsElapsedTime reports whether this failure is one that another dispatch
// milliseconds later cannot help, but a dispatch after several upstream
// round-trips can.
//
// It is what keeps two retry mechanisms from answering one question. The
// per-target loop retries in place, which is right for a blip — a reset
// connection, a single 5xx, a request that timed out on a machine that is
// otherwise fine. The walk coming back to a rested target is right for a quota,
// because what a quota needs is time, and an in-place retry cannot supply it.
//
// Without the split the in-place loop spends the target's whole request
// allowance on the first turn, and the walk can never come back — which makes
// the resting pass unreachable at every setting rather than reserved for the
// case it exists for.
//
// Total over the class set for the same reason the other predicates are: a
// class added later has to be decided, not left to inherit a branch.
func (c errClass) wantsElapsedTime() bool {
	switch c {
	case classRate429:
		return true
	case classSuccess, classAuthFailed, classPermissionDenied, classTargetUnsupported,
		classContextOverflow, classNetwork, classTimeout, class5xx, classUnclassified,
		classClientGone, classLocalProcessing, classBadRequest, classNoCandidate,
		// classProviderQuotaExhausted: the opposite of classRate429 above. A
		// rate limit clears in seconds, so time is the fix; an account budget
		// clears when its window resets or the customer raises it — days, not
		// seconds — so waiting spends the request's budget on nothing and
		// another provider is the only thing that answers.
		classProviderQuotaExhausted:
		return false
	}
	return false
}
