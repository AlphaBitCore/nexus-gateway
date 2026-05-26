// Package redisfactory builds a [github.com/redis/go-redis/v9.UniversalClient]
// from a unified yaml + env schema shared by every Nexus Gateway service that
// talks to Redis (Hub, Control Plane, AI Gateway, Compliance Proxy).
//
// Three deployment modes are supported behind a single Config:
//
//   - standalone: a single Redis instance (the default for local dev and most
//     prod deployments).
//   - sentinel: failover-managed primaries via [redis.FailoverOptions].
//   - cluster: sharded Redis cluster via [redis.ClusterOptions].
//
// On top of mode selection, the factory exposes:
//
//   - ACL credentials (Redis 6+ username + password + db). Leave username
//     blank to fall back to the legacy AUTH-only password flow.
//   - TLS, including mTLS (caFile + certFile + keyFile + serverName) and
//     an explicit insecureSkipVerify escape hatch for dev.
//   - Connection pool tuning (poolSize, minIdleConns, maxRetries) and the
//     four canonical timeouts (dialTimeout, readTimeout, writeTimeout,
//     poolTimeout).
//
// The companion [Env] type collects every REDIS_* environment variable
// services may use to override yaml values at boot. Callers obtain it via
// [LoadEnv] and pass it alongside the yaml-derived [Config] to [New], which
// applies env-wins precedence (L3 > L2 per the project's configuration
// architecture) before constructing the underlying client.
//
// The authoritative field-by-field contract lives in
// docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md §9.
package redisfactory
