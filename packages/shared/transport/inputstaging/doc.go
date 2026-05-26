// Package inputstaging provides a shared primitive for fitting multi-turn
// conversations into a model's context window before embedding or inference.
//
// # Design intent
//
// The semantic-cache
// embedding path calls [Plan] to truncate the conversation to a shape the
// embedding model can process, then feeds the resulting messages to the
// embedding provider.  Smart-routing (packages/ai-gateway/internal/routing/llm)
// and AI Guard (packages/ai-gateway/internal/policy/aiguard) also consume this
// primitive — truncation policy is expressed once and surfaced uniformly in
// the admin UI via the shared InputStagingSelector component.
//
// # What this package does NOT do
//
// - No logging — pure compute, no side effects.
// - No Prometheus metrics — callers instrument at the use-site.
// - No external dependencies beyond the Go standard library.
//
// # Thread safety
//
// [Plan] and [Suggest] are stateless pure functions and are safe for
// concurrent use.
package inputstaging
