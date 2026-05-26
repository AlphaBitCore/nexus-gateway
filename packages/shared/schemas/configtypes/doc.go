// Package configtypes groups the five config-domain sub-packages:
//
//   - configtypes/enums         — shared enum types (BumpStatus, …)
//   - configtypes/identity      — identity and token schemas
//   - configtypes/interception  — network interception schemas (Killswitch, domains, paths)
//   - configtypes/observability — metrics and diagnostics schemas
//   - configtypes/policy        — AI-guard, retry, override-policy, hook, and model-governance schemas
//
// Import the specific sub-package for the types you need rather than this
// root package directly.
package configtypes
