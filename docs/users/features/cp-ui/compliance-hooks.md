# Control Plane UI — Compliance: hooks and policies

This document covers the hooks-and-policy part of the COMPLIANCE sidebar section: **Overview**, **Hooks & Policies**, **Rule Packs**, and **Exemptions**. The network-interception part (Interception Domains, AI Guard Backend, Streaming Compliance, Payload Capture) is in [compliance-network.md](./compliance-network.md), and the records part (Operation Logs, Data Subject Requests, Compliance Report) is in [compliance-records.md](./compliance-records.md). Sidebar labels and routes are defined in `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`.

## Overview

**Purpose.** A global enforcement-health dashboard across the three intercept layers for a selected time window.

**What you see.** A time-range filter bar (presets 24h / 7d / 30d / custom, plus Refresh and Export CSV); four KPI cards — total requests, blocked, TLS coverage, hook errors; an **Enforcement Trinity** row of three cards (AI Gateway, Network Proxy, Agent), each showing events, blocked, and TLS coverage with decision badges; a **Hook Decision Health** panel (total / allow / deny / error / unknown counts, p50 / p95 / p99 latency, and top deny reason codes); and a **Top Blocked** table tabbed by target host, reason code, and source IP.

**Controls.** The page is read-only; the only action is the CSV export, whose window is capped at 366 days.

**Key concepts.** Each intercept layer reports a per-request decision — `APPROVE`, `MODIFY`, `REJECT_HARD`, `BLOCK_SOFT`, or `ABSTAIN`. Hook-level decisions roll up as allow, deny, error, or unknown.

**Where the data comes from.** `complianceApi.getOverview`; the export uses `complianceApi.buildOverviewExportUrl`.

## Hooks & Policies

**Purpose.** Configure which hooks run on traffic, at which stage, in what order, and how each one is set up.

**List page.** Pipeline tabs (All / Request / Response) sit above a table with columns: name, category, stage, status (an enable/disable switch), priority, and actions (view, delete). A create button opens the form; the list has a search box and an enabled filter. A collapsible **Hook Pipeline** panel shows the execution chain and reorders hooks with up/down controls.

**Create and detail.** The form collects name, `type` (built-in, webhook, or script), `stage` (request or response), a category override, `priority`, `timeoutMs`, `failBehavior` (fail-open or fail-closed), the enabled flag, and `applicableIngress` (ALL, AI_GATEWAY, COMPLIANCE_PROXY, or AGENT). An `implementationId` select is populated from the hook registry and filtered by stage and type — a built-in hook picks one of the eleven implementations (`keyword-filter`, `pii-detector`, `content-safety`, `rate-limiter`, `request-size-validator`, `ip-access-filter`, `data-residency`, `rulepack-engine`, `noop`, `webhook-forward`, `quality-checker`); a webhook hook forwards to an external endpoint; a script hook runs an inline script. A JSON-schema-driven config panel (or a manual JSON editor) sets the implementation's parameters. The detail page has Overview, Configuration, Pipeline, Rule Packs (shown for `content-safety`, `keyword-filter`, `pii-detector`, and `rulepack-engine`), and Test tabs.

**Key concepts.** A content-touching hook carries an `onMatch` policy with a single **action** selector that decides what happens when the hook matches — `approve`, `redact`, or `block`:

- **Approve** — let the traffic through unchanged and keep the original in the audit log.
- **Redact** — mask the matched content (e.g. `[REDACTED_SSN]`); the *same* masked copy is what reaches the provider, what the caller gets back, and what the audit log keeps. There is no way to forward the original while storing a masked copy, or vice versa — redaction is all-or-nothing across the live traffic and the stored copy.
- **Block** — reject the request and store a masked copy so a reviewer can later see *what kind* of content tripped the policy (the rule that fired) without the raw sensitive value.

What a blocked caller sees depends on where the block happened and which layer enforced it. The AI Gateway and the Network Proxy return an explanatory error; the Agent's on-device interception returns a bare refusal. The explanation is also stage-aware: a block on the **request** may name the matched rule and even the flagged value (it is the caller's own data), but a block on the **response** names only the rule or category — it never echoes the upstream content the block exists to contain. When a redaction cannot be applied precisely (the matched content cannot be located in the body Nexus can rewrite), the audit store fails safe and keeps nothing rather than storing the unmasked content. Categories are compliance, traffic_control, quality, observability, or custom.

**`failBehavior` and unbuildable hooks.** `failBehavior` governs two distinct moments. At *run* time, a hook that errors or times out resolves per its setting: fail-open → approve, fail-closed → reject. At *build* time, a hook that cannot be constructed at all (a typo'd `implementationId`, a broken config) is normally skipped with a warning — except a **fail-closed** hook on the AI Gateway or the Compliance Proxy, where skipping would silently disable a mandatory control: there the request is **refused** instead (AI Gateway 500, or in-band stream termination when the failure surfaces mid-SSE; Compliance Proxy 403 on CONNECT / 502 on request and response / aborted SSE stream — the request/response/SSE refusals each leave an audit row with bump status `BUMP_PIPELINE_BUILD_FAILED`; the CONNECT refusal predates any transaction and surfaces via metric + log only). If proxy traffic starts failing right after a hook edit, check that hook's configuration or disable it here — see the operator runbook [compliance-pipeline-build-failures](../../../operators/ops/runbooks/compliance-pipeline-build-failures.md). The agent's on-device proxy never refuses on a build error (host networking must keep working); it skips the hook and logs.

**Where the data comes from.** `hookApi` — `list`, `get`, `create`, `update`, `delete`, `test`, `getImplementations`, `getExecutionChain`, `reorder`.

## Rule Packs

**Purpose.** Author or import versioned rule packs and bind them to the hooks that consume rules.

**List page.** A maintainer filter sits above a table with columns: name (a link), version, maintainer, created, and a delete action. The toolbar offers Import YAML and Create.

**Create, import, bind, and override.** Creation collects name, version, maintainer, an optional description, and the rules — in a form mode or a JSON mode; each rule has a rule id, category, severity, pattern, and optional flags, labels, and description. Import pastes a YAML pack, previews it (surfacing warnings and errors), then imports. Binding picks a pack family, pins a version, and installs it onto a rule-pack-consuming hook. The overrides panel sets, per install, a per-rule disabled flag and a severity override against that install's effective rule set.

**Key concepts.** A rule's severity is `hard`, `soft`, or `info`. A pack is versioned, and each install pins a specific version; overrides are scoped to a single install rather than the source pack.

**Where the data comes from.** `rulePacksApi` — `list`, `create`, `get`, `update`, `delete`, `import`, `preview`, `install`, `uninstall`, `listInstallsForHook`, `patchInstall`, `effectiveRules`, `upsertOverrides`.

## Exemptions

**Purpose.** Grant a time-bounded source/target carve-out that bypasses the compliance hooks for matching traffic. The traffic is still TLS-bumped — an exemption skips hook evaluation, not interception.

**List page.** A single list unifies active grants and pending requests, with a status filter (All, Effective, Oncoming, Pending, Expired) and columns: id, status (with a disabled badge for inactive grants), source IP, target host, effective-from, expiry (relative), reason, requested-by, approved-by, and created. Row actions branch by kind — grants offer enable / disable and delete (deletable only before activation); pending requests offer approve / reject. A create button opens the form.

**Create and detail.** Creation collects a source IP or CIDR, a target host, a duration (chosen from preset minutes), a reason (4 to 500 characters), and a "submit as pending" checkbox that creates a pending request for approval rather than an immediate grant. The detail page is a read-only key-value view with the same lifecycle actions; rejecting a request requires a note.

**Key concepts.** A list row's `kind` is either grant or pending. A grant's status is `effective`, `oncoming`, `pending`, or `expired`, and an inactive grant carries a disabled flag. The activation time gates whether a grant can still be deleted.

**Where the data comes from.** `complianceApi` — `listExemptions`, `getExemption`, `createExemptionGrant`, `createPendingExemptionRequest`, `deleteExemptionGrant`, `approveExemption`, `rejectExemption`, `patchExemptionGrant`.

## References

- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` — route registry and `nav: { sectionKey: 'compliance', ... }` blocks
- `packages/control-plane-ui/src/i18n/locales/en/nav.json` — sidebar labels
- `packages/control-plane-ui/src/pages/compliance/dashboard/ComplianceDashboardPage.tsx` — Compliance Overview
- `packages/control-plane-ui/src/pages/compliance/hooks/list/HookList.tsx` — Hooks list and pipeline panel
- `packages/control-plane-ui/src/pages/compliance/hooks/form/` — hook create/edit form (type, stage, implementation, onMatch)
- `packages/control-plane-ui/src/pages/compliance/hooks/detail/` — hook detail tabs
- `packages/control-plane-ui/src/pages/compliance/hooks/panels/` — per-implementation config panels
- `packages/control-plane-ui/src/pages/compliance/rule-packs/` — rule pack list, form, import, bind, overrides, detail
- `packages/control-plane-ui/src/pages/compliance/exemptions/` — Exemptions list and detail
- `packages/shared/policy/hooks/builtins/` — the eleven built-in hook implementations
- `packages/control-plane-ui/src/api/` — `complianceApi`, `hookApi`, `rulePacksApi`
- `tools/db-migrate/schema/compliance.prisma` — `HookConfig`, `RulePack`, `Rule`, `RulePackInstall`, `RuleOverride`, `ComplianceExemptionGrant` models
