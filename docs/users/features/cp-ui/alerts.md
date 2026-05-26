# Control Plane UI — Alerts

The ALERTS sidebar section has three leaves: **Inbox**, **Rules**, and **Channels**. Rules define what to detect, the Inbox shows what fired, and Channels define where notifications are delivered. Sidebar labels and routes are defined in `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`.

Two enums run through all three pages. An alert's **severity** is `critical`, `high`, `medium`, `low`, or `info`. A **source type** — the subsystem an alert comes from — is `quota`, `proxy`, `thing`, `provider`, `auth`, or `system`.

## Inbox

**Purpose.** The alert inbox — every alert instance the Hub has raised, with row-level acknowledge and resolve.

**List page.** The list auto-refreshes every 15 seconds. Columns: state, severity, source type, rule id, target, fired-at, and actions. Filters cover state, severity, source type (multi-select), a debounced rule-id search, and a since/until datetime range. Clicking a row opens the detail drawer.

**Actions and detail.** A row shows **Ack** while it is firing and **Resolve** unless it is already resolved. The drawer shows the severity, state, and source-type chips, the message, the target, the fired-at and last-seen-at times, the duplicate count, the acknowledge and resolve metadata, a per-type details block, and a dispatch-history table (time, channel, status, error). The drawer carries its own Ack and Resolve buttons.

**Key concepts.** The alert state is `firing`, `acknowledged`, or `resolved`. The detail block is rendered by a per-rule renderer — `quota.threshold`, `quota.vk_expiring`, `proxy.hook_failure_rate`, `proxy.hook_timeout_rate`, and `proxy.high_error_rate` each have a dedicated view; every other rule falls back to a generic raw-detail renderer.

**Where the data comes from.** `alertsApi` — `list`, `detail`, `ack`, `resolve`.

## Rules

**Purpose.** Browse the Hub's built-in alert-rule catalogue, toggle each rule on or off, and tune its parameters.

**List page.** A filter toolbar (search plus enabled, severity, and source-type filters) sits above a table with columns: rule id, display name, source type, default severity, requires-acknowledgement, an enabled toggle, and an edit action. The catalogue is seeded by the Hub; rules are toggled and tuned in place.

**Edit.** The edit page sets the enabled flag, the default severity, a cooldown (seconds), the requires-acknowledgement flag, a group-id filter (fleet-wide when empty), and the rule's own parameters. A reset action restores the rule's defaults. Saving applies the change.

**Key concepts.** Each rule belongs to a source type and carries a default severity. Some rules have a dedicated parameter editor — `quota.threshold` (a list of percent thresholds, 1–100), `quota.vk_expiring` (a list of warning-day counts, each at least 1), and `proxy.hook_failure_rate` and `proxy.hook_timeout_rate` (a threshold percent 1–100, a window of at least 60 seconds, and a minimum sample count of at least 1); every other rule uses a generic editor that renders a schema-driven form with a raw-JSON fallback. The full set of rule types is the Hub-seeded catalogue (for example `proxy.high_error_rate`, `auth.login_failure_rate`, `thing.offline`, `provider.unavailable`), not a fixed list maintained in the UI.

**Where the data comes from.** `alertsApi` — `listRules`, `getRule`, `updateRule`, `resetRule`.

## Channels

**Purpose.** Define the notification destinations the Hub delivers to when a rule fires, and send a synthetic test to one.

**List page.** A table with columns: name, type, an enabled toggle, severities (chips; an empty set means all severities), source types (chips), and actions. Row actions are edit, test (fires a synthetic notification and reports the result), and delete. The page header has a create action.

**Create and edit.** The form collects a name, a type, the enabled flag, the severities this channel receives (an empty selection means all), the source types it receives, and a per-type configuration: a webhook takes a URL and custom headers; Slack takes a webhook URL, a bot token, and a channel; email takes an SMTP host, port, from, to, username, and password; PagerDuty takes a routing key. Stored secrets are masked and edited behind a change control.

**Key concepts.** The channel type is `webhook`, `slack`, `email`, or `pagerduty`. A channel filters which alerts it receives by severity and source type; leaving severities empty subscribes it to all severities.

**Where the data comes from.** `alertsApi` — `listChannels`, `getChannel`, `createChannel`, `updateChannel`, `deleteChannel`, `testChannel`.

## References

- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` — route registry and `nav: { sectionKey: 'alerts', ... }` blocks
- `packages/control-plane-ui/src/i18n/locales/en/nav.json` — sidebar labels
- `packages/control-plane-ui/src/pages/alerts/list/AlertListPage.tsx` — Inbox list
- `packages/control-plane-ui/src/pages/alerts/detail/AlertDetailDrawer.tsx` — alert detail drawer
- `packages/control-plane-ui/src/pages/alerts/detailRenderers/` — per-rule detail renderers
- `packages/control-plane-ui/src/pages/alerts/rules/AlertRulesListPage.tsx` — Rules list
- `packages/control-plane-ui/src/pages/alerts/rules/AlertRuleEditPage.tsx` — rule edit page
- `packages/control-plane-ui/src/pages/alerts/ruleEditors/` — per-rule parameter editors
- `packages/control-plane-ui/src/pages/alerts/channels/AlertChannelsListPage.tsx` — Channels list
- `packages/control-plane-ui/src/pages/alerts/channels/AlertChannelEditPage.tsx` — channel create/edit
- `packages/nexus-hub/internal/alerts/engine/rules/builtin.go` — the Hub-seeded built-in rule catalogue
- `packages/control-plane-ui/src/api/` — `alertsApi`
- `tools/db-migrate/schema.prisma` — `AlertRule`, `Alert`, `AlertChannel`, `AlertDispatch` models
