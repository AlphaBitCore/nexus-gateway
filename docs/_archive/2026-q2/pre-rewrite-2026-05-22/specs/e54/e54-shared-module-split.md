# E54 — Split `shared/` driver-scoped subpackages into their own Go modules

**Tracker:** CLAUDE.md "Dependencies in shared" tech-debt note.
**Status:** queued architectural cleanup; no functional impact; no
customer signal driving urgency.

## Background

`packages/shared/` is one Go module that owns a small "core" set
(pgx, slog, prometheus, gjson/sjson, yaml, opentelemetry, golang.org/x/*)
plus a larger "driver-scoped" set of subpackages each pulling its own
heavy dep:

| Subpackage | Drives in |
|---|---|
| `shared/mq/natsmq` | nats-io/nats.go |
| `shared/spillstore/s3` | aws-sdk-go-v2 (multiple modules) |
| `shared/cache` | redis/go-redis/v9 |
| `shared/spillstore/redis` | redis/go-redis/v9 |
| `shared/thingclient` | coder/websocket |
| `shared/auth/jwt` (and verifiers) | golang-jwt/jwt/v5 |
| `shared/quotastore/redis` | redis/go-redis/v9 |
| `shared/cache/certcache` | hashicorp/golang-lru/v2 |
| `shared/dedup`, `shared/compliance/bloom` | bits-and-blooms/bloom/v3 |
| `shared/echohandler` (when present) | labstack/echo/v4 |

Today every consumer of `packages/shared/*` transitively pulls all
of those because they live in the same `go.mod`. ai-gateway / cp /
compliance-proxy / hub / agent — every binary's vendor tree bloats.

## Why bother

Two distinct pains:

1. **Build / test isolation.** Spinning up a CI test for just
   `shared/hooks` (pure stdlib) needs aws-sdk + redis-go-9 +
   nats-go fetched first. Slow + flaky on unreliable mirrors.

2. **Surface area for new contributors.** A go-doc explore of
   `shared` shows ~100 importable packages. The mental model is
   "this huge sprawling module" rather than "core + 7 narrow
   drivers".

Neither pain is operationally critical. Today's `go build` is
sub-second once deps are cached. This is hygiene work.

## Approach

The Go-canonical pattern for splitting a module is:

1. Add a new `go.mod` inside the subdir (`packages/shared/transport/mq/natsmq/go.mod`).
2. Update `go.work` to list the new module.
3. The new module imports the parent `shared` types it depends on
   via the parent's module path; the parent does NOT import back
   (cycle).
4. Bump consumer `go.mod` files to require the split-out module
   only when they wire that driver.

## Per-subpackage SDD sketches

### S1 — Split `shared/mq/natsmq`

- Move `mq.Producer` + `mq.Consumer` interfaces into a tiny
  `shared/mq` core (which they already live in — just clean up
  the type assertions that leak natsmq specifics).
- New module `packages/shared/transport/mq/natsmq` with its own go.mod.
- Update `go.work`.
- Consumer go.mod changes: ai-gateway / cp / compliance-proxy / hub
  all import `shared/mq/natsmq` directly (already done — just add
  the new module to their `go.mod` require lines explicitly).

**Estimate:** 1 day. Risk: low (interface boundaries clean today).

### S2-S7 — Same pattern for spillstore/s3, cache (redis), cache/certcache,
spillstore/redis, thingclient (coder/websocket), auth/jwt verifiers,
bloom/lru dedup subpackages.

Each is ~½ day if the interface seam is clean (most are). The
ordering doesn't matter — splits are independent. Recommend doing
mq/natsmq first as the worked example since natsmq's seam
(`mq.Producer` / `mq.Consumer`) is the cleanest.

### S8 — CI lint to prevent the umbrella from re-bloating

Add a CI check that runs `go mod why -m <driver-dep>` from
`packages/shared/go.mod` for each driver in the table above. If any
match (= driver dep crept back into the core module), fail the
build with a pointer to the offending importer.

**Estimate:** 2 hours.

## Acceptance

- `cd packages/shared && go build ./...` no longer pulls nats-go /
  aws-sdk / redis / coder-websocket / jwt / bloom / lru into the
  core module's dependency closure.
- Each split subpackage has its own `go.mod` listed in `go.work`.
- All existing consumers continue to build + test green.
- CI lint detects regressions.

## Out of scope

- API redesigns. This is a pure mechanical move.
- Vendor-mode dependency vendoring (we use modules, not vendor/).
- Splitting `shared/hooks` further (it's already core).

## Risk + rollout

- **Risk:** medium. `replace` directives in consumer `go.mod` files
  are forbidden by CLAUDE.md, so the split must work via `go.work`
  + module-tagged versions. Local dev is fine; cross-repo consumers
  (none today, but planned for the public SDK) would need tagged
  releases of the split modules.
- **Rollout:** one PR per split subpackage so a single revert is
  surgical. CI catches breakage at the per-subpackage layer.

## Priority

This is the lowest-priority item in the entire backlog. Defer until
either (a) a real CI / build-time pain forces the split or (b) we
need to publish one of these subpackages as a standalone OSS module.
