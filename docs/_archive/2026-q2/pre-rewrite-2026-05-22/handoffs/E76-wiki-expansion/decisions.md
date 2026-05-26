# E76 Wiki — Decision log

> Decisions made while authoring the public-facing Wiki for OSS evaluators.
> Each entry: decision, why, what was rejected, source of authority.
> Conventions follow other in-repo decision logs (e.g. T3-DEC-* used in the
> 2026-05-21 doc audit).

## E76-DEC-001 — Lock the sidebar at 8 pages

**Decision**: Use exactly the IA from E76-S1: Home / Getting Started / Architecture / Deployment / FAQ / Contributing / Roadmap / Security.

**Why**: Eight pages cover every common evaluator job-to-be-done. Any further split (a separate "Configuration" or "Operations" page) duplicates content already in `docs/operators/ops/` and adds a maintenance surface for marginal value. CLAUDE.md "less is more" — extending an existing section beats adding a new one.

**Rejected**:
- "API Reference" — covered by `docs/users/api/openapi/` and explicitly out of scope per the user goal ("除开 sdd 文档和 openapi yaml 文件").
- "Configuration" — environment variables live in `.env.example` (source of truth); the wiki Deployment page links there rather than restating.
- "Troubleshooting" — folded into Getting Started (most failure modes show up at bootstrap time).
- "Glossary" — `docs/users/product/overview.md` covers the term mapping; wiki readers who need it follow the link.

**Source of authority**: `docs/developers/roadmap.md` §E76 Stories; CLAUDE.md "Less is more / delete instead of add".

## E76-DEC-002 — Wiki ≠ README; the wiki ORIENTS, README LANDS

**Decision**: Wiki pages do not duplicate README content. Where README covers a topic adequately, link to the README section. Where README is too terse for a first-time evaluator, the wiki expands. Wiki pages always link DOWN into `docs/` for module-level depth.

**Why**: README is the GitHub repo landing surface and already contains 80% of what an "Architecture in one minute" wiki page would say. Re-stating it creates drift (a later README rewrite would leave the wiki stale). The wiki's role is structured orientation — a sidebar a reader navigates — not a competing copy of the same content.

**Rejected**: copy/paste from README to wiki. (Tempting because it produces a fuller-looking wiki faster, but creates two sources of truth.)

**Source of authority**: CLAUDE.md "Code / doc lockstep" — single source of truth; this generalises to wiki vs README.

## E76-DEC-003 — Author pages with parallel Sonnet subagents

**Decision**: One author subagent per page, dispatched in parallel. Each agent gets: page name, audience, job, section outline, exact source-doc paths to read, the wiki/README boundary rule, the "link don't duplicate" rule, a 2-round self-audit instruction, and a request for the touched-file list.

**Why**: Pages are independent (no cross-page dependencies once the IA is locked). Parallel dispatch is the fastest path. CLAUDE.md sub-agent dispatch discipline matches: bounded scope, parallel-safe, report-back is enough.

**Source of authority**: CLAUDE.md "Sub-agent dispatch discipline" (a/c categories).

## E76-DEC-004 — Output target is `docs/_wiki/` in this repo

**Decision**: Author the wiki markdown files into `docs/_wiki/` in the main repo (under `feature/E76` worktree). A maintainer later pushes them to the `nexus-gateway.wiki` repo when ready.

**Why**: Keeps the content under version control, reviewable in a PR, and within reach of the lockstep checks. Pushing directly to the wiki repo (a separate git repo with its own history) would bypass the SDD review process.

**Rejected**: cloning the wiki repo as a submodule. (Two-repo dance for a one-time content push; submodule overhead exceeds value.)

**Source of authority**: convention — the doc-audit program shipped 120 docs via the same `docs/`-in-repo path.

## E76-DEC-005 — `docs/_wiki/` is excluded from doc-lockstep

**Decision**: `docs/_wiki/` files are authored snapshots intended for the GitHub Wiki. They are NOT load-bearing in the code/doc lockstep map and do NOT need to update on every code change. The canonical docs under `docs/developers/`, `docs/users/`, `docs/operators/` remain the single source of truth.

**Why**: Wiki pages summarise / re-organise canonical docs. If a code change updates the canonical doc, the wiki will eventually be re-rendered from it — but blocking every PR on wiki freshness creates pointless churn.

**How to enforce**: `docs/_wiki/**` is NOT added to `scripts/doc-lockstep.config.mjs`. A periodic regeneration cadence (e.g. every release) keeps the wiki within drift tolerance.

**Source of authority**: CLAUDE.md "Code / doc lockstep" binding scope is module-level docs; the wiki sits outside that scope by design.

## E76-DEC-006 — Code cleanup found during the survey

**Decision**: any obsolete code or dead doc reference spotted during the wiki survey gets recorded here with: file path, what's stale, how I verified it's safe to remove, then handled in a SEPARATE commit (not bundled with the wiki content).

**Why**: bundling cleanup with content authoring violates commit hygiene (CLAUDE.md "commit reminder" + the 2026-05-21 incident of mixing 62 parallel-work files into a refactor commit). Keep the wiki PR atomic on wiki content; keep cleanups atomic on their own scope.

**Verification gate for any removal**: (a) `git grep` confirms no other file references the symbol/path; (b) `git log -- <file>` to understand history; (c) if unsure, leave it and add a row here noting the doubt.

**Findings during the E76 survey**:

| Finding | Status | Verification |
|---|---|---|
| References to deleted packages `shared/heartbeat`, CP `internal/pubsub`, CP `internal_registry` in `docs/developers/architecture/overview.md:63` and `docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md:135` | **Keep** — these are intentional archaeological notes ("if seen elsewhere, treat as stale"), not stale references themselves | `grep -rEn '...' docs/ packages/` — only the two intentional callouts remain; no other consumers |
| `TODO`/`FIXME`/`XXX` strings in wiki output | **Clean** — the two grep hits (`Roadmap.md:99` IdP "stub" prose, `Contributing.md:167` lint description) are legitimate prose, not editorial markers | `grep -rEn '\b(TODO\|FIXME\|XXX)\b' docs/_wiki/` |
| Wiki frontmatter | **Clean** — every page starts with `# <Page>` heading; no YAML frontmatter | `head -3 docs/_wiki/*.md` |

No obsolete code or dead docs were spotted during the survey that warrant a separate cleanup commit. The 2026-05-21 doc audit (commit `b467b03f8`) already aligned 120 docs with code and deleted 3 dead scripts; the survey reaffirms that work.

## E76-DEC-010 — All deep-doc links use absolute GitHub URLs

**Decision**: Every link from a wiki page to a file in the main repository uses an absolute URL of the form `https://github.com/AlphaBitCore/nexus-gateway/blob/main/<path>`. No relative paths (`../`, `../../`) appear in any wiki page output.

**Why**: Wiki pages are published to a separate `nexus-gateway.wiki` git repository (a sibling of the main repo). Relative paths that work in the `docs/_wiki/` worktree (`../developers/...`) do NOT resolve once pushed to the wiki repo — they would 404. Absolute URLs are the only format that survives the publish step.

**Mechanics**: One agent (Contributing.md) authored absolute URLs from the start. Seven other pages used relative paths and were rewritten in a single transform pass via a Python script that:
1. Fixed inconsistent `../X` references to repo-root files that should have been `../../X` (Security.md had 4 such bugs that would have 404'd even in the worktree).
2. Converted `../../<path>` → `https://github.com/AlphaBitCore/nexus-gateway/blob/main/<path>`.
3. Converted `../<path>` → `https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/<path>`.

Inter-wiki page links continue to use GitHub Wiki page-name syntax (`[Architecture](Architecture)`) — these are NOT relative paths; they are wiki-internal references and remain unchanged.

**Source of authority**: GitHub Wiki documented behavior — the wiki repo is independent of the main repo's file tree; cross-repo links require absolute URLs.

## E76-DEC-009 — Doc gaps surfaced by author subagents (logged, NOT fixed in E76)

Two minor doc gaps were flagged by author agents during research. These are recorded here for follow-up but are intentionally NOT fixed in the E76 PR (E76 is wiki authoring, not doc remediation):

| Gap | Source | Suggested follow-up |
|---|---|---|
| Air-gapped deployment has no operator runbook under `docs/operators/ops/runbooks/`. The architecture is compatible (no mandatory runtime outbound internet) but no step-by-step playbook exists. The FAQ answer states this directly rather than inventing one. | FAQ author subagent open question | New runbook `docs/operators/ops/runbooks/air-gapped-deployment.md` — file as a roadmap row or carve a follow-up task. Not blocking E76. |
| `docs/developers/architecture/cross-cutting/safety/credentials-architecture.md` describes the VK secret hash as "constant-time" without naming the algorithm. The actual algorithm (HMAC-SHA256) is only readable from `packages/control-plane/internal/vkauth/`. | Security author subagent open question | One-line addition to the architecture doc citing HMAC-SHA256. Not blocking E76; the wiki Security page sources HMAC-SHA256 directly from the implementation. |

These gaps were captured here so future doc-audit or architecture passes can pick them up without re-discovery.

## E76-DEC-007 — Markdown frontmatter on each wiki page

**Decision**: NO frontmatter on the wiki pages. GitHub Wiki does not parse frontmatter; it would render as literal YAML at the top of the page.

**Why**: matches what's actually visible to the wiki reader. The wiki source path itself records the page identity.

**Rejected**: copying the `doc:` / `area:` / `tier:` / `updated:` frontmatter pattern used in `docs/developers/architecture/overview.md`.

## E76-DEC-008 — Mermaid diagrams allowed; no other rendering deps

**Decision**: Wiki pages may use Mermaid (GitHub Wiki renders `mermaid` fenced blocks). No SVGs, no images, no PlantUML, no LaTeX.

**Why**: Mermaid renders natively in GitHub Wiki without external assets. Anything else creates an asset-management problem (where do the SVGs live? how do they update?).

**Source of authority**: GitHub Wiki documented Markdown support.

---

# E76 Wiki v2 expansion — DEC-011 onward

> 2026-05-21: the user reviewed the shipped 8-page wiki and judged it "too
> few, too poor" for a mature OSS product. A v2 IA expansion (`_outline.md`)
> was designed and locked, taking the wiki from 8 pages to ~95 pages, organised
> into 5 super-buckets (Landing / Product / Getting Started / Technical /
> Development / Community) and 19 groups. This section logs the v2 expansion
> decisions.

## E76-DEC-011 — Adopt IA v2; supersede the 8-page lock

**Decision**: Replace the 8-page sidebar IA (DEC-001) with the ~95-page,
19-group, 5-super-bucket structure documented in `_outline.md`. Existing 8
pages refactor into the new structure rather than being discarded.

**Why**: The 8-page IA gave evaluators a one-pass orientation but did not match
the depth a mature OSS product offers. The repo has 329 dev docs, 35 user
feature docs, 18 ops docs — the wiki was indexing a tiny fraction of it. A
serious evaluator landing on the wiki could not find: a comparison vs LiteLLM,
a deployment recipe, an API reference catalog, the workbench methodology, or
recipes for adding a provider. The v2 IA closes those gaps without duplicating
canonical docs.

**Rejected alternatives**:

- *Stay at 8 pages, link out for everything* — every wiki link going off-wiki
  defeats the purpose of having a wiki. The wiki should answer "what is this,
  what does it do, how do I run it, how do I extend it" within the wiki
  surface.
- *80 pages but no Features group* — Features (10 capability cards) is what
  evaluators deeplink to share with stakeholders ("here's the PII redaction
  page, this is what we'd buy"). Skipping Features keeps the wiki
  contributor-focused at the expense of the most important audience
  (decision-makers).
- *60 pages, no Workbench group* — the AI vibe-coding workbench is a project
  differentiator the OSS community asks about. Burying it under "Contributing"
  hides it. A top-level Workbench group (6 pages including a Forking Guide)
  makes it discoverable.

**Source of authority**: user direction 2026-05-21 ("作为一个成熟的完善的oss
产品 需要什么样的wiki组织结构"); mature OSS IA patterns (HashiCorp Vault,
Envoy, Linkerd, Coder); CLAUDE.md "less is more" applied at the right level
(per-page tight, per-IA comprehensive).

## E76-DEC-012 — Top-level partition by audience/purpose, not by service

**Decision**: 5 super-buckets — Landing / Product / Getting Started /
Technical / Development / Community. Service-specific groups (AI Gateway,
Compliance Proxy, Desktop Agent, Control Plane) live INSIDE the Technical
bucket, not as top-level groups.

**Why**: First-time wiki visitors split into ~4 archetypes — evaluator,
operator, contributor, end-user. Top-level bucketing by audience routes each
archetype to their starting group in one click. Service-first IA forces an
evaluator to learn the 5-service split before they can navigate, which is the
wrong order.

**Rejected**: top-level by service (AI Gateway / Compliance Proxy / Desktop
Agent / Control Plane / Cross-Cutting at top level) — clearer for contributors
already familiar with the architecture, harder for the bigger audience of
evaluators.

## E76-DEC-013 — Features as 10 capability cards, not one matrix

**Decision**: The Product → Features group has 10 pages, one per major
capability (Multi-Provider Routing, Smart Routing, Prompt Cache, Response
Cache, Cost Tracking, PII Redaction, Audit & SIEM, IAM & SSO, Desktop Agent,
Hooks Framework), plus a `Features-Index.md` catalog page.

**Why**: Each capability page becomes a deeplinkable surface an evaluator can
share with stakeholders ("here's the Audit & SIEM page; that's what we're
proposing"). A single feature-matrix page produces no shareable URLs and
flattens distinct capabilities into a single comparison grid.

**Cost**: 10 extra pages to maintain. Each is short (~250-350 lines) and links
to its canonical architecture doc, so maintenance cost is bounded — when the
architecture doc updates, the wiki page either stays accurate (links don't
break) or gets a re-render pass at the next wiki-cadence regen.

## E76-DEC-014 — Workbench as top-level group, 6 pages

**Decision**: The "AI Vibe-Coding Workbench" gets its own top-level group of 6
pages (Overview / CLAUDE-md Anatomy / Cursor Rules / Claude Code Skills /
Forking Guide / Lessons Learned). It does NOT live under Development → 1 page.

**Why**: External readers explicitly ask about the methodology (the
README.md "AI vibe-coding workbench" section gets the most surfacing in
external conversations). Burying it 2 clicks deep under Contributing reduces
discoverability for the highest-leverage external audience: other OSS teams
wanting to adopt the same workflow.

**Rejected**: a single "Workbench.md" page — too thin to convey both the
mechanics (which `.mdc` files load when, what skills exist, why CLAUDE.md is
structured the way it is) and the forking guide (how to extract and adapt the
workbench to a different codebase).

## E76-DEC-015 — Wiki↔docs boundary: synthesize, never duplicate (Tier 0-3 same)

**Decision**: Reaffirm DEC-002's "synthesize, never duplicate" rule. Every
wiki page has a `## Canonical docs` footer linking to 1-4 main-repo docs +
3-6 adjacent wiki pages. No page restates a full doc or OpenAPI YAML; the
wiki page weaves across sources and link-down for depth.

**Why**: 95 pages duplicating canonical docs creates 95 maintenance burdens.
Synthesize-and-link keeps the wiki as a thin discoverability layer over
authoritative `docs/`.

**Rejected**: hybrid by tier (some pages self-contained, others link-out) —
inconsistent reader experience.

## E76-DEC-016 — Naming: flat `<Group>-<Page>.md` with hyphens

**Decision**: File naming convention is `<Group>-<Page>.md` with hyphens. The
sidebar provides the hierarchy via nested bullets.

**Why**: GitHub Wiki has a flat file namespace — there are no subdirectories.
Conventional prefix-grouping (`AI-Gateway-*`, `Compliance-Proxy-*`,
`Workbench-*`, `Recipe-*`, `Operations-*`) provides at-a-glance grouping when
browsing the file list directly.

## E76-DEC-017 — Phase plan; subagent dispatch pattern

**Decision**: 4 phases (P1 Foundation 28 / P2 Subsystems 37 / P3 Operate 40 /
P4 Dev+Community+Features 37). Within each phase, 4-6 parallel Sonnet
subagents work on disjoint page batches. Opus reviews each batch on return,
runs 2-round audit, merges to wiki tree, then dispatches the next phase.

**Why**: Each phase ~28-40 pages = 4-6 hours wall-clock with parallel
dispatch. Phasing prevents "all 95 pages broken at once" — issues in P1's
template usage get caught before P2 inherits them.

**Subagent prompt contract** (CLAUDE.md "Sub-agent dispatch discipline"
binding):
- Goal + non-goals in first 3 lines of every dispatch prompt
- Worktree path stated (`worktrees/E76`)
- Page list with source docs each agent must read
- Reference `_outline.md` and `_page-template.md` and `_style-guide.md`
- 2-round self-audit demanded before report-back
- Per-file output report: paths + line counts + verification grep outputs
- Cleanup candidates appended to `_cleanup-candidates.md`, NOT deleted in-band

## E76-DEC-018 — Brainstorm escalation triggers

**Decision**: Opus runs `/brainstorm` (internal architectural reasoning) when
any of the following surface from a subagent return or during the review pass:

1. IA conflict — a page topic doesn't fit cleanly in its assigned group
2. Doc/code mismatch — research surfaces a doc that disagrees with current code
3. Sidebar overload — a group exceeds 13 pages or drops below 3 after review
4. Workbench positioning — any decision on how to frame the methodology publicly
5. Non-trivial cleanup candidate (referenced anywhere outside the obvious file)

Every brainstorm outcome lands here as a new DEC entry with rejected alternatives.

## E76-DEC-019 — Cleanup pass is centralized, gated, separate-commit

**Decision**: Subagents append cleanup candidates to `_cleanup-candidates.md`
but never delete. Opus reviews each candidate via the strict verification
gate (5 checks in `_cleanup-candidates.md`). Confirmed deletions land in a
separate commit from wiki content authoring.

**Why**: Mixed-content commits caused the 2026-05-21 husky/lint-staged
incident where 62 parallel-work files got bundled into a refactor commit.
Wiki content authoring stays atomic; cleanup is its own scope.

**Source of authority**: CLAUDE.md commit hygiene; memory anchor
`[[feedback_recheck_staged_after_failed_commit]]`.

## E76-DEC-020 — Final 2-round product+architecture review (user-added 2026-05-21)

**Decision**: After P1-P4 + cleanup, run 2 rounds of strict review on every
file changed in this expansion session from (a) product angle — discoverability,
audience fit, IA shape, wiki↔docs boundary — and (b) architecture angle —
technical accuracy vs current code, faithful diagrams, correctly stated
bindings (NE fail-open, env-only secrets, pull-only config, IoT terminology).
Round 1 surfaces issues, each gets fixed. Round 2 verifies. Iterate until two
consecutive rounds clean. All fixes logged here as `DEC-REVIEW-NNN`.

**Why**: User-added requirement 2026-05-21. Closes the loop on quality —
parallel subagent authoring has surface-level risks (terminology drift,
broken links, stale claims); the final review catches them centrally.

**Source of authority**: user direction 2026-05-21
("做完以后 以最佳产品和架构 对本session的改动进行2轮严格审查和修复");
generalises CLAUDE.md's "Self-audit before done — 4 questions × 2 rounds"
binding to the full session diff.

## E76-DEC-021 — Wiki is not archaeology (binding for published wiki pages)

**Decision**: Wiki pages describe what the system IS today, forward-looking and
declarative. Banned in any published wiki page: `DEC-NNN`, "decision log",
"rejected alternatives", "supersedes", "previously", "as of `<date>`",
"in 2026-MM-DD we…", "originally", "historically", `E<n>-S<m>` SDD identifiers,
incident-date references. Release-History is the only page where past-tense
release events are appropriate (it is literally about what shipped when).

**Why**: User direction 2026-05-21 ("wiki不要提什么 Decisions等, wiki不是
考古"). Published wiki pages are read by evaluators and new users — they have
no context for IA-revision archaeology. `git log` and the
`docs/handoffs/E76-wiki-expansion/` directory carry the change history for
maintainers.

**Mechanics**: tracking files moved out of `docs/_wiki/` (GitHub Wiki
publishes every `.md` in that directory). New location:
`docs/handoffs/E76-wiki-expansion/`. Only `_Sidebar.md` and `_Footer.md`
(GitHub Wiki special-cases) remain in `docs/_wiki/` alongside the
publishable pages.

**Audit**: `grep -iE '(DEC-[0-9]|supersed|decision log|rejected alternative|as of 2026|in 2026-05-[0-9]|previously this|originally|historically|tracks historical|E[0-9]+-S[0-9])'` returns no hits across any `docs/_wiki/*.md` (excluding `_Sidebar.md` and `_Footer.md`).

## E76-DEC-022 — IA v2 final state (143 publishable pages)

**Decision**: 143 publishable wiki pages organized into 5 super-buckets / 19
groups, plus `_Sidebar.md` + `_Footer.md`. Sidebar IA matches `outline.md`.

| Super-bucket | Group | Pages |
|---|---|---|
| Landing | Home | 1 |
| Product | Overview | 5 |
| Product | Features | 11 |
| Product | Roadmap | 3 |
| Product | FAQ + Glossary | 3 |
| Getting Started | (single group) | 8 |
| Technical | Concepts | 8 |
| Technical | AI Gateway | 13 |
| Technical | Compliance Proxy | 7 |
| Technical | Desktop Agent | 8 |
| Technical | Control Plane | 10 |
| Technical | Cross-Cutting | 9 |
| Technical | Deployment | 8 |
| Technical | Operations | 8 |
| Technical | API Reference | 6 |
| Technical | Security | 8 |
| Development | Core | 8 |
| Development | Recipes | 8 |
| Development | Workbench | 6 |
| Community | (single group) | 5 |
| **Total** | | **143** |

## E76-DEC-023 — Cleanup pass dispositions (DEC-CLEANUP-001..004)

**Decision**: Six superseded old wiki pages deleted in-band (cleared by IA v2
expansion). Seventeen arch-doc archaeology candidates deferred to a separate
doc-audit PR (commit hygiene — bundling 17 arch-doc edits with 143 new wiki
pages would balloon review surface and risk lockstep churn). Two doc gaps
(missing MAINTAINERS.md, missing air-gapped runbook) filed as follow-ups.

See `cleanup-candidates.md` "Reviewed and resolved" table for the per-candidate
disposition.
