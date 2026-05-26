// Package defs is the root of the jobs/defs domain. It exports the shared
// interface types (PgxPool, AlertRaiser) consumed by all sub-domain packages:
//
//   - rollup/             — traffic, thing, cred-health, and provider-health rollup pipelines
//   - health/             — credential, provider, agent-cert, and cache-quality alert jobs
//   - expiry/             — credential, override, passthrough, VK, diag, enrollment, and auth expiry
//   - audit/              — audit-chain verification, freshness, and SIEM bridge
//   - drift/              — config drift, identity enrichment, smart-group recompute, stale-thing
//   - retention/          — data, job, ops, credential-circuit-flush, and stats-flush retention
//   - quota/              — quota threshold alert check
//   - metrics/            — device metrics rollup, ops rollup (1h, cascade)
//   - semanticcacheflush/ — blue/green Valkey vector index swap on embedding model change
//
// Production wiring (cmd/nexus-hub/wiring/jobs.go) imports each sub-package
// directly with a named alias to call its exported constructors.
package defs
