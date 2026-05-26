---
doc: domain-device-predicate-architecture
area: service
service: compliance-proxy
tier: 1
---

# Domain & Device Predicate Library

> **Tier 3 architecture doc.** Read when touching `packages/shared/policy/domain/` or `packages/shared/policy/device/`. These libraries provide reusable matchers consumed by policy / exemption / interception-domain code across the compliance proxy + agent + Hub.

---

## 1. What they do

| Library | Purpose |
|---|---|
| `shared/policy/domain` | Match hostnames against `interception_domain` rows (EXACT / GLOB / PREFIX / REGEX) and resolve per-path `PathAction` |
| `shared/policy/device` | Evaluate a smart-group membership predicate against a single device's attributes (E52-S2 wire shape) |

Both are designed for **hot-path** evaluation — millions of evaluations per minute under load. They prefer compiled state + fast lookup over flexibility.

## 2. `shared/policy/domain` API

```go
// Engine is the runtime decision engine. Constructed empty; reloaded
// atomically via Swap when configloader returns a new domain list.
type Engine struct { /* ... */ }

func NewEngine() *Engine
func (e *Engine) Swap(domains []InterceptionDomain) error
func (e *Engine) MatchHost(host string) *InterceptionDomain
func (e *Engine) PathAction(domain *InterceptionDomain, path string) PathAction
func (e *Engine) AllowlistEntries() []string
```

`InterceptionDomain` carries `HostMatchType` ∈ {`EXACT`, `GLOB`, `PREFIX`, `REGEX`} mirroring the DB enum. `PathAction` ∈ {`PROCESS`, `PASSTHROUGH`, `BLOCK`} — `BLOCK` matches the Prisma enum even though current callers treat it as `PASSTHROUGH` for the bumped-traffic path.

There is no separate `Matcher` interface or `NewGlobMatcher` / `NewRegexMatcher` / `NewSuffixMatcher` / `NewExactMatcher` constructor — `Engine.Swap` accepts the full row list and compiles regexes internally (failed regex compilation rejects the swap so a bad config can't black-hole the proxy).

## 3. Glob semantics

For `HostMatchType == GLOB`, the engine supports a single leading `*.` wildcard — matches any subdomain depth — and exact match otherwise:

```
*.openai.com         → matches api.openai.com AND foo.api.openai.com (any depth)
api.openai.com       → exact match only
```

`HostMatchType == REGEX` accepts arbitrary `regexp.Compile`-able patterns; `HostMatchType == PREFIX` does a `strings.HasPrefix(host, pattern)` check. The doubled `**` glob form is *not* implemented — use `REGEX` instead. Compilation happens during `Swap`; runtime matching iterates the priority-sorted matcher list (first match wins) and is O(N) over enabled domains.

## 4. Wildcard guardrails

Patterns that would explode the match set are rejected at the admin write path before reaching the engine:

- `*` (matches everything) — rejected; specify at least one literal label.
- `*.*` — rejected; ambiguous.
- Empty pattern — rejected.

The engine itself does not enforce these (it just compiles whatever it's handed); guardrails live in the Hub admin handler.

## 5. `shared/policy/device` predicate shape

Per E52-S2 SDD, the predicate is a JSON wire shape — not a Go-constructor DSL. Wire shape:

```json
{
  "all": [
    {"field": "os",                "op": "in",      "value": ["darwin", "linux"]},
    {"field": "agentVersion",      "op": "ge",      "value": "1.5.0"},
    {"field": "primaryIp",         "op": "cidr",    "value": "10.32.0.0/16"},
    {"field": "boundUserOrgPath",  "op": "prefix",  "value": "corp/finance/"}
  ]
}
```

Top-level wrapper is `all` (logical AND) or `any` (logical OR) — exactly one of them must be set; nested groups are not currently allowed (YAGNI per SDD).

Closed sets:

| Element | Members |
|---|---|
| `field` | `os` · `osVersion` · `agentVersion` · `hostname` · `primaryIp` · `physicalId` · `status` · `boundUserId` · `boundUserOrgPath` · `enrolledAt` · `lastHeartbeat` · `idpGroup` (sentinel) · `tags` (sentinel) · `metadata.<key>` |
| `op` | `eq` · `ne` · `in` · `nin` · `prefix` · `regex` · `cidr` · `lt` / `le` / `gt` / `ge` (semver-aware for dotted versions) · `tags_contains` · `tags_contains_all` · `idp_group_member` · `relative_seconds_within` |

API:

```go
func Evaluate(p Predicate, d *Device, nowSec int64) (bool, error)
```

No exported `OSPredicate` / `OSVersionPredicate` / `IPRangePredicate` / `UserAgentPredicate` / `AndPredicate` / `OrPredicate` / `NotPredicate` constructors exist — they would invert the design. Operators author the JSON; the matcher stays standalone and tiny.

## 6. Consumers

| Service | Use case |
|---|---|
| Compliance proxy | `shared/policy/domain` for `interception_domain` rules; not a `shared/policy/device` consumer |
| Agent | `shared/policy/domain` for in-scope policies |
| Hub | `shared/policy/device` for per-heartbeat + 60s safety job that recomputes `device_group_membership_cache` rows |
| CP admin API | `shared/policy/device` dry-run preview ("how many devices does this predicate match right now") |

The CP UI's "Test pattern" widget calls the Hub-side recompute so what admins see is what runtime evaluates.

## 7. Testing

Both libraries are heavily unit-tested with table-driven tests covering:

- Common patterns (single-label glob, suffix, exact).
- Edge cases (empty input, malformed pattern, very long input).
- The 95%-coverage gate applies (see `unit-test-coverage-95.mdc`).

## 8. Future consolidation

The `shared/iam/` NRN matcher uses a similar pattern model. There's mild overlap that could be consolidated into a single `shared/match/` package. Not urgent — would touch many files; current setup is fine.

## 9. Cross-references

- `compliance-pipeline-architecture.md` §5 — domain matching for interception rules.
- `agent-forwarder-architecture.md` §3 — agent uses domain matching to scope intercept.
- `iam-identity-architecture.md` §2 — sibling NRN matcher.
- `shared-package-architecture.md` — where both libraries fit in the package catalogue.
