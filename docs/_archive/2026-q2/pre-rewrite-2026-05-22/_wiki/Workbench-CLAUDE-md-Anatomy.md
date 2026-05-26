# Workbench CLAUDE md Anatomy

*Audience: engineers adapting the workbench to their own codebase, or contributors who want to understand why a specific binding exists.*

`CLAUDE.md` is the single canonical rule file that governs every AI agent session in this repo. It lives at the repo root, loads automatically into every Claude Code and Cursor context, and carries ~250 lines of dense English where every sentence is binding unless explicitly waived. This page walks through each section, explains what the binding does, and identifies the class of failure it prevents.

---

## Mandatory rules

The mandatory rules section is the most load-bearing part of the file. Each rule is a hard contract; waivers require explicit user approval in the chat session, not silent assumption.

### English only

All project artifacts committed to the repo must be English: `CLAUDE.md`, `.cursor/rules/`, `docs/**`, source comments, user-visible UI copy, commit messages, READMEs. Chat language may match the user; files in the repo stay English. The binding matters because AI agents will produce whatever language seems contextually appropriate if not constrained — a Chinese-language architecture doc or commit message creates a two-tier contributor experience.

### Workflow discipline

Six numbered sub-rules enforce the development discipline:

**Plan first.** Before edits, research, or implementation the agent produces a written plan — approach, scope, risks, file touch list. This prevents the failure mode of the agent jumping to code and solving the wrong problem or overreaching the agreed scope.

**Todo list stays live.** Every user request becomes a todo immediately. Statuses update as work progresses. New requests during a multi-message session do NOT interrupt in-flight work — they queue. This prevents the pattern of an agent abandoning half-finished work to chase the newest thing.

**Complex tasks: Plan + Todo non-waivable.** A task is complex if it touches more than 2 files, crosses service boundaries, modifies an API contract, changes a data model or migration, or touches a high-blast-radius surface (Network Extension provider code, admin endpoint registration, IAM policies, token-field stamping, kill-switch, emergency passthrough, credential encryption). Complex tasks cannot proceed without a plan and live todo list, regardless of how confident the agent seems.

**Goal-anchored execution.** Before non-trivial work the agent writes "Goal: …" at the top of the plan and echoes it in chat. When mid-stream constraints arrive, the goal is re-stated rather than silently absorbed. This prevents the pattern of the agent completing a technically correct task that is not what the human asked for.

**2-round self-audit before "done".** Four questions run twice: (Q1) every todo completed? (Q2) no TODO/FIXME/stub/unimplemented strings in production code? (Q3) every changed code path exercised by a real test or explicitly acknowledged as untested? (Q4) no "fix later" claims? Round 1 surfaces issues; Round 2 verifies the fixes. Two consecutive clean rounds are required. This is the most effective single gate against the "I'll just add a TODO for now" anti-pattern.

**No auto-commit.** When work is done, the agent asks the user about committing. Never commits unsolicited. This keeps the human in the loop for the final review.

### Adversarial product review + less-is-more

Two halves of the same discipline. Before implementing, the agent steel-mans the proposal and then attacks it: is the user-facing value clear? How often will users hit this path? Could a sensible default solve it without a new knob? Does the cost exceed the value? Counter-arguments surface in chat before any code is written.

The less-is-more half enforces spring-style defaults: every new configuration knob needs a sensible default, new admin UI surfaces need a documented user journey, and the default answer to "should I add a new tab/page/field?" is no. The canonical pattern is the Anthropic adapter filling `max_tokens` from the model capability when the caller omits it — adapter auto-fill, zero admin config required.

### Worktree per session

Multiple agent sessions running concurrently each get their own `git worktree` under `./worktrees/`. The working tree, index, and `.git/index.lock` are private to that session, so `git stash`, `git add -A`, and `git restore` are all safe inside a worktree. Two sessions editing the same `packages/shared/<subpkg>/` file simultaneously will conflict at merge — the binding requires coordinating at the subpackage level.

### Handoff at context-full

When a session accumulates enough state — auto-compact threshold near, multi-phase program closing, more than ~50 tool-call turns, or after a major push — the agent offers to write a handoff document capturing the program goal, architecture facts the next session needs, work completed, and next steps. Auto-compact summarizes but loses fidelity; an explicit on-disk handoff file is more reliable.

### Sub-agent dispatch discipline

Sub-agents (spawned via the Agent tool) have zero session context. The binding defines what is safe to delegate: mechanical multi-file work, parallel-safe code searches, bounded audits. It defines what must NOT be delegated: understanding a new problem, decisions that change scope, git commits or pushes. Every dispatched prompt must state goal and non-goals, demand a 2-round self-audit, and request a per-file list of touched paths.

### Real implementation only

Production code is fully implemented. No TODO/FIXME/XXX, no `not implemented` throws, no empty handlers (unless the spec defines them), no fake returns, no stub modules, no "demo" logic. Mocks and test doubles belong only in test code. If scope is too large, the plan narrows with the user — placeholder code does not merge.

### Development-phase policy: no backward compatibility

Pre-GA, no installed users. Treat all refactoring as greenfield: delete obsolete code outright when replacing it, no phased compatibility rollouts, no `@deprecated` markers kept alive, no data-migration code for dev-only records, no feature flags for rollback (rollback is `git revert`). "Phase" in plans means order of work, not compatibility layer.

### Build-agent skill mandate

macOS Agent builds must go through `Skill('build-agent')`. Improvising `codesign`, `pkgbuild`, `productbuild`, `xcrun notarytool`, or `wails build` calls directly is forbidden. The skill is the single source of truth for signing, provisioning, notarization, launch-constraints, and the install/uninstall sequence.

### IAM impact review

Any PR that adds, moves, renames, or removes an admin API endpoint, sidebar nav item, or admin route path must run the 5-step IAM audit (`.cursor/rules/iam-impact-review.mdc` or `Skill('iam-impact-review')`) in the same PR. Drift between UI `allowedActions` and handler `iamMW(...)` produces silent 403s — the user sees the menu item, clicks it, and gets a 403 with no explanation.

### macOS NE fail-open

`NETransparentProxyProvider` is in the host's outbound packet path. Any hang, panic, or silently-claimed-but-not-relayed flow takes down the entire Mac's network including DNS, DHCP, mDNS, NTP, Apple Push, and VPNs. Recovery requires manual `launchctl unload` and plist deletion. Five rules: synchronous `handleNewFlow` decisions, fail-open timeouts on every async callback, no hardcoded enforcement lists in Swift, `isLikelyXyz = true` patterns banned, system DNS/DHCP/Push processes must never have their UDP closed.

### Adapter format translation

All provider adapters follow `provider-adapter-architecture.md` §3a Rules 1-8: canonical format is OpenAI shape, non-OpenAI adapters own their full bidirectional translation, per-model wire quirks stay in the adapter not in the generic dispatcher, extension fields ride inside `nexus.ext.<provider>.<key>`, cross-format callers canonicalize before the codec, streaming and non-streaming parity is required, every prefix-list rule needs a comment citing the observed 400. Run `/adapter-conformance-check` before completion.

### Unit test coverage ≥95%

Every Go package in `packages/**` must hit at least 95% statement coverage, or be listed in `scripts/.coverage-allowlist` with a category (A: cmd entry point, B: test helper, C: DB-bound, D: OS-bound, E: network-infra-bound, F: integration-only) and a one-line rationale. Adding to the allowlist requires explicit user approval. Tests must assert observable business behavior and named failure modes — coverage padding defeats the rule.

### Test/skill env files

Tests and `prod-*` skills read config from `tests/.env.<target>` where target is `local`, `dev`, or `prod`. The loader is fail-closed: missing target on non-TTY runs is an error, hostname allowlist violations are an error. Adding a new variable requires updating both `tests/.env.<target>` and documenting it in `local-dev-debugging.md`.

### Secrets are env-only

No secret field may appear in any YAML committed to the repo. Auth tokens, HMAC keys, credential-encryption keys, internal-service tokens, and DB passwords ride environment variables documented in `.env.example`. Cross-service shared secrets are tagged `[MUST MATCH]` in `.env.example`; drift between consumers is the most common source of inter-service 403s.

### Code/doc lockstep

When a PR touches code covered by an architecture doc, feature doc, OpenAPI spec, runbook, or SDD story, every matching doc must update in the same commit. The lockstep map (`scripts/doc-lockstep.config.mjs`) is the authoritative code-glob → doc-files registry. `scripts/check-doc-lockstep.mjs` enforces it on every PR.

### Configuration changes

Any PR that adds, removes, or renames a YAML field, env variable, `thing_config_template` configKey, `system_metadata` key, or publisher/receiver wiring must conform to the 4-layer config model. New keys update the §7 per-key catalog AND `packages/shared/schemas/configkey/` (constants, `ValidByThingType`, `TypedRegistry`) in the same PR.

### AI Gateway smoke mandatory

Any edit touching AI Gateway code, `traffic_event` schema, canonical/cost/cache/normalize code, or provider adapters must run an AI Gateway smoke before claiming the work is done. What the smoke catches that unit tests cannot: cross-ingress asymmetry (one ingress passes, another silently drops a field), traffic_event cost/token accuracy, cache classification bugs, and codec parity regressions.

---

## Pre-edit reading (3-doc rule)

Before any code change, three documents must be read:

1. **Architecture doc(s)** — found via the trigger map at `docs/developers/architecture/README.md`.
2. **Feature doc(s)** — for user-visible surface changes, the matching doc in `docs/users/features/cp-ui/`, `docs/users/features/agent-ui/`, or `docs/users/features/flows/`.
3. **Conventions** — `docs/developers/workflow/conventions.md` for naming, idioms, commit style, PR review checklist.

Skipping any of the three requires an explicit waiver. The trigger table lives outside `CLAUDE.md` by design — it grows row by row, and CI enforces it separately via `npm run check:arch-doc-triggers`.

---

## Mandatory development workflow

The SDD pipeline in sequence:

```
Plan + Todo  →  Architecture  →  Requirements  →  SDD  →  OpenAPI  →  Code  →  Unit Tests  →  Verify  →  Ask-about-commit
```

No step skipped. "Phase" in plans means order of work, not a compatibility rollout layer. The full pipeline applies to every non-trivial change; one-liners still need a plan and a todo, just shorter ones.

---

## Current state, project structure, tech stack, and conventions

The bottom of `CLAUDE.md` carries stable facts about the repo: the Hub-centric pull-only config sync model, Redis as cache-only (no pub/sub), the kill-switch managed via Hub shadow, the Control Plane UI's Infrastructure section, the Go module and import conventions, the CI-enforced TypeScript bindings (i18n mandatory, design-token strict, `useApi` queryKey domain prefix), and the IoT terminology boundary (internal "Thing/Shadow/desired/reported/drift" versus user-facing "node/config sync/target config/applied config/out of sync"). These facts are here so the agent doesn't rediscover them from code reading, and they update in lockstep with the code they describe.

---

## Canonical docs

- [`CLAUDE.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CLAUDE.md) — the full binding rule file; this page is a reading guide, not a replacement
- [`ai-workflow.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/ai-workflow.md) — the four artifact layers and the SDD pipeline in detail

**Adjacent wiki pages**: [Workbench Overview](Workbench-Overview) · [Workbench Cursor Rules](Workbench-Cursor-Rules) · [Workbench Claude Code Skills](Workbench-Claude-Code-Skills) · [Workbench Forking Guide](Workbench-Forking-Guide) · [Workbench Lessons Learned](Workbench-Lessons-Learned)
