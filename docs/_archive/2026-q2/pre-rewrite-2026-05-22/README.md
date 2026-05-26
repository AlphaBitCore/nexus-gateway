# Archived — pre-rewrite snapshot, 2026-05-22

This subtree holds every doc that used to live under `docs/` before the 2026-05-22 archive sweep. **Treat the content as untrustworthy for current work** — it is preserved for archaeology only, not as a source of truth.

## Why this is here

Across 16 commit passes on `docs/cache-readme-align`, every fresh fact-check sweep against current code kept surfacing material drift:

- Entire architectures described that do not exist in code (quota Lua sliding window; Cat C config category; agent-ui `GET /local/<X>` HTTP endpoints; MQ 5-stream layout vs the real 2-stream layout; jobs/MQ interfaces with phantom types; SIEM bridge envelope schema-version).
- Wrong service ownership of CRUD (multi-endpoint Flow 1/2/3/6 said admin actions "forward to Hub" which then "Insert / Persist / Encrypt" — reality is CP-owned CRUD with Hub as signal-only).
- Wrong HTTP verbs / wrong endpoint paths / wrong file-line anchors (PATCH where real is PUT or POST; phantom `tools/db-migrate/prisma/` subdir; wrong mount.go line ranges; fabricated audit event types).
- Wrong constants / wrong defaults / wrong enum value lists (Thing status `online|degraded|offline` vs real `enrolled|online|offline|drift|revoked`; ErrorClass 9-value vs real 4-value; embedding singleflight 100ms vs real 5s; cert cache "~256 entries" vs exact `cache.NewLRUCache(256)`; PR-9 migration status PARTIAL vs LANDED).
- Wrong UI dimensions / wrong feature surfaces (Cache ROI by VK/org vs real by adapter; agent-ui pages described that don't exist on disk).
- Archaeology language leaking into binding text (`previously`, `was retired`, `(EN-SX incident)`, dated parentheticals, commit-hash citations).

Each individual fix is recorded in the commits `c21850df0` → `48df66b4b` on this branch. The fixes themselves were valuable — they discovered real product-doc bugs (e.g. smart-routing timeout doc said 1 s when code is 10 s; alert-evaluation doc said "retry 3x" when code does no retries; `traffic_event.virtual_key_id` column claim when real columns are `entity_type`/`entity_id`/`entity_name`). But the cumulative drift was too large to repair in place; the wiki single-source-of-truth bar made an in-place patch path untenable.

## What's in this snapshot

- `architecture/` — every `docs/developers/architecture/*.md` as of 2026-05-22 (90 files: services + cross-cutting + foundation + observability + safety + shared + storage + ui).
- `workflow/` — `docs/developers/workflow/*.md` (8 files).
- `features/` — `docs/users/features/*.md` across `cp-ui/ + agent-ui/ + flows/` (27 files).
- `product/` — `docs/users/product/*.md` (5 files).
- `ai-gateway-client-guide.md` — the lone non-OpenAPI user-API doc.
- `operators/` — `docs/operators/**/*.md` (20 files: ops + runbooks).
- `api/` — `docs/users/api/` including `openapi/` YAML specs.
- `specs/` — `docs/developers/specs/` including SDD stories + decision logs + the 2026-05-22 doc-review decision log.
- `roadmap.md` — `docs/developers/roadmap.md` snapshot.
- `handoffs/` — `docs/handoffs/E76-wiki-expansion/` snapshot.
- `_wiki/` — `docs/_wiki/` content snapshot.
- `top-level-README.md` — old `docs/README.md`.
- `developers-README.md` — old `docs/developers/README.md`.
- `users-README.md` — old `docs/users/README.md`.

## How to navigate after the archive

The rewrite, when it lands, will populate a fresh `docs/` tree from code first. Until then:

- For code behavior, **read code** (`packages/**`).
- For binding rules, read **`CLAUDE.md`** at the repo root.
- For contributor workflow, read **`CONTRIBUTING.md`** at the repo root.
- For the running product narrative, read the root **`README.md`** + **`README.zh-CN.md`**.
- For audit / runbook execution today, read the Claude Code skills under `.claude/skills/` (each one carries the operational truth for its surface).
- For schema, read `tools/db-migrate/schema.prisma`.
- For OpenAPI surface, the openapi YAML lives in this archive under `api/openapi/` — treat as a starting point, not a contract.
- The Git history of this branch (`docs/cache-readme-align`) carries the discovered code anchors in commit messages — `git log --all` is a reasonable index into "what surface lives where" for the rewrite.

The fresh `docs/` will be code-anchored by construction with a mechanical lint gate (`scripts/check-doc-code-anchors.mjs`) to prevent recurrence of the drift documented above.
