# Control Plane UI — Devices

The DEVICES sidebar section manages the fleet of enrolled Agent endpoints. It has four leaves: **Devices**, **Device Groups**, **Device Auth**, and **Device Defaults**. Sidebar labels and routes are defined in `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`.

A "device" in the UI is an enrolled Agent endpoint; the admin device API serves it from `/api/admin/agent-devices`.

## Devices

**Purpose.** The fleet inventory — every endpoint that has enrolled an Agent.

**List page.** Columns: hostname (with a short physical-id / id digest), bound user, OS (darwin shown as macOS, windows as Windows), primary IP, agent version, status, and last heartbeat. Filters cover status (ACTIVE, ENROLLED, OFFLINE, REVOKED), OS, and a search box. The header has an "Enroll device" action.

**Enrollment.** A device joins the fleet by enrolling, and the enrollment method is set by the Device Auth mode (see below). In `mtls-only` mode, the "Enroll device" dialog generates a one-time enrollment token (with an optional hostname) that is shown once, is copyable, and carries an expiry; the installer presents it to the Hub. In the login modes, the device enrolls through a browser login instead, which binds it to the authenticated user.

**Detail.** The detail page shows an identity card (hostname, bound user, physical id, thing id, IP, OS, agent version, last heartbeat, enrolled-at and enrolled-by) and a free-form tag editor (tags up to 64 characters, replaced as a set). It has tabs for traffic, compliance, configuration (the effective config JSON), system (host info and NICs), and activity (assignments plus admin audit). Actions are force-refresh, rotate-cert, unenroll (revoke), and diagnostic-mode for a 30-minute, 2-hour, or 8-hour window.

**Key concepts.** A device's status is `online`, `enrolled`, `offline`, `out-of-sync`, or `revoked`. Each device is identified by a hardware-stable fingerprint — a 128-bit hash the Agent derives from stable hardware signals (hardware UUID, hardware serial, primary MAC, and CPU model), carried as the device's physical id. When a device re-enrolls, the Hub matches on this fingerprint and reuses the existing thing id instead of minting a new one, so the device keeps the same identity — and its history, configuration, and user assignment — across re-enrollments. The bound user is the person the device is assigned to.

**Where the data comes from.** `devicesApi` — `list`, `get`, `getEvents`, `getAssignments`, `forceRefresh`, `rotateCert`, `setTags`, `generateEnrollToken`, `unenroll`, `listMine`; `fleetApi` — `getDeviceTimeline`, `getDeviceAudit`, `getDeviceConfig`; `diagModeApi` — `list`, `enable`, `disable`.

## Device Groups

**Purpose.** Logical groupings of devices for bulk actions and config targeting, with either explicit or rule-driven membership.

**List page.** Columns: name, description, member count, and created-at. Row actions are edit and delete.

**Create and detail.** Creation collects a name and an optional description. The detail page shows a summary, a static members table (hostname, OS, status, an optional expiry, and a remove control, with a searchable add-member picker), a smart-membership card (a predicate editor with a preview of matches and save/revert), and a bulk-actions card (force config refresh, rotate certs — fanned out with per-device results).

**Key concepts.** A group supports both membership models at once. Static membership adds and removes specific devices, each with an optional expiry. Smart membership is a predicate query that the Hub recomputes about every 60 seconds; a badge marks each group as Static or Smart. Predicate operators include `eq`, `ne`, `in`, `nin`, `prefix`, `regex`, `cidr`, `lt`/`le`/`gt`/`ge`, `relative_seconds_within`, `idp_group_member`, `tags_contains`, and `tags_contains_all`, combined under a top-level `all` (AND) or `any` (OR). The `idp_group_member` operator binds an external IdP group to a device group.

**Where the data comes from.** `deviceGroupsApi` — `list`, `get`, `create`, `update`, `delete`, `addMember`, `removeMember`, `previewMembership`, `setMembershipQuery`, `bulkForceRefresh`, `bulkRotateCert`.

## Device Auth

**Purpose.** Select how Agents authenticate, which in turn sets how new devices enroll.

**What you see.** A single settings field — a `mode` radio group with three options: `mtls-only`, `local-login`, and `enterprise-login`. The save control is disabled when the chosen mode's backing is unavailable: `enterprise-login` shows a read-only list of the configured SSO identity providers (name and type) and warns when none are configured; `local-login` warns when the local Nexus login store is unavailable.

**How the mode drives enrollment.** The mode decides which credential a device presents when it enrolls with the Hub:

- **`mtls-only`** — token enrollment. An admin generates a one-time token in the Devices "Enroll device" dialog and the installer presents it. The device is not bound to a user and starts at the base trust level.
- **`local-login`** and **`enterprise-login`** — login enrollment. The Agent runs a browser login flow, receives an enrollment JWT, and enrolls with it, which binds the device to the authenticated user. The two differ only in the login backend: `enterprise-login` authenticates against an external SSO identity provider, while `local-login` authenticates against the local Nexus login store. The Agent drives both through a single browser-login path — the Hub bootstrap presents `local-login` to the Agent as `enterprise-login`, while the raw mode stays in system metadata for admin and audit visibility.

**Key concepts.** The identity providers referenced here are the shared IAM Identity Providers, managed under the IAM section; Device Auth references them read-only.

**Where the data comes from.** `fleetApi` — `getDeviceAuthSettings`, `updateDeviceAuthSettings`.

## Device Defaults

**Purpose.** The fleet-wide Agent runtime defaults, delivered to every Agent through the `agent_settings` shadow config key.

**What you see.** Cards for the Agent quit policy (`quitAllowed`), Agent attestation (`attestationEnabled`), a shutdown warning (`shutdownWarningEnabled` plus a per-locale message in English, Chinese, and Spanish), a runtime-defaults grid, and the QUIC fallback bundle list (macOS).

**Controls.** The runtime grid sets the heartbeat, audit-drain, and config-sync intervals (each server-clamped between 10 seconds and 86400 seconds), the audit batch size (empty uses the YAML default), the log level (`debug`, `info`, `warn`, `error`), the auto-update channel (`stable`, `beta`) and an auto-update enabled toggle, the traffic upload level, a theme id, and the forced QUIC fallback bundle list.

**Key concepts.** The traffic upload level is `all`, `processed` (the default), or `blocked` — it sets how much traffic each Agent uploads. The auto-update channel is `stable` or `beta`. The theme id selects the Agent UI skin. The forced QUIC fallback bundle list is an allowlist of macOS bundle ids; system processes must never be added to it.

**Where the data comes from.** `devicesApi` — `getAgentSettings`, `updateAgentSettings`.

## Deep-link pages

Three pages live in the Devices area without a sidebar entry. The **Fleet Overview** page (`/fleet-overview`) is a fleet-status analytics dashboard — KPI cards (total, active, stale, critical), a status pie and health bar, a status-trend line chart, and a top-destinations table — with a banner pointing to Infrastructure → Nodes. The **Fleet Users** pages (`/fleet/users`) list the Agent end users (display name, email, status, created-at) and, per user, show identity, a devices tab, and an audit tab, with suspend and activate actions. The **device detail** page is reached from a device row (`/devices/:id`) or from a user's device list (`/fleet/devices/:id`).

## References

- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` — route registry and `nav: { sectionKey: 'devices', ... }` blocks
- `packages/control-plane-ui/src/i18n/locales/en/nav.json` — sidebar labels
- `packages/control-plane-ui/src/pages/devices/DeviceListPage.tsx` — Devices list, enroll token dialog, tag editor
- `packages/control-plane-ui/src/pages/devices/FleetDeviceDetailPage.tsx` — device detail
- `packages/control-plane-ui/src/pages/devices/groups/` — Device Groups list, form, detail, smart-membership, bulk actions
- `packages/control-plane-ui/src/pages/devices/auth/DeviceAuthSettingsPage.tsx` — Device Auth mode
- `packages/control-plane-ui/src/pages/devices/agent-defaults/SettingsAgentTab.tsx` — Device Defaults
- `packages/control-plane-ui/src/pages/fleet-analytics/FleetOverviewPage.tsx` — Fleet Overview deep-link
- `packages/control-plane-ui/src/pages/fleet/` — Fleet Users deep-link pages
- `packages/nexus-hub/internal/identity/handler/enroll/enrollment_handler.go` — the token and login (JWT) enrollment paths
- `packages/nexus-hub/internal/identity/handler/bootstrap/agent_bootstrap.go` — the `local-login` → `enterprise-login` bootstrap normalization
- `packages/shared/core/metrics/platform/fingerprint.go` — the hardware-stable device fingerprint (physical id) computation
- `packages/agent/internal/identity/enrollment/` — the Agent-side enrollment flows
- `packages/control-plane-ui/src/api/` — `devicesApi`, `deviceGroupsApi`, `fleetApi`, `fleetAnalyticsApi`, `diagModeApi`
- `tools/db-migrate/schema.prisma` — `DeviceGroup`, `DeviceGroupMembership`, `DeviceAssignment` models
