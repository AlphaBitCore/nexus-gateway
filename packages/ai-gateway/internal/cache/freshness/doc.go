// Package freshness implements the time-sensitive prompt detector for the
// AI Gateway response cache.
//
// # Overview
//
// Some prompts ask about information that changes continuously — stock prices,
// weather conditions, live scores, exchange rates, breaking news. Serving a
// cached response for these queries is worse than a cache miss: the answer is
// factually stale from the user's perspective and, if stored, poisons L2 with
// time-bound content.
//
// The Detector evaluates every incoming conversation against a list of
// configurable Rules before any cache lookup or write is attempted. When a
// rule fires, the caller skips BOTH the extract (L1) and semantic (L2) tiers
// for that request.
//
// # Algorithm
//
// Each Rule specifies one or more keywords plus two optional co-occurrence
// requirements:
//
//   - RequireQuestionMark — the message must contain a literal "?" or "？"
//     (Unicode full-width question mark). This guards against discourse-particle
//     false positives: "Use this now" does not fire; "What's the stock price
//     now?" does.
//   - RequireEntity — the message must contain a recognized entity: an uppercase
//     ticker-like token (AAPL, TSLA), a number of two or more digits (price,
//     year), a common currency symbol or code (USD, CNY, €, ¥), or ZH currency
//     words (元, 美元, 欧元). This prevents "stock price as a metaphor" from
//     triggering the skip.
//
// Detection always operates on the last user-role message in the conversation.
// Earlier turns are ignored.
//
// # Rule provisioning
//
// This package owns no default rule list. The canonical defaults live in
// tools/db-migrate/seed/data/time-sensitive-rules.json, are written into the
// DB by seed.ts on a fresh install, and reach the AI Gateway via the Hub
// shadow push (configKey response_cache.time_sensitive_patterns). Callers
// construct Detector with nil or an empty list at boot; Detector.Reload
// installs the real rules when the first shadow snapshot arrives.
//
// # Metrics
//
// NewDetector accepts a Prometheus namespace string and registers
// nexus_cache_freshness_skips_total{rule_id, language} via promauto. Pass a
// custom prometheus.Registerer to isolate tests from the default registry.
//
// # Dependency policy
//
// This package intentionally depends only on the Go standard library plus the
// Prometheus client library. It must not import any upstream cache packages so
// that the dependency arrow runs from cache/core → cache/freshness, not the
// other way around. Callers project their own canonical message type into the
// local ChatMessage struct.
package freshness
