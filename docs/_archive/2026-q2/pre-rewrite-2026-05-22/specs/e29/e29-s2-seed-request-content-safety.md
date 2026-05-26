# E29 S2 — Request-stage Content-Safety Seed + Spec Alignment

## Story

As a compliance operator I need an inbound `content-safety` hook
pre-wired in the seed so I can enable request-stage content safety
in one click instead of hand-rolling an extra row.

## Scope

- `tools/db-migrate/seed/seed-hook-configs.ts`
- `docs/developers/specs/e29/e29-hooks-region-rulepack-refactor.md`
  (already captures the full scope)
- `docs/users/product/architecture.md` (appends the hook input runtime
  semantics paragraph with IP / region source-of-truth)

## Tasks

1. Add a `request-content-safety` row to `SEED_HOOK_CONFIGS` mirrors
   the existing `response-content-safety` row but with
   `stage: 'request'` and `applicableIngress: ['AI_GATEWAY',
   'COMPLIANCE_PROXY']`. Default-disabled.
2. Update `docs/users/product/architecture.md` to clarify that
   `HookInput.SourceIP` is extracted via `middleware.ClientIP` and
   that `HookInput.ProviderRegion` is taken from provider metadata
   on the resolved routing target.

## Acceptance criteria

- `npx prisma db seed` inserts a second `content-safety` row at
  `request` stage; running seed again does not duplicate it (seed
  path uses the same `(name, stage)` idempotency as every other
  row).
- Architecture doc renders without dangling section references.
