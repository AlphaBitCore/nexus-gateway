# Compliance Proxy Domain And Device Predicates

*Audience: operators building interception rules and contributors working on the domain matching or device-group evaluation code.*

Two shared libraries govern how the Compliance Proxy decides which traffic to intercept: `shared/policy/domain` matches hostnames against the interception domain ruleset, and `shared/policy/device` evaluates per-device smart-group membership predicates. The domain library is in the hot path of every CONNECT request — it decides whether the proxy intercepts or passes through each connection. The device library is used by the Hub's background job to compute device group memberships, and by the Control Plane UI's "test predicate" widget. Both are designed for high-volume evaluation with precompiled state.

---

## Domain matching — `shared/policy/domain`

### What it does

Every CONNECT request carries a `Host` header with the target hostname. The domain engine evaluates the hostname against the `InterceptionDomain` rows loaded from the database, returning the matching rule (or nil for pass-through).

### Match types

| `HostMatchType` | Behaviour | Example pattern | Matches |
|---|---|---|---|
| `EXACT` | Exact string match | `api.openai.com` | `api.openai.com` only |
| `GLOB` | Leading `*.` wildcard, any depth | `*.openai.com` | `api.openai.com`, `beta.api.openai.com` |
| `PREFIX` | `strings.HasPrefix(host, pattern)` | `api.open` | `api.openai.com`, `api.openassist.io` |
| `REGEX` | `regexp.Compile`-able pattern | `^api\d*\.openai\.com$` | `api.openai.com`, `api2.openai.com` |

For `GLOB`, only a leading `*.` wildcard is supported — it matches any depth of subdomains. The `**` form is not implemented; use `REGEX` for more complex subdomain patterns.

### Match ordering and atomic hot-swap

The engine compiles regex patterns at load time during `Engine.Swap`. A bad regex pattern causes the swap to fail (the old ruleset stays active); a bad config can never black-hole the proxy. The compiled matcher list is sorted by priority; on each CONNECT, the engine iterates the list and returns the first match.

Hot-swap happens when Hub broadcasts a config change:

```go
newEngine := domain.NewEngine()
newEngine.Swap(freshDomains)        // compile, sort
policyResolver.Store(newEngine)     // atomic pointer swap
```

In-flight connections keep using the previous engine; new connections use the fresh one. Zero coordination, zero blocking. The pattern is identical to the hook config hot-swap throughout the gateway.

### PathAction — per-path routing within a domain

Each `InterceptionDomain` row carries path-level routing via `PathAction`:

| `PathAction` | Behaviour |
|---|---|
| `PROCESS` | Run the full compliance pipeline (extract, hook, audit) |
| `PASSTHROUGH` | Relay bytes unbumped; no hook, no capture |
| `BLOCK` | Enum value present; current callers treat as `PASSTHROUGH` on the bumped-traffic path |

This allows fine-grained control within a domain: intercept `/v1/chat/*` on `api.openai.com` while passing through `/v1/images/*` (e.g., if image content should not be captured).

### Wildcard guardrails

Overly broad patterns are rejected at the admin write path before reaching the engine:

- `*` (matches every hostname) — rejected.
- `*.*` — rejected.
- Empty pattern — rejected.

The guardrails live in the Hub admin handler, not in the engine itself. Patterns that pass validation are compiled and run unchanged.

### API surface

```go
func NewEngine() *Engine
func (e *Engine) Swap(domains []InterceptionDomain) error
func (e *Engine) MatchHost(host string) *InterceptionDomain
func (e *Engine) PathAction(domain *InterceptionDomain, path string) PathAction
func (e *Engine) AllowlistEntries() []string
```

`AllowlistEntries` returns the flat hostname list used for the proxy's domain allowlist check — computed from the compiled engine so there is no separate allowlist data structure.

## Device predicates — `shared/policy/device`

### What it does

The device predicate library evaluates a JSON predicate expression against a single device's attributes. It is used in two places: the Hub's background job that recomputes `device_group_membership_cache` rows every 60 seconds, and the Control Plane UI's "Test pattern" widget that lets admins preview how many devices a predicate currently matches.

The Compliance Proxy itself does not evaluate device predicates at request time — device group membership is pre-computed and cached by the Hub.

### Predicate shape

Predicates are JSON wire shapes, not a constructor DSL:

```json
{
  "all": [
    {"field": "os",               "op": "in",     "value": ["darwin", "linux"]},
    {"field": "agentVersion",     "op": "ge",     "value": "1.5.0"},
    {"field": "primaryIp",        "op": "cidr",   "value": "10.32.0.0/16"},
    {"field": "boundUserOrgPath", "op": "prefix", "value": "corp/finance/"}
  ]
}
```

The top-level key is either `all` (logical AND) or `any` (logical OR). Exactly one must be present. Nested groups are not currently supported.

### Supported fields

| Field | Type | Notes |
|---|---|---|
| `os` | string | `darwin`, `linux`, `windows` |
| `osVersion` | string | Semver-aware comparison |
| `agentVersion` | string | Semver-aware comparison |
| `hostname` | string | Exact or regex match |
| `primaryIp` | CIDR | `cidr` op checks subnet membership |
| `physicalId` | string | Hardware identifier |
| `status` | string | Enrollment status |
| `boundUserId` | string | Enrolled user's identity |
| `boundUserOrgPath` | string | User's org hierarchy path |
| `enrolledAt` | timestamp | `relative_seconds_within` op available |
| `lastHeartbeat` | timestamp | Staleness check |
| `idpGroup` | string (sentinel) | `idp_group_member` op |
| `tags` | string[] (sentinel) | `tags_contains`, `tags_contains_all` ops |
| `metadata.<key>` | string | Custom metadata fields |

### Supported operators

| Operator | Description |
|---|---|
| `eq`, `ne` | Exact match / not-equal |
| `in`, `nin` | Set membership / exclusion |
| `prefix` | String prefix match |
| `regex` | Regexp match |
| `cidr` | IP subnet membership |
| `lt`, `le`, `gt`, `ge` | Numeric or semver comparison |
| `tags_contains` | Single tag present |
| `tags_contains_all` | All listed tags present |
| `idp_group_member` | Device's user is member of named IdP group |
| `relative_seconds_within` | Timestamp within N seconds of now |

### Evaluate function

```go
func Evaluate(p Predicate, d *Device, nowSec int64) (bool, error)
```

No exported per-field structs or per-operator constructors exist — they would invert the design. Operators author the JSON; the evaluator stays standalone. The Control Plane UI's "Test pattern" widget calls the Hub-side recompute endpoint so what admins preview matches what the runtime evaluates.

## How the two libraries relate

The Compliance Proxy uses `shared/policy/domain` on every CONNECT to decide which traffic to intercept. The `shared/policy/device` library is used by the Hub and Control Plane to build the device group memberships that feed into per-device policy enforcement across all three traffic paths. The Compliance Proxy does not currently evaluate device predicates at request time; device-group-based policy filtering is resolved before the request reaches the proxy via Hub-pushed config.

---

## Canonical docs

- [`domain-device-predicate-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/compliance-proxy/domain-device-predicate-architecture.md) — domain engine API, glob semantics, wildcard guardrails, device predicate fields and operators
- [`compliance-pipeline-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/compliance-proxy/compliance-pipeline-architecture.md) — §5 (domain/path match in the request lifecycle)

**Adjacent wiki pages**: [Compliance Proxy Overview](Compliance-Proxy-Overview) · [Compliance Proxy TLS Interception](Compliance-Proxy-TLS-Interception) · [Thing Model And Config Sync](Thing-Model-And-Config-Sync) · [Agent Policy Evaluation](Agent-Policy-Evaluation)
