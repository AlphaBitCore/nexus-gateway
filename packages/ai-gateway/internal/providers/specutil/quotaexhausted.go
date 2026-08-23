// quotaexhausted.go — recognising a spent provider ACCOUNT budget in an
// upstream error message.

package specutil

import "strings"

// quotaExhaustedMarkers are substrings that identify a spent account budget in
// an upstream error message, lowercased.
//
// Message matching is the wrong tool for most classification and the right one
// here: several providers file account exhaustion under an error TYPE that
// means something else — Anthropic returns it as `invalid_request_error` with
// HTTP 400, the same envelope a malformed body gets — so the type and the
// status carry no signal to key on. The same reasoning already justifies the
// context-overflow reclassification in the Anthropic normaliser.
//
// Each entry must be specific enough that a genuine caller error cannot trip
// it. "limit" alone would swallow every rate-limit and max_tokens message;
// these phrases name the account's budget, not a per-request bound.
var quotaExhaustedMarkers = []string{
	"usage limit", // Anthropic: "You have reached your specified API usage limits."
	"credit balance is too low",
	"exceeded your current quota", // OpenAI: "You exceeded your current quota, please check your plan and billing details."
	"insufficient_quota",
	"insufficient balance",
	"billing hard limit",
	"out of credits",
}

// Deliberately NOT a marker: a bare "quota exceeded". Google files a
// per-minute rate limit under exactly that prose —
// `Quota exceeded for quota metric '...requests per minute'` — which resets in
// sixty seconds and is the transient bucket, not a spent budget. Matching it
// would walk away from a healthy provider for the length of a request. The
// gateway's own virtual-key quota text uses the same words, so the phrase
// cannot distinguish whose budget is meant either. Every entry above names
// the ACCOUNT's money.

// IsQuotaExhaustedMessage reports whether an upstream error message describes
// the provider ACCOUNT being out of budget.
//
// This is not a rate limit: a rate limit clears in seconds and the same target
// is worth another turn, while a spent budget lasts until the window resets or
// the customer raises it, and it is account-scoped — every model behind that
// provider is equally unusable. The executor therefore eliminates the provider
// for the request and moves on rather than deprioritising it, and does not
// charge the credential, whose key is perfectly valid.
func IsQuotaExhaustedMessage(msg string) bool {
	if msg == "" {
		return false
	}
	lower := strings.ToLower(msg)
	for _, m := range quotaExhaustedMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}
