package specutil

import "testing"

// The matcher decides whether a request is abandoned to the caller as their
// own fault or handed to another provider, so both directions matter: a miss
// sinks a request three healthy providers could have served, and a false
// positive walks away from a provider over a genuine client error.
func TestIsQuotaExhaustedMessage(t *testing.T) {
	exhausted := []struct{ name, msg string }{
		{"anthropic", "You have reached your specified API usage limits. You will regain access on 2026-09-01 at 00:00 UTC."},
		{"anthropic credit", "Your credit balance is too low to access the Anthropic API."},
		{"openai", "You exceeded your current quota, please check your plan and billing details."},
		{"openai code", "insufficient_quota"},
		{"deepseek", "Insufficient Balance"},
		{"billing cap", "Billing hard limit has been reached"},
		{"credits", "You are out of credits"},
		{"case insensitive", "YOU HAVE REACHED YOUR SPECIFIED API USAGE LIMIT"},
	}
	for _, tc := range exhausted {
		t.Run(tc.name, func(t *testing.T) {
			if !IsQuotaExhaustedMessage(tc.msg) {
				t.Errorf("not recognised as a spent account budget: %q — the request is abandoned to the caller instead of moving to another provider", tc.msg)
			}
		})
	}

	// A per-request bound, a transient rate limit, and a malformed body are
	// each the opposite decision. "limit" appears in most of them, which is
	// why the markers name the ACCOUNT's budget rather than matching on it.
	notExhausted := []struct{ name, msg string }{
		{"empty", ""},
		{"rate limit", "Number of requests has exceeded your rate limit. Please try again later."},
		{"token limit", "max_tokens: 131072 > 128000, which is the maximum allowed"},
		{"context overflow", "prompt is too long: 300000 tokens > 200000 maximum"},
		{"missing field", "messages: at least one message is required"},
		{"model not found", "model gpt-9 does not exist"},
		{"concurrency", "You have reached your concurrent request limit"},
		// Google files a PER-MINUTE rate limit under this prose. Sixty seconds
		// is the transient bucket; treating it as a spent budget abandons a
		// healthy provider for the whole request.
		{"google per-minute quota", "Quota exceeded for quota metric 'Generate Content API requests per minute' of service 'generativelanguage.googleapis.com'"},
		// The gateway's own virtual-key spend cap uses the same words about a
		// different budget entirely.
		{"gateway's own quota text", "virtual key quota exceeded: monthly (12.50 / 10.00 USD)"},
	}
	for _, tc := range notExhausted {
		t.Run("not/"+tc.name, func(t *testing.T) {
			if IsQuotaExhaustedMessage(tc.msg) {
				t.Errorf("wrongly read as a spent account budget: %q — this walks away from a provider that is fine", tc.msg)
			}
		})
	}
}
