# Control Plane UI — Infrastructure: observability and audit

This group of INFRASTRUCTURE leaves controls platform telemetry, how long diagnostic and metric data is kept, forwarding events to an external SIEM, and the audit pipeline's dead-letter queue. It covers **Observability**, **Observability Retention**, **SIEM**, and the **Dead-letter Queue**. Sidebar labels and routes are defined in `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` and `packages/control-plane-ui/src/i18n/locales/en/nav.json`.

## Observability

**Purpose.** Configure OpenTelemetry tracing for the platform.

**What you see.** A switch to enable OpenTelemetry, the read-only OTel endpoint and service name the platform reports under, a trace sampling-rate input (0 to 1), and a trace-viewer URL.

**Controls.** Turn OpenTelemetry on or off, set the sampling rate, and point the trace-viewer link at an external dashboard (for example a Grafana traces view). Save applies the change.

**Where the data comes from.** `systemApi.getObservabilityConfig` / `updateObservabilityConfig` → `/api/admin/settings/observability`, gated on `observability.read` to view and `observability.write` to save.

## Observability Retention

**Purpose.** Set how many days each tier of operational metrics and diagnostic events is kept.

**What you see.** Three groups of day-count inputs, each input showing its allowed range as a hint: Runtime metrics (raw, 1-hour, 1-day, and 1-month tiers), Business metrics (the same four tiers), and Diagnostic events (warn, error, fatal).

**Controls.** Edit any layer's retention, in days, within its bounds. Save submits only the layers you changed. Reset to defaults rolls every layer back to its built-in default, behind a confirmation dialog.

**What the page shows before anything is configured.** The form shows the
built-in defaults as *defaults*, not as settings in force. The distinction is the
whole point of the page: an operator reading a populated form reasonably concludes
those windows are what the platform is applying, and acting on that belief — "90
days is already configured, nothing to do" — is how a retention requirement goes
unmet while the screen looks correct. Values that have never been saved are
presented as such.

**Key concepts.** Each layer is a retention window in days bounded by a minimum and maximum. The four metric tiers (raw → 1-hour → 1-day → 1-month) keep progressively coarser rollups for progressively longer. Diagnostic retention is keyed by severity, so fatal events are kept longer than errors, and errors longer than warnings.

**Where the data comes from.** `retentionApi.get` / `put` → `/api/admin/observability/retention`, gated on `observability.read` to view and `observability.write` to save.

## SIEM

**Purpose.** Forward platform events to an external SIEM, filtered to the event types you choose.

**What you see.** An enable switch, the SIEM ingest URL, a wire-format select (JSON, CEF, or Syslog), an editable list of custom request headers, and an event-type filter laid out as a service → resource → event-type tree.

**Controls.** Toggle forwarding; set the URL, format, and headers; check the event types to forward — each service and each resource has a batch checkbox with an indeterminate state when only some children are selected. Save applies the configuration, and Send Test Event posts a sample to the configured endpoint and reports success or the error.

**Key concepts.** The wire format is JSON, CEF, or Syslog. The event-type tree groups by service (gateway, compliance, agent, platform, IAM), then resource, then event type — the same hierarchy the IAM catalog picker uses.

**Where the data comes from.** `systemApi.getSiemConfig` / `updateSiemConfig` / `listSiemEventTypes` / `sendSiemTestEvent` → `/api/admin/settings/siem` and its sub-paths, gated on `audit-log.read` to view the configuration and event-type catalog and `audit-log.write` to save or send a test event.

## Dead-letter Queue

**Purpose.** Inspect and replay audit and event messages that exhausted their redelivery attempts to the Hub consumer.

**What you see.** A subject filter over a table of dead-lettered messages: inserted-at, subject, message id, delivery count, payload size, and the last error. Rows are newest first, with the same paginated footer as every other admin list (rows-per-page, page numbers, row range, and total count).

**Controls.** Filter by subject and page through the rows with the shared pagination control. Each row has a Retry action that republishes the original payload to its MQ subject and deletes the row on success; Retry is shown only to users who can manage the queue.

**Key concepts.** A message lands here after the Hub consumer's redelivery threshold is hit; the delivery count and last error explain why. Retrying is the way to recover a message once the underlying cause is fixed.

**Where the data comes from.** `dlqApi.list` / `retry` → `/api/admin/observability/dlq`, gated on `observability-dlq.read` to list and `observability-dlq.manage` to retry.

## References

- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` — route registry and `nav: { sectionKey: 'infrastructure', ... }` blocks
- `packages/control-plane-ui/src/i18n/locales/en/nav.json` — sidebar labels
- `packages/control-plane-ui/src/pages/infrastructure/observability/SettingsObservabilityTab.tsx` — OpenTelemetry configuration
- `packages/control-plane-ui/src/pages/infrastructure/observability-retention/ObservabilityRetention.tsx` — retention layers
- `packages/control-plane-ui/src/pages/infrastructure/siem/SettingsSiemTab.tsx` — SIEM forwarder configuration and event-type filter
- `packages/control-plane-ui/src/pages/infrastructure/dlq/InfraDlqPage.tsx` — Dead-letter Queue
- `packages/control-plane-ui/src/pages/_shared/settings/SettingsPageWrappers.tsx` — the Observability and SIEM page wrappers
- `packages/control-plane-ui/src/api/services/infrastructure/misc/system.ts` — `systemApi` observability + SIEM calls
- `packages/control-plane-ui/src/api/services/infrastructure/ops/retention.ts` — `retentionApi`
- `packages/control-plane-ui/src/api/services/infrastructure/dlq/dlq.ts` — `dlqApi`
- `packages/control-plane/internal/settings/handler/settings/handler.go` — observability config routes and IAM gates
- `packages/control-plane/internal/observability/retention/handler/retention.go` — retention routes and IAM gates
- `packages/control-plane/internal/observability/siem/handler/siem.go` — SIEM routes and IAM gates
- `packages/control-plane/internal/observability/dlq/handler/dlq.go` — Dead-letter Queue routes and IAM gates
- `packages/shared/identity/iam/catalog_data.go` — the `observability`, `audit-log`, and `observability-dlq` resources
