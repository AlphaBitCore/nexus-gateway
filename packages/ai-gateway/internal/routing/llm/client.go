// Package routerllm encapsulates the "ask an LLM to pick a model" half
// of smart routing. The Decider interface is a pure decision function:
// given a prepared system prompt, the recent conversation, and routing
// metadata, return the picked model. The smart strategy depends only on
// this interface — it does not import the provider adapter registry,
// the provtarget resolver, the canonical-OpenAI JSON wire format, or
// the HTTP status-code vocabulary.
//
// The production implementation AdapterDecider calls a provider adapter
// over HTTP. Future implementations (local classifier, rule engine, ML
// model) plug into the same interface; the strategy needs no change.
//
// Errors carry their trace text verbatim. The smart strategy writes
// the returned error's Error() string into the audit routing_trace
// without further inspection, so error messages here are operator-
// facing.
package llm

import (
	"context"
	"time"

	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Decider produces a Decision from canonical inputs.
type Decider interface {
	Decide(ctx context.Context, req Request) (Decision, error)
}

// Request is the canonical input to a router-LLM decision. The caller
// (smart strategy) prepares SystemPrompt with any operator-specified
// template and the model catalog already substituted; the Decider
// does not need access to either the catalog or the prompt template.
type Request struct {
	// SystemPrompt is the fully-prepared system message for the
	// router-LLM call (placeholder substitution + operator overrides
	// already applied). When empty the Decider uses
	// DefaultSystemPrompt with no catalog — this is rare; the smart
	// strategy normally always supplies a non-empty prompt because the
	// catalog is required for the router to pick anything.
	SystemPrompt string

	// Messages is the recent conversation from the original request —
	// user and assistant turns, already filtered by the smart strategy
	// (client system messages are excluded: they are large and
	// low-signal for routing; the metadata line in SystemPrompt carries
	// the request shape instead). Each Message can carry multimodal
	// content; the Decider's prompt builder projects ContentText blocks
	// only.
	Messages []normcore.Message

	// RouterContextLimit is the router model's declared context window
	// (maxContextTokens) in tokens. 0 means undeclared; the prompt
	// builder falls back to a conservative built-in window.
	RouterContextLimit int

	Temperature float64
	MaxTokens   int
	Timeout     time.Duration

	// RouterProviderID + RouterModelID identify which LLM is doing the
	// deciding. The AdapterDecider passes these to its provtarget
	// resolver; other Decider implementations may ignore them.
	RouterProviderID string
	RouterModelID    string
}

// Decision is the canonical output of a router-LLM call.
type Decision struct {
	// ModelID is the Model.code returned by the router. The smart
	// strategy resolves it against the catalog of enabled models.
	ModelID string

	// ProviderID, when non-empty, disambiguates Models that share a
	// code across providers (e.g. "gpt-4o" hosted on both OpenAI and
	// Azure). Optional.
	ProviderID string

	// Reason is the router-LLM's natural-language justification.
	// Surfaces in the audit routing_trace.
	Reason string
}
