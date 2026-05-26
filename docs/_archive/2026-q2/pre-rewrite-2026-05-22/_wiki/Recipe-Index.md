# Recipe Index

*Audience: contributors adding new capabilities to Nexus Gateway.*

Recipes are step-by-step how-to guides for the most common contributor tasks. Each recipe names the files to touch, the verification commands to run, and the cost of skipping any step. The underlying "why" lives in the architecture docs each recipe links to — the recipe itself is the "how".

---

## When to use which recipe

| You want to… | Recipe |
|---|---|
| Support a new LLM vendor or wire format | [Recipe Adding A Provider Adapter](Recipe-Adding-A-Provider-Adapter) |
| Add a new compliance enforcement rule | [Recipe Adding A Hook](Recipe-Adding-A-Hook) |
| Register a new managed service type with Hub | [Recipe Adding A Thing Type](Recipe-Adding-A-Thing-Type) |
| Add a page or section to the Control Plane admin UI | [Recipe Adding A CP UI Section](Recipe-Adding-A-CP-UI-Section) |
| Document an operational procedure for the ops team | [Recipe Adding A Runbook](Recipe-Adding-A-Runbook) |
| Record a new admin or data-plane audit event | [Recipe Adding An Audit Event](Recipe-Adding-An-Audit-Event) |
| Introduce a new admin-API verb or resource | [Recipe Adding An IAM Action](Recipe-Adding-An-IAM-Action) |

---

## Recipe Adding A Provider Adapter

Adding a new LLM provider or wire format into the AI Gateway. Covers the codec (canonical ↔ wire translation), streaming session, error normalizer, builtins registration, seed rows, and the mandatory 9-step smoke test. The `add-provider-adapter` skill (`Skill('add-provider-adapter')` in Claude Code) automates the checklist. The companion `adapter-conformance-check` skill audits an existing adapter against the 7 §3a binding rules.

**Use when**: a new LLM vendor needs to receive traffic from Nexus, or when a new wire format (binary protocol, custom SSE variant) needs a detector.

See [Recipe Adding A Provider Adapter](Recipe-Adding-A-Provider-Adapter).

---

## Recipe Adding A Hook

Adding a new enforcement stage to the three-path compliance pipeline (AI Gateway, Compliance Proxy, Desktop Agent). Covers the `Hook` interface, `HookConfig.onMatch` schema, `applicableIngress` filtering, metrics wiring, and staged rollout (`flag` → `block-soft` → `block-hard`).

**Use when**: a new policy check (keyword filter, content classifier, size limit, custom webhook) needs to fire on traffic before or after the upstream call.

See [Recipe Adding A Hook](Recipe-Adding-A-Hook).

---

## Recipe Adding A Thing Type

Adding a new managed service type to the Hub-centric registry. Covers the extension table, the shadow key categories (Cat A/B/C), the three-path consistency audit (configreconcile + seed + migration), and the `OnConfigChanged` callback wiring. The `add-shadow-key` skill (`Skill('add-shadow-key')`) walks the three-path audit step by step.

**Use when**: a new service needs to join the Hub registry, receive config via the shadow, and appear on the CP Infrastructure → Nodes page.

See [Recipe Adding A Thing Type](Recipe-Adding-A-Thing-Type).

---

## Recipe Adding A CP UI Section

Adding a new page, sidebar item, or route to the Control Plane admin UI. Covers route registration, IAM action wiring (`iamMW` + `allowedActions` symmetry), i18n keys in all three locales, design-token compliance, `useApi` queryKey shape, and the mandatory positive + negative IAM test. The `add-cp-ui-section` skill (`Skill('add-cp-ui-section')`) runs all 8 steps with verification commands.

**Use when**: a new admin capability needs a UI surface in the Control Plane.

See [Recipe Adding A CP UI Section](Recipe-Adding-A-CP-UI-Section).

---

## Recipe Adding A Runbook

Adding an operator runbook to `docs/operators/ops/runbooks/`. Covers the expected runbook shape (state model, common actions, failure modes, verification commands), the doc-lockstep entry that keeps the runbook synchronized with code, and cross-linking to the canonical architecture doc.

**Use when**: a new operational procedure (incident response, migration, rotation, rollout) is well-understood enough to document for on-call use.

See [Recipe Adding A Runbook](Recipe-Adding-A-Runbook).

---

## Recipe Adding An Audit Event

Adding a new event type to the audit pipeline — either a `traffic_event` column/field for data-plane events or an `AdminAuditLog` entry for admin mutations. Covers the `AuditEvent` struct, the emitter `Writer`, the Hub ingest path, Postgres schema changes, and the CP UI surface.

**Use when**: a new traffic signal, admin action, or lifecycle event needs to appear in the audit log or analytics dashboard.

See [Recipe Adding An Audit Event](Recipe-Adding-An-Audit-Event).

---

## Recipe Adding An IAM Action

Adding a new IAM action (resource + verb) to the action catalog, wiring it into the backend middleware and the UI `allowedActions`, and updating the managed policy seed so existing roles still work. The `iam-impact-review` skill (`Skill('iam-impact-review')`) runs the 5-step audit automatically.

**Use when**: a new admin API endpoint needs access control, or when an existing resource needs a new verb (e.g., `toggle`, `export`, `simulate`).

See [Recipe Adding An IAM Action](Recipe-Adding-An-IAM-Action).

---

## Canonical docs

- [`CLAUDE.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CLAUDE.md) — Mandatory development workflow and binding rules for every contributor
- [`docs/developers/architecture/README.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/README.md) — "What to read before editing X" trigger table

**Adjacent wiki pages**: [Contributing](Contributing) · [Dev SDD Pipeline](Dev-SDD-Pipeline) · [Dev Code Doc Lockstep](Dev-Code-Doc-Lockstep) · [Dev Testing Coverage](Dev-Testing-Coverage) · [Workbench Claude Code Skills](Workbench-Claude-Code-Skills)
