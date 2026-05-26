# E74-S8 — Admin Configuration Surface + IAM Enforcement for `interceptMode`

> Epic: 74
> Story: 8
> Status: Planning
> Date: 2026-05-21
> FR mapping: FR-8.1, FR-8.2, FR-8.3, FR-8.4, FR-8.5
> Source decisions: DEC-001, DEC-007, DEC-012

---

## 1. User Story

As a **macOS agent operator (admin)**, I want to flip `interceptMode` between `ne` and `pf` from the existing Device Defaults settings page, see the currently applied mode (from reported state), and have every mode change captured in the audit log — so I can migrate the fleet to the pf path incrementally and roll back without reinstalling anything.

As a **viewer-role admin**, I want to read the current `interceptMode` value on that same page without being able to change it — so I get visibility without needing write access.

---

## 2. Tasks

### T8.1 — Add `interceptMode` Cat B shadow key to `agent_settings`

- Add `"interceptMode"` to the `agent_settings` system-metadata blob.
- Type: `string` enum constrained to `"ne" | "pf"`. The `"hybrid"` value is retired per DEC-001; any stored value of `"hybrid"` from a prior prototype run must be treated as `"ne"` by both the daemon and the admin API (normalise on read, reject on write with HTTP 400).
- The field is a Cat B shadow key per the `thing-config-pull-model` binding: the daemon pulls the value on boot and on every `OnConfigChanged` signal; Hub never pushes the full blob unprompted.
- Shadow key registration: `packages/shared/schemas/configkey/configkey.go` already defines `AgentSettings = "agent_settings"`. No new configkey constant is required. The `interceptMode` field is a new JSON sub-field within the existing blob — no catalog change needed.
- Default: not present in the stored blob means the daemon applies its compile-time default per DEC-007 (pf-only build: `"pf"`; legacy NE build: `"ne"`). The admin UI displays the live reported value (from Thing's reported state) rather than the desired blob value when desired is absent, so the operator sees what is actually running.

### T8.2 — Extend `configappliers.go` to consume `interceptMode` in the daemon

- In `packages/agent/cmd/agent/configappliers.go` `agentSettingsApply` closure (currently lines 87–145), add `InterceptMode string \`json:"interceptMode"\`` to the anonymous struct.
- On receipt: validate the value is `""`, `"ne"`, or `"pf"`. If `"hybrid"` is received, log a warning and normalise to `"ne"`. If any other non-empty string is received, log an error and ignore the field (do not crash; fail-open).
- Call `pfintercept.Supervisor.SetMode(mode)` (wired in Story S5) to atomically switch the active intercept path. If Story S5 is not yet wired in a given build, the call is a no-op behind an interface check — the apply function must not fail or panic when the supervisor is nil.
- Log `agent_settings shadow applied` with field `"interceptMode"` appended to the existing structured log line (line 138).
- Emit the `agent_intercept_mode_change` audit row per T8.5 below.

### T8.3 — Extend `GET /api/admin/settings/device-defaults` response

- File: `packages/control-plane/internal/settings/handler/settings/agent_settings.go`, `GetAgentSettings`.
- Add `"interceptMode"` to the response map. Source: `system_metadata` blob field `agent.settings.interceptMode`. Default returned when absent: `""` (empty string — UI displays this as "Default (build-stamped)").
- No new HTTP endpoint. No new router registration. No new IAM middleware call.

### T8.4 — Extend `PUT /api/admin/settings/device-defaults` handler

- File: same `agent_settings.go`, `UpdateAgentSettings`.
- Add `interceptMode` to the accepted request body struct. Accepted values: `""`, `"ne"`, `"pf"`. Reject `"hybrid"` and any other string with HTTP 400 `{"error": "interceptMode must be ne or pf"}`.
- On `""` (empty): remove `interceptMode` from the stored blob entirely (semantics: "let each build use its compile-time default").
- On `"ne"` or `"pf"`: write the string into `agent.settings.interceptMode` in `system_metadata` and publish the shadow update through Hub so connected agents receive `OnConfigChanged` within the Hub reconcile window.
- The existing `PUT` handler is already gated by `iamMW(iam.ResourceDeviceDefaults.Action(iam.VerbUpdate))` (registered at `handler.go:99`). No middleware change needed. A viewer-role admin who only holds `admin:device-defaults.read` will receive HTTP 403 on any PUT — enforced by the existing IAM middleware.

### T8.5 — Emit `agent_intercept_mode_change` audit row on mode flip

- When `UpdateAgentSettings` changes `interceptMode` to a different value from what was stored (including nil → value and value → nil transitions), emit an audit row of kind `agent_intercept_mode_change` carrying:
  - `old_mode`: the previously stored value (or `""` if absent).
  - `new_mode`: the value being written.
  - `admin_user_id`: the authenticated admin user from the Echo context.
  - `timestamp`: `time.Now().UTC()`.
- Use the existing `audit.Emit(ctx, h.audit, ...)` pattern from `packages/control-plane/internal/platform/audit/` — same shape as other setting-change audit rows in the handler.
- The audit row is visible in the admin Audit Log page (`/admin/governance/audit-log`) with kind filter `agent_intercept_mode_change`. No new UI surface needed — existing kind-agnostic log table renders it.

### T8.6 — Admin UI: add `interceptMode` dropdown to Device Defaults page

- File: `packages/control-plane-ui/src/pages/devices/agent-defaults/SettingsAgentTab.tsx`.
- Add `interceptMode?: string` to the `AgentSettingsData` interface.
- Add a `const [interceptMode, setInterceptMode] = useState<string>('')` state variable.
- In the `useEffect` sync block, set `setInterceptMode(data.interceptMode || '')`.
- In the `useMutation` save call, include `interceptMode` in the request body alongside existing fields.
- Render a single `<Select>` control inside a new `<Card>` below the existing QUIC Fallback Bundles card. The card title and all option labels use `t()` keys (T8.9 below). Three options:
  - `""` — `t('pages:agentSettings.interceptModeDefault', 'Default (build-stamped)')`
  - `"ne"` — `t('pages:agentSettings.interceptModeNE', 'NE legacy')`
  - `"pf"` — `t('pages:agentSettings.interceptModePF', 'pf (recommended)')`
- Below the dropdown, render a read-only status indicator showing the live applied mode from the agent Thing's reported state. Source: a separate `useApi` call to the existing Thing reported-state endpoint (already used elsewhere on the page for device status). Label: `t('pages:agentSettings.interceptModeApplied', 'Currently applied:')`. If the reported value is absent or the Thing is offline, display `t('common:unknown', 'Unknown')`.
- The `<Select>` is disabled when the `usePermission('admin:device-defaults.update')` check returns false, preventing viewer-role users from triggering the PUT.

### T8.7 — IAM Impact Review (5-step audit, DEC-001 / FR-8.4)

The five steps from `.cursor/rules/iam-impact-review.mdc` are applied here:

**Step 1 — UI `allowedActions` and backend `iamMW(...)` reference the same action.**
The `SettingsAgentTab` component does not declare its own `allowedActions` guard (it renders inside the Device Defaults settings page which is already behind the sidebar's `admin:device-defaults.read` visibility gate). The new `<Select>` control is additionally gated via `usePermission('admin:device-defaults.update')` so the UI disables the control for viewer-role users rather than hiding the entire card. The backend `PUT /api/admin/settings/device-defaults` is already gated by `iamMW(iam.ResourceDeviceDefaults.Action(iam.VerbUpdate))`. The action string `admin:device-defaults.update` matches on both sides. No drift.

**Step 2 — Decide resource carve-out.**
`interceptMode` lives within `device-defaults`. There is no reason to carve out a dedicated `agent-intercept-mode` resource: the value is one field in the agent settings blob, and granting `device-defaults.update` already implies the operator can change any field in that blob. Adding a new resource type would create a fine-grained permission that no existing role policy distinguishes, imposing admin cognitive load for zero security benefit. Decision: **keep on `admin:device-defaults.read` / `admin:device-defaults.update`**.

**Step 3 — No new resource; no fixture or seed changes required.**
Because no new resource is carved out (Step 2 decision), `packages/control-plane/internal/identity/iam/managed.go` and `tools/db-migrate/seed/seed.ts` do not change. Both already grant `admin:device-defaults.read` and `admin:device-defaults.update` to the `NexusAdmin` role, and `admin:device-defaults.read` to the `NexusViewer` role (confirmed in `managed.go:90`).

**Step 4 — No path rename or move.**
The surface lives on the existing `/admin/settings/device-defaults` route. No sidebar entry is added. No breadcrumb changes. No dead `case` arms accumulate.

**Step 5 — IAM decision recorded.**
Decision: **kept on `admin:device-defaults.read` / `admin:device-defaults.update`**. No new IAM action required. This decision must appear verbatim in the commit message for the handler + UI changes, per CLAUDE.md binding.

### T8.8 — Test: IAM enforcement negative test

- File: `packages/control-plane/internal/settings/handler/settings/agent_settings_test.go` (new or extend existing).
- Table-driven test cases:
  - `viewer_role_read_returns_200`: a request authenticated as a viewer-role identity (holds only `admin:device-defaults.read`) hits `GET /api/admin/settings/device-defaults` and receives HTTP 200 with an `interceptMode` field.
  - `viewer_role_write_returns_403`: the same viewer-role identity hits `PUT /api/admin/settings/device-defaults` with `{"interceptMode":"pf"}` and receives HTTP 403.
  - `admin_role_write_returns_200`: a request authenticated as an admin-role identity (holds `admin:device-defaults.update`) hits the same PUT and receives HTTP 200.
  - `invalid_mode_returns_400`: admin-role identity sends `{"interceptMode":"hybrid"}` and receives HTTP 400 with `{"error":"interceptMode must be ne or pf"}`.
  - `empty_mode_clears_field`: admin-role identity sends `{"interceptMode":""}` and the stored blob no longer contains `interceptMode`.
- All test cases use the existing `httptest.NewRecorder` + Echo test harness pattern from sibling handler test files. No real DB or Hub required — mock the `systemmetastore` interface.

### T8.9 — i18n key registration (all three locale bundles)

- Namespace: `pages` (the `SettingsAgentTab` already uses `pages:settings.*` keys).
- Add the following keys to all three locale files under `packages/control-plane-ui/src/i18n/locales/`:

  | Key | EN | ZH | ES |
  |---|---|---|---|
  | `pages:agentSettings.interceptModeTitle` | `Intercept Mode` | `拦截模式` | `Modo de interceptación` |
  | `pages:agentSettings.interceptModeDesc` | `Controls which kernel-level interception path the agent uses. pf (recommended) closes QUIC and raw-socket coverage gaps; NE legacy is the pre-E74 path retained as a rollback option.` | `控制 Agent 使用的内核级拦截路径。pf（推荐）可消除 QUIC 和原始套接字的覆盖盲区；NE 旧模式为 E74 之前的路径，保留作为回滚选项。` | `Controla qué ruta de intercepción a nivel de kernel utiliza el agente. pf (recomendado) cierra las brechas de cobertura QUIC y de socket sin procesar; NE heredado es la ruta anterior a E74 retenida como opción de reversión.` |
  | `pages:agentSettings.interceptModeDefault` | `Default (build-stamped)` | `默认（构建内置）` | `Predeterminado (integrado en build)` |
  | `pages:agentSettings.interceptModeNE` | `NE legacy` | `NE 旧模式` | `NE heredado` |
  | `pages:agentSettings.interceptModePF` | `pf (recommended)` | `pf（推荐）` | `pf (recomendado)` |
  | `pages:agentSettings.interceptModeApplied` | `Currently applied:` | `当前已应用：` | `Actualmente aplicado:` |

- Verify key parity: after adding keys, run `npm run i18n:check` (or the equivalent gap-check invocation) and confirm zero missing keys across EN / ZH / ES.

### T8.10 — domain.Engine shared-use verification (DEC-012 gate)

- Per DEC-012, the pf listener's domain evaluation path MUST import `packages/shared/policy/domain.Engine` directly and MUST NOT define a parallel evaluation function in any agent-private package.
- Add a CI-enforcement comment in Story S5's wiring code: `// DEC-012: one domain.Engine instance shared across bridge, listener, and tlsbump — do not fork`.
- Verification commands (run in Phase X+1 second-round review, cited here so the reviewer knows what to check):
  ```sh
  # Only shared/policy/domain defines Engine struct:
  grep -RIlE 'type\s+Engine\s+struct' packages/ --include='*.go' | grep -v '_test.go'
  # Expected: exactly one match — packages/shared/policy/domain/engine.go

  # Agent pf listener does not host its own evaluate logic:
  grep -RInE 'func.*Evaluate.*pfintercept' packages/agent/ --include='*.go'
  # Expected: zero matches
  ```
- This task has no code deliverable for Story S8 specifically; it is a review gate that Story S8 documents so the S5 PR author knows the check is expected.

---

## 3. Acceptance Criteria

All criteria are observable by a tester with admin access to a locally running stack:

1. **Shadow key round-trip**: After `PUT /api/admin/settings/device-defaults` with `{"interceptMode":"pf"}`, a subsequent `GET` returns `{"interceptMode":"pf"}` and the agent Thing's desired state in Hub contains `interceptMode: "pf"`. After `PUT` with `{"interceptMode":""}`, `GET` returns `{"interceptMode":""}` and the desired blob no longer contains the key.

2. **Daemon applies the mode**: Within the Hub reconcile window (≤5 s on the local stack), the agent daemon receives `OnConfigChanged` and logs `agent_settings shadow applied interceptMode=pf`. The pf anchor is installed if Story S5 is wired (verified by `sudo pfctl -s all -a nexus-agent/transparent` showing rules); the NE extension is stopped.

3. **Mode-change audit row**: After flipping `interceptMode` via the admin UI, the Governance → Audit Log page shows a row of kind `agent_intercept_mode_change` with `old_mode` and `new_mode` fields populated. The row appears within ≤10 s of the PUT.

4. **UI shows applied value**: The Device Defaults page shows the dropdown reflecting the desired `interceptMode` AND a separate read-only "Currently applied:" indicator reflecting the live reported value from the agent Thing. When desired and applied differ (e.g., the agent is offline), both values are visible independently so the operator understands the gap.

5. **Viewer-role 403**: A session authenticated as a viewer-role admin can load the Device Defaults page (HTTP 200 on GET) and sees the intercept mode dropdown in a disabled state. Clicking Save issues a PUT that returns HTTP 403. The UI does not crash; it surfaces the 403 as a standard error banner.

6. **Reject invalid mode**: `PUT /api/admin/settings/device-defaults` with `{"interceptMode":"hybrid"}` returns HTTP 400 with `{"error":"interceptMode must be ne or pf"}`. No audit row is emitted for the rejected request.

7. **i18n key parity**: `npm run i18n:check` (or equivalent) exits 0 with no missing keys across EN / ZH / ES for all six keys added in T8.9.

8. **No new route or nav entry**: `git diff main -- packages/control-plane-ui/src/routes/ packages/control-plane-ui/src/components/ui/Sidebar/` shows zero changes. The interceptMode surface lives exclusively within the existing Device Defaults page.

---

## 4. Interface Contract

### 4.1 Hub Admin API — `PUT /api/admin/settings/device-defaults` (existing endpoint, extended)

No new endpoint is added. The existing endpoint at `packages/control-plane/internal/settings/handler/settings/handler.go:99` is extended with one new optional field.

**Request body (partial — existing fields omitted for brevity):**

```json
{
  "interceptMode": "pf"
}
```

`interceptMode` is optional. Accepted values: `""` (clear), `"ne"`, `"pf"`. Any other non-empty string → HTTP 400.

**Response body (partial):**

```json
{
  "interceptMode": "pf"
}
```

`interceptMode` is always present in the GET response. Value is `""` when the stored blob does not contain the key (meaning the daemon uses its build-stamped default).

**IAM gate (unchanged):**
- `GET`: `iamMW(iam.ResourceDeviceDefaults.Action(iam.VerbRead))` → `admin:device-defaults.read`
- `PUT`: `iamMW(iam.ResourceDeviceDefaults.Action(iam.VerbUpdate))` → `admin:device-defaults.update`

### 4.2 UI Prop Shape

The `SettingsAgentTab` component consumes the existing `AgentSettingsData` interface, extended:

```typescript
interface AgentSettingsData {
  // ... existing fields unchanged ...
  interceptMode?: string;  // "" | "ne" | "pf"; absent === ""
}
```

The status indicator sources the live reported value from the existing device Thing polling mechanism (no new API call shape required; the Thing's reported-state endpoint already carries the `agent_settings` reported blob).

### 4.3 Audit Row Shape

Kind: `agent_intercept_mode_change`

```json
{
  "kind": "agent_intercept_mode_change",
  "actor_id": "<admin-user-uuid>",
  "timestamp": "2026-05-21T10:00:00Z",
  "details": {
    "old_mode": "ne",
    "new_mode": "pf"
  }
}
```

`old_mode` is `""` when no prior value was stored. `new_mode` is `""` when the field is being cleared.

---

## 5. Dependencies

**Upstream (this story consumes):**

- `AgentSettings` Cat B shadow key infrastructure (`configkey.AgentSettings = "agent_settings"`) — already in `packages/shared/schemas/configkey/configkey.go:61`. No new configkey needed.
- `iam.ResourceDeviceDefaults` — already in `packages/shared/identity/iam/catalog_data.go:222`. No catalog change needed.
- Existing `devicesApi.getAgentSettings()` / `devicesApi.updateAgentSettings()` service layer in the CP UI — extended in place, no new service method.

**Downstream (other stories consume this story's output):**

- **Story S5** (interceptMode wiring + pf supervisor): S5 reads `interceptMode` from the shadow blob (applied by T8.2's extended `configappliers.go`). S8 must land before or alongside S5 — the shadow key must exist for S5 to consume.
- **Story S1** (pf rules + QUIC fallback): S1 uses `quicFallbackUIDs` derived from `forceQUICFallbackBundles` (existing bundle list field, unchanged admin surface per FR-8.2). S1 depends on the daemon's `configappliers.go` already consuming the bundle list (current) and the new mode field (T8.2). S8 is a soft prerequisite for S1's UID-resolution path.

---

## 6. Out of Scope

- **Per-host or per-bundle overrides for `interceptMode`**: FR-8.1 explicitly states a single global dial. The Device Defaults page is fleet-wide; per-device overrides are not in scope for E74 or any currently queued epic.
- **Admin UI editor for `capabilityJson` or `interception_domain` rules**: those surfaces belong to E62 (domain rule management). This story touches only `interceptMode` within `agent_settings`.
- **The `"hybrid"` value**: DEC-001 retires hybrid mode. The handler rejects it with HTTP 400; `configappliers.go` normalises any stale stored value to `"ne"` on read. No UI option is rendered for hybrid.
- **pf rule introspection in the admin UI** (`pfctl -s rules` read-only view): deferred per the Requirements §7 "Could" tier. No admin surface is added for raw pf rule listing.
- **New sidebar nav entry or admin route**: FR-8.3 is explicit — `interceptMode` lives on the existing Device Defaults settings page. Zero routing changes.
- **`system_metadata` key catalog update for `interceptMode` sub-field**: `interceptMode` is a JSON sub-field within the existing `agent.settings` metadata key, not a new top-level key. The §7 per-key catalog in `configuration-architecture.md` documents `agent.settings` as a single Cat B key; the sub-field does not need its own catalog entry.

---

## 7. References

- `docs/developers/specs/e74-macos-pf-intercept.md` §FR-8 — Configuration surface requirements (FR-8.1 through FR-8.5).
- `docs/developers/specs/e74/DECISIONS.md` — DEC-001 (hybrid retired, enum collapses to `ne | pf`), DEC-007 (default is build-stamped, not an admin UI dial), DEC-012 (`shared/policy/domain.Engine` stays the sole evaluation engine; no per-Thing fork).
- `.cursor/rules/iam-impact-review.mdc` — 5-step IAM audit applied in T8.7.
- `packages/control-plane/internal/settings/handler/settings/agent_settings.go` — existing `GetAgentSettings` / `UpdateAgentSettings` handler extended in T8.3 and T8.4.
- `packages/control-plane/internal/settings/handler/settings/handler.go:98-99` — route registration confirming `ResourceDeviceDefaults` IAM gate.
- `packages/shared/identity/iam/catalog_data.go:116, 222` — `device-defaults` resource definition confirming `read` + `update` verbs.
- `packages/control-plane/internal/identity/iam/managed.go:90` — `NexusViewer` fixture confirming `admin:device-defaults.read` is granted to viewer role.
- `packages/agent/cmd/agent/configappliers.go:87-145` — `agentSettingsApply` closure extended in T8.2.
- `packages/control-plane-ui/src/pages/devices/agent-defaults/SettingsAgentTab.tsx` — UI file extended in T8.6.
- `docs/developers/architecture/services/control-plane/iam-identity-architecture.md` — canonical IAM architecture reference.
