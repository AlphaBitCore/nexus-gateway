package executor

import (
	"errors"
	"io"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	configtypes "github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configtypes/policy"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name      string
		resp      *provcore.Response
		err       error
		wantClass errClass
		wantErrCl configtypes.ErrorClass
	}{
		{"2xx success", &provcore.Response{StatusCode: 200}, nil, classSuccess, ""},
		{"rate limited", nil, &provcore.ProviderError{Code: provcore.CodeRateLimited, Status: 429}, classRate429, configtypes.ErrorClassRate429},
		{"timeout", nil, &provcore.ProviderError{Code: provcore.CodeTimeout, Status: 504}, classTimeout, configtypes.ErrorClassTimeout},
		{"upstream 5xx", nil, &provcore.ProviderError{Code: provcore.CodeUpstreamError, Status: 502}, class5xx, configtypes.ErrorClass5xx},
		// The request is at fault — no other target answers differently.
		{"invalid request 4xx", nil, &provcore.ProviderError{Code: provcore.CodeInvalidRequest, Status: 400}, classBadRequest, ""},
		{"no compatible provider", nil, &provcore.ProviderError{Code: provcore.CodeNoCompatibleProvider, Status: 400}, classNoCandidate, ""},

		// This provider or this target is at fault — the walk continues.
		{"auth failed", nil, &provcore.ProviderError{Code: provcore.CodeAuthFailed, Status: 401}, classAuthFailed, ""},
		{"endpoint unsupported", nil, &provcore.ProviderError{Code: provcore.CodeEndpointUnsupported, Status: 400}, classTargetUnsupported, ""},
		{"not implemented", nil, &provcore.ProviderError{Code: provcore.CodeNotImplemented, Status: 501}, classTargetUnsupported, ""},

		{"plain transport error EOF", nil, io.EOF, classNetwork, configtypes.ErrorClassNetwork},
		{"plain transport error generic", nil, errors.New("dial tcp: connection refused"), classNetwork, configtypes.ErrorClassNetwork},

		// Not network. An operator reading "network" on either of these goes
		// to look at the network and finds nothing; the honest label is that
		// we could not tell, which is itself a request to give it a name.
		{"unknown ProviderError code", nil, &provcore.ProviderError{Code: "totally_made_up", Status: 599}, classUnclassified, configtypes.ErrorClassNetwork},
		{"non-2xx with no error at all", &provcore.Response{StatusCode: 418}, nil, classUnclassified, configtypes.ErrorClassNetwork},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotClass, gotErrCl := classify(tc.resp, tc.err)
			if gotClass != tc.wantClass {
				t.Errorf("class: got %v want %v", gotClass, tc.wantClass)
			}
			if gotErrCl != tc.wantErrCl {
				t.Errorf("errClass: got %q want %q", gotErrCl, tc.wantErrCl)
			}
		})
	}
}

// TestErrClass_EveryClassSaysWhetherItChargesTheCredential.
//
// The credential breaker takes a working API key out of rotation. Which
// failures count toward that is a judgement only the walk's classifier can
// make — the breaker sees status codes, and two of the classes that must NOT
// charge arrive as ordinary 4xx and 5xx.
//
// This asserts the decision is made for every class, including ones added
// later. A new class that nobody classified would default silently, and the
// direction it defaults in is the difference between a key surviving a burst of
// cancelled streams and a key being disabled by it.
func TestErrClass_EveryClassSaysWhetherItChargesTheCredential(t *testing.T) {
	// Every class the walk can produce, named. Adding one to the const block
	// without adding it here fails on the count check below.
	decided := map[errClass]bool{
		classSuccess: true, classBadRequest: true, classNoCandidate: true,
		classClientGone: false, classAuthFailed: true, classPermissionDenied: false,
		classTargetUnsupported: true, classContextOverflow: true,
		classLocalProcessing: false, classNetwork: true, classTimeout: true,
		classRate429: true, class5xx: true, classUnclassified: true,
		// The key authenticated fine; the account's budget is what ran out.
		// Charging it opens the key's circuit over something a limit bump fixes.
		classProviderQuotaExhausted: false,
	}
	if got, want := len(decided), int(classCount); got != want {
		t.Fatalf("the table names %d classes but %d exist — a class was added without anyone "+
			"saying whether it is evidence about the API key, so it charges the credential by "+
			"default and can disable a working key for something it did not do", got, want)
	}
	for c, want := range decided {
		if got := c.chargesCredential(); got != want {
			t.Errorf("class %d chargesCredential() = %v, want %v", c, got, want)
		}
	}

	// The three that must not charge, each for its own reason, spelled out so a
	// future change has to argue with the reason rather than with a list.
	if classClientGone.chargesCredential() {
		t.Error("a caller hanging up is charged to the key that was never refused; a burst of " +
			"cancelled streams then opens the circuit on a key that is working")
	}
	if classLocalProcessing.chargesCredential() {
		t.Error("the upstream answered and billed, so the key worked — charging our own " +
			"decode failure to it disables a key for a bug on our side")
	}
	if classPermissionDenied.chargesCredential() {
		t.Error("a 403 is this target not being licensed to this key, which the provider " +
			"accepted the credential in order to say")
	}
}

// TestErrClass_EveryClassIsNamedAndDistinct.
//
// The row is where an operator finds out what happened. A class with no name
// reports as nothing at all; two classes sharing one name report as each
// other, and the operator diagnoses the wrong system. Both were live: the five
// eliminating and aborting classes had no reported name, and `unclassified`
// reported as `network` because the row carried the retry bucket rather than
// the class.
//
// Counted against `classCount` so a class added later cannot skip this.
func TestErrClass_EveryClassIsNamedAndDistinct(t *testing.T) {
	seen := map[string]errClass{}
	for c := classSuccess; c < classCount; c++ {
		n := c.name()
		if c == classSuccess {
			if n != "" {
				t.Errorf("classSuccess names itself %q; a successful attempt has no failure to report", n)
			}
			continue
		}
		if n == "" {
			t.Errorf("class %d has no name, so every failure of that kind reaches the traffic row "+
				"as a blank and the operator has nothing to read", c)
			continue
		}
		if prev, dup := seen[n]; dup {
			t.Errorf("classes %d and %d both report as %q — two failures needing different "+
				"diagnoses are indistinguishable on the row", prev, c, n)
		}
		seen[n] = c
	}
}

// TestClassify_AnUnrecognisedProviderErrorIsNotReportedAsANetworkFault is the
// specific confusion the naming exists to end.
//
// A provider error whose code the classifier does not know is RETRIED like a
// network fault — that part is right, and it is why both carry
// ErrorClassNetwork for the RetryOn check. Reporting it as one is not: the
// network was never involved, and an operator handed "network" goes and looks
// at one.
func TestClassify_AnUnrecognisedProviderErrorIsNotReportedAsANetworkFault(t *testing.T) {
	unknown, unknownRetry := classify(nil, &provcore.ProviderError{Status: 418, Code: "teapot"})
	transport, transportRetry := classify(nil, errors.New("dial tcp: connection refused"))

	// The premise: both are retried through the same bucket. If this ever
	// stops being true the test below proves nothing.
	if unknownRetry != configtypes.ErrorClassNetwork || transportRetry != configtypes.ErrorClassNetwork {
		t.Fatalf("premise gone: retry buckets are %q and %q, so reporting the bucket would "+
			"already tell them apart and this test cannot see its defect", unknownRetry, transportRetry)
	}
	if unknown.name() == transport.name() {
		t.Fatalf("both report as %q; the row cannot tell a provider error we failed to "+
			"recognise from the network being down", unknown.name())
	}
	// And neither may collide with the metrics sentinel of the same idea,
	// which means the OPPOSITE: proxy_upstream_errors.go labels an attempt
	// carrying NO code "unclassified". A dial refusal is that; an unrecognised
	// code is not.
	if unknown.name() == "unclassified" {
		t.Error("the class for an unrecognised provider CODE is spelled like the metrics label " +
			"for an attempt carrying NO code — an operator cross-reading the two surfaces gets " +
			"opposite answers")
	}
	if transport.name() != string(configtypes.ErrorClassNetwork) {
		t.Errorf("a real transport failure reports as %q, not the word the retry policy uses "+
			"for it", transport.name())
	}
}

// TestErrClass_NamesAgreeWithTheCanonicalCodeTheyDescribe.
//
// The attempt publishes `code` and `errorClass` on the same JSON object. When
// both describe one failure they must be one string, not two spellings of it —
// otherwise a reader has to work out whether `upstream_error` and `5xx` are the
// same fact, and every class added later picks a spelling at random.
//
// Driven through `classify` from a real ProviderError, so it asserts the pair a
// production attempt actually carries rather than a table someone wrote twice.
// The classes with no canonical code are named and excluded on purpose: they
// exist precisely because `code` cannot express them.
func TestErrClass_NamesAgreeWithTheCanonicalCodeTheyDescribe(t *testing.T) {
	for _, code := range []string{
		provcore.CodeInvalidRequest, provcore.CodeAuthFailed, provcore.CodeRateLimited,
		provcore.CodeTimeout, provcore.CodeUpstreamError, provcore.CodeEndpointUnsupported,
		provcore.CodeContextOverflow, provcore.CodeNotImplemented,
		provcore.CodeNoCompatibleProvider, provcore.CodeClientGone, provcore.CodeLocalProcessing,
	} {
		t.Run(code, func(t *testing.T) {
			cls, _ := classify(nil, &provcore.ProviderError{Code: code, Status: 500})
			got := cls.name()
			switch {
			case got == code:
				return // the pair agrees, which is the whole point
			case code == provcore.CodeNotImplemented && got == provcore.CodeEndpointUnsupported:
				// Both mean "this target cannot serve this endpoint"; the
				// classifier folds them, and folding is not a second spelling.
				return
			default:
				t.Errorf("code %q classifies to %q — the attempt publishes both on one object, "+
					"so this is two vocabularies for one failure", code, got)
			}
		})
	}

	// The classes that exist BECAUSE `code` cannot express them. Listed so a
	// future reader sees the exception is deliberate and bounded.
	for cls, why := range map[errClass]string{
		classPermissionDenied: "401 and 403 both normalise to auth_failed; which one decides " +
			"whether the provider or only this model is out",
		classNetwork:         "a transport error produces no ProviderError, so code is empty",
		classUnclassified:    "the code exists but the classifier does not know it",
		classNoCandidate:     "",
		classBadRequest:      "",
		classClientGone:      "",
		classLocalProcessing: "",
	} {
		if why == "" {
			continue
		}
		if cls.name() == "" {
			t.Errorf("class %d has no name, and %s", cls, why)
		}
	}
}

// TestErrClass_EveryClassSaysWhetherItIsEvidenceAboutTheProvider.
//
// The health tracker marks a provider unavailable at an error rate, and the
// requests then routed away belong to everyone using it. So which failures
// count has to be a decision per class, not a consequence of which branch a
// class happens to fall into.
//
// It is a DIFFERENT question from chargesCredential and the two disagree: a 403
// says the key is fine and this model is not licensed to it, so the provider
// hears about the refusal and the credential does not. Asserting them together
// is what keeps a future edit from collapsing them into one flag.
func TestErrClass_EveryClassSaysWhetherItIsEvidenceAboutTheProvider(t *testing.T) {
	decided := map[errClass]bool{
		classSuccess: true, classBadRequest: false, classNoCandidate: false,
		classClientGone: false, classAuthFailed: true, classPermissionDenied: true,
		classTargetUnsupported: true, classContextOverflow: true,
		classLocalProcessing: false, classNetwork: true, classTimeout: true,
		classRate429: true, class5xx: true, classUnclassified: true,
		// The provider genuinely cannot serve anything right now. The health
		// window decays, so a raised limit is picked up on the next turn.
		classProviderQuotaExhausted: true,
	}
	if got, want := len(decided), int(classCount); got != want {
		t.Fatalf("the table names %d classes but %d exist — a class was added without anyone "+
			"saying whether it is evidence about the PROVIDER, so it counts by default and can "+
			"mark a working provider unavailable", got, want)
	}
	for c, want := range decided {
		if got := c.recordsProviderHealth(); got != want {
			t.Errorf("class %d recordsProviderHealth() = %v, want %v", c, got, want)
		}
	}

	// The four that must not count, each for its own reason.
	if classLocalProcessing.recordsProviderHealth() {
		t.Error("the upstream answered and billed, so it worked — counting our own decode " +
			"failure against it pushes a healthy provider toward unavailable for a bug on our side")
	}
	if classBadRequest.recordsProviderHealth() || classNoCandidate.recordsProviderHealth() {
		t.Error("a malformed body, or our own judgement that nothing can serve it, is not the " +
			"provider failing; counting it lets one bad client take a provider out for everyone")
	}
	if classClientGone.recordsProviderHealth() {
		t.Error("a caller hanging up says nothing about the provider, which may have been about " +
			"to answer perfectly")
	}

	// And the pair that proves this is not chargesCredential under another
	// name: 403 is the provider refusing, with a key that is fine.
	if !classPermissionDenied.recordsProviderHealth() || classPermissionDenied.chargesCredential() {
		t.Error("permission_denied must count against the PROVIDER and not the credential; " +
			"collapsing the two questions loses the case that distinguishes them")
	}
}

// TestErrClass_EveryClassSaysWhetherItWantsElapsedTime.
//
// The answer routes the failure to one of two retry mechanisms: false means the
// per-target loop retries in place, true means the target is handed back to the
// walk and comes round again after other providers have been tried.
//
// Defaulting silently is the hazard. A class that says false when it wanted
// time gets a retry milliseconds later against the same exhausted quota; a
// class that says true when it wanted an immediate retry gives up its turn and
// lets a different model answer a request that would have succeeded on the very
// next attempt.
func TestErrClass_EveryClassSaysWhetherItWantsElapsedTime(t *testing.T) {
	decided := map[errClass]bool{
		// A quota is the one thing another dispatch cannot fix and time can.
		classRate429: true,
		// Everything else either has nothing to retry or is the kind of fault an
		// immediate second attempt is exactly right for.
		classSuccess: false, classBadRequest: false, classNoCandidate: false,
		classClientGone: false, classAuthFailed: false, classPermissionDenied: false,
		classTargetUnsupported: false, classContextOverflow: false,
		classLocalProcessing: false, classNetwork: false, classTimeout: false,
		class5xx: false, classUnclassified: false,
		// An account budget clears in days, not seconds: waiting is not the
		// fix, another provider is.
		classProviderQuotaExhausted: false,
	}
	if got, want := len(decided), int(classCount); got != want {
		t.Fatalf("the table names %d classes but %d exist — a class was added without anyone "+
			"saying which retry mechanism owns it, so it inherits the in-place loop and the "+
			"walk never gets a turn at it", got, want)
	}
	for c := classSuccess; c < classCount; c++ {
		want, named := decided[c]
		if !named {
			t.Errorf("class %q is not in the table", c.name())
			continue
		}
		if got := c.wantsElapsedTime(); got != want {
			t.Errorf("%s.wantsElapsedTime() = %v, want %v", c.name(), got, want)
		}
	}
}
