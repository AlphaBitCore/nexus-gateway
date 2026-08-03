# Control Plane UI — Infrastructure: operations and diagnostics

This group of INFRASTRUCTURE leaves is where an operator reaches for emergency control, fleet diagnostics, and Agent rollout. It covers **Kill Switch**, **Recent Errors**, **Crash Reports**, **Agent Diag Mode**, **Proxy Rollout**, and **Agent Setup**. Sidebar labels and routes are defined in `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` and `packages/control-plane-ui/src/i18n/locales/en/nav.json`.

## Kill Switch

**Purpose.** A fleet-wide emergency stop that halts TLS interception across every Compliance Proxy and Agent at once.

**What you see.** A toggle control with an engaged / normal-operation badge; a per-type status breakdown — Compliance Proxies (N of M active), Agents (N of M active), and who last changed it, when, and the reason; and a merged history table of every kill-switch toggle (timestamp, node type, action, actor, version).

**Controls.** A single engage/disengage button behind a confirmation dialog. The fleet reads engaged when any Compliance Proxy or Agent has it engaged, so the badge never understates an in-flight emergency.

**Key concepts.** Engaged means a node's applied kill-switch configuration is on — it has stopped TLS interception and is passing traffic through uninspected. The toggle fans out to both compliance-proxy and Agent configuration in one call, and is meant for emergencies: a bad provider rollout, a hook regression blocking legitimate traffic, or a proxy or Agent fault.

**Where the data comes from.** The toggle is `complianceApi.setKillSwitch` → `POST /api/admin/compliance/killswitch`, gated on `kill-switch.toggle`, which stamps a dedicated kill-switch audit event. The per-type status is derived from the nodes list (`node.read`) and the toggle history from the configuration change history (`settings.read`).

## Recent Errors

**Purpose.** A triage view of the diagnostic events emitted across the fleet, grouped so an operator can spot and quiet noisy or newly appeared issues quickly.

**What you see.** A hero strip of four tiles — errors in the last hour with a trend against the hour before, the active-issue count, the top offender source with its share, and the newest issue — above an aggregate sparkline. Below it, an issue list with one row per distinct message group: a severity badge, source, affected-node count, total occurrences, first and last seen, an inline sparkline, a "new" badge for issues first seen within the hour, and a faded style for silenced rows. A filter panel — collapsed by default — covers time range, node type, event type, a show-silenced toggle, and search.

**Controls.** Per issue: view details, silence (1 hour, 24 hours, or permanent), unsilence, and — for an Agent issue — enable diagnostic mode on that node. A detail dialog shows the full event metadata and recent occurrences; a "manage silences" popup lists the active silences and lifts them.

**Key concepts.** Severity is `fatal`, `error`, `warn`, or `info`; `fatal` and `error` are the high-severity levels the active-issue count and hero tiles track. A silence is keyed on the message and level and collapses that group's silenced flag. Time ranges are 1 hour, 24 hours, 7 days, and 30 days.

**Where the data comes from.** `diagEventsApi` (groups, list) and `diagSilencesApi` (list, create, remove) — reads gated on `observability.read`, and creating or lifting a silence on `observability.write`. Enabling diagnostic mode from a detail dialog is `diagModeApi.enable`, gated on `diagnostic-mode.update`.

**Traffic errors tab.** For admins who also hold `traffic-log.read`, the page adds a two-tab data-source switch: **System faults** (the diagnostic view above) and **Traffic errors** — an aggregated governance view over failed gateway/proxy/agent requests. Each row is one error class, grouped by error code × status class (4xx/5xx) × serving provider × model, with an occurrence count, distinct affected end-users, first/last seen, and a trend sparkline. Every class carries a computed **attribution** badge naming the first suspect: **Ours** (gateway routing, format translation, or provider configuration — the classes the operator can fix), **Client** (the caller's own key, quota, disconnect, or malformed request), or **Upstream** (provider-side failures passed through). Attribution is a filter and sort priority, never an exclusion — one-click chips narrow to a single bucket (for example "only our faults") and client errors stay visible. A **View in Traffic** action deep-links into the Traffic page pre-filtered to exactly that class — error code (unclassified classes select rows with no code at all), serving provider, model, status class, and the aggregation time window — on the All source tab, since a class can span gateway, proxy, and agent traffic; the drill-down's row count matches the class count. Unclassified rows are shown too: a 5xx with no code is attributed upstream, a 4xx with no code to the client. Data comes from `systemApi.listTrafficErrorGroups`, gated on `traffic-log.read`; admins without that action simply see the diagnostic view with no tab strip. The diagnostic (system-fault) tab keeps its role unchanged.

The aggregation is served rollup-first so the view stays responsive during error storms: sealed history comes from pre-aggregated 5-minute/hourly/daily buckets maintained by the platform's rollup pipeline, and only the last few unsealed minutes are scanned from raw traffic events. The requested window is snapped outward to whole bucket boundaries so counts always cover complete buckets. Three honest-degradation details follow from that. The affected-end-users column shows an exact distinct count for classes up to an affordability bound and a dash ("—") for larger classes — or when data retention has already purged the class's raw rows — rather than an approximation. If a single 5-minute window ever holds more than 500 distinct error classes (a spray of unique model strings can do this), the excess folds into one "(other classes)" row per status class whose count keeps the total exact — that row carries a neutral "Mixed" attribution (it disappears under any attribution filter) and supports no drill-down, since no individual traffic row carries its synthetic code. And whenever the rollup pipeline cannot honestly cover the request — jobs never ran, the pre-aggregated buckets no longer retain a window that old, or the pipeline is stalled — the view transparently serves the raw scan instead.

## Crash Reports

**Purpose.** Group Agent crashes into version × OS cohorts so a regression that only hits one build or one platform stands out from the noise.

**What you see.** A time-range filter (24 hours, 7 days, 30 days) over a cohort table: agent version, OS, OS version, crash count, affected nodes, first seen, last seen. Expanding a cohort lists its individual crash events; clicking an event opens a dialog with the message, the stack trace, and the event attributes.

**Where the data comes from.** `diagEventsApi.crashCohorts` for the cohort table and `diagEventsApi.list` (restricted to the fatal level) for the per-cohort drilldown, both gated on `observability.read`.

## Agent Diag Mode

**Purpose.** Turn on verbose Agent diagnostics for a bounded window — in bulk — to capture detail while reproducing an issue, then have it switch itself off when the window expires.

**What you see.** An active-windows table of the Agents currently in diagnostic mode (node, started, time remaining, who set it, reason), refreshing every 10 seconds, each row with a disable action behind a confirm. Below it, a bulk-enable form.

**Controls.** The bulk form filters by agent version, OS, or an explicit list of node ids — an id list overrides the other filters — with a window preset (1, 4, 12, or 24 hours; capped at 24) and a required reason. A "resolve preview" step shows the exact match count before you commit, capped at 500 agents. The outcome is reported per agent, with partial-success and all-failed panels that list the agents that did not take.

**Where the data comes from.** `diagModeApi` — the active-windows list gated on `diagnostic-mode.read`, and enable, disable, and bulk-enable on `diagnostic-mode.update`. The preview resolves the candidate agents from the nodes list (`node.read`) and applies the filter client-side.

## Proxy Rollout

**Purpose.** A launcher for rolling the Compliance Proxy out to a node — it lists the Compliance Proxy nodes and sends you into a node's setup flow.

**What you see.** One card per Compliance Proxy node with its name and status. A "configure setup" button — disabled while the node is offline — opens that node's setup flow: the four-step CA certificate / MDM profile / PAC file / onboarding helper described in the [nodes and configuration document](infrastructure-nodes.md).

**Where the data comes from.** `hubApi.listNodes` filtered to the compliance-proxy type (`node.read`).

## Agent Setup

**Purpose.** The self-service install / enroll / verify guide for putting the Agent on a workstation or server, with a troubleshooting FAQ.

**What you see.** A platform-tabbed install card (macOS, Windows, Linux) with four steps — download, install, enroll, verify — and a troubleshooting card with a searchable, category-filtered FAQ.

**Controls.** Download offers a per-platform build: a signed macOS `.pkg`, a Windows MSI that registers the Agent service and traffic-capture driver and sets the system CA-trust environment variables, or a Linux binary. Enroll is single sign-on — a headless server runs `nexus-agent enroll-sso`, which prints a URL to open on a workstation. The verify step shows a live panel of the devices bound to the current admin, refreshing every five seconds, with an online / waiting / offline badge per device. The FAQ filters by search text and six categories: trust, coverage, network, performance, logs, and lifecycle.

**Where the data comes from.** The download host comes from the Control Plane's published URL (`serviceUrlsApi.publicURLs`, `settings.read`); the verify panel reads the caller's own devices (`devicesApi.listMine`, scoped to the signed-in user); the installers are served as static downloads.

## References

- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` — route registry and `nav: { sectionKey: 'infrastructure', ... }` blocks
- `packages/control-plane-ui/src/i18n/locales/en/nav.json` — sidebar labels
- `packages/control-plane-ui/src/pages/infrastructure/kill-switch/InfraKillSwitchPage.tsx` — Kill Switch toggle, status, history
- `packages/control-plane-ui/src/pages/infrastructure/recent-errors/InfraRecentErrorsPage.tsx` — Recent Errors triage, silences
- `packages/control-plane-ui/src/pages/infrastructure/crash-reports/InfraCrashReportsPage.tsx` — Crash Reports cohorts
- `packages/control-plane-ui/src/pages/infrastructure/diag-mode/InfraDiagModePage.tsx` — Agent Diag Mode active windows + bulk enable
- `packages/control-plane-ui/src/pages/proxy/setup/ProxySetupPage.tsx` — Proxy Rollout launcher
- `packages/control-plane-ui/src/pages/infrastructure/agent-setup/InfraAgentSetupPage.tsx` — Agent Setup install / enroll / verify + FAQ
- `packages/control-plane-ui/src/api/services/infrastructure/diag/diagevents.ts` — `diagEventsApi`, `diagSilencesApi`
- `packages/control-plane-ui/src/api/services/infrastructure/diag/diagmode.ts` — `diagModeApi`
- `packages/control-plane-ui/src/api/services/compliance/compliance.ts` — `complianceApi.setKillSwitch`
- `packages/control-plane/internal/governance/killswitch/handler/handler.go` — the kill-switch toggle route and its fan-out
- `packages/control-plane/internal/infrastructure/infra/diagevents.go` — diag-events and crash-cohorts routes and IAM gates
- `packages/control-plane/internal/infrastructure/infra/diag_silences.go` — diag-silences routes and IAM gates
- `packages/control-plane/internal/infrastructure/infra/diagmode.go` — diagnostic-mode routes and IAM gates
