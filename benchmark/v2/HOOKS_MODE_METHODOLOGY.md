# Hooks-Mode Methodology — the VALID way to benchmark Nexus hooks-OFF

**Why this document exists:** an earlier hooks-OFF arm was produced by editing hook
rows directly in the database. That is **invalid** and its numbers must not be
compared or published. This document records why, the correct propagation path, the
runtime proof the harness now stamps into every result, and the restore guarantee.

---

## 1. Why a direct DB edit is invalid

Nexus hooks are **Category-B configuration**. The running AI Gateway does not read
hook state from the database on the request path — it holds a hot in-memory
`HookConfigCache` that is only swapped when the Hub pushes a new shadow version over
the WebSocket. So:

- `UPDATE "Hook" SET enabled=false …` changes the *stored* rows.
- The **running gateway keeps executing the old (enabled) hook set** — the cache
  was never told to reload.

A benchmark run started right after a DB edit therefore measures a gateway that is
**still governed**, while the operator believes hooks are off. The result looks like
"hooks-off" but is really "hooks-on with stale-looking rows." That is the trap this
methodology closes.

**Rule:** never toggle hooks for a benchmark by editing the DB. Always go through the
Control Plane, and always prove the gateway runtime actually applied the change
before load starts.

---

## 2. The correct propagation path

```
CP admin API                     Hub                         AI Gateway (runtime)
PUT /api/admin/hooks/{uuid}  →  POST /api/hub/config/update  →  shadow desired_ver++
   {enabled:false}                  (updates shadow)              │
                                                                  │ WebSocket push
                                                                  ▼
                                          thingclient.OnConfigChanged
                                                                  │
                                                                  ▼
                                          HookConfigCache.Reload → resolver.Swap
                                                                  │
                                                                  ▼
                                          reported_ver catches up to desired_ver
```

Convergence — the gateway has genuinely applied the push — is:

```
meta.desired_ver == meta.reported_ver   AND   the target hooks show enabled=false
```

Both conditions matter. `desired_ver == reported_ver` alone only says "the gateway
acknowledged *some* config version"; we additionally require the specific compliance
hooks to be absent from the enabled set at the runtime.

---

## 3. How the harness toggles and proves it

The harness does **not** reimplement CP OAuth in Python. It delegates the actual
on/off toggle to the proven `scripts/hooks_toggle.sh` (OAuth PKCE → discover hook
UUIDs by name → `PUT …/hooks/{uuid}` → poll `GET /api/admin/nodes/{id}/runtime` to
convergence, nonzero exit on timeout). Python owns the structured proof and the
result metadata.

- `engine/hooks_control.py`
  - `set_hooks("off"|"on")` → shells out to `hooks_toggle.sh`; raises
    `CalledProcessError` if the script's own convergence poll fails (so the run
    never starts against an unconverged gateway).
  - `read_runtime_state(cp_url, node_id)` → `GET /api/admin/nodes/{id}/runtime`,
    parses `snapshot.sources["config.hooks"]`, and returns a `RuntimeState`:
    `source_ok`, `hook_count` (enabled), `response_hook_count` (enabled
    response-stage), `enabled_names`, `desired_ver`, `reported_ver`.
    **If there is no admin key, counts are `None` — never a fabricated `0`.**
  - `poll_until_converged(...)` → bounded poll; converged only when versions match
    AND the target hooks match the requested mode. Unreadable runtime never reports
    converged.

### The `governance` block stamped into every hooks-off result

`cli.py run-nexus-hooks-off` records, on each result row:

| field | meaning |
|-------|---------|
| `requested_mode` | `"hooks-off"` |
| `control_plane_verified` | `true` — `set_hooks` (bash PUT + verify) succeeded |
| `gateway_runtime_verified` | `true` only if the CP-admin runtime read succeeded |
| `runtime_hook_count` | enabled hooks at the runtime (null if unreadable) |
| `runtime_response_hook_count` | enabled response-stage hooks (must be `0` for off) |
| `hook_names` | the enabled hook names the runtime reported |
| `desired_ver` / `reported_ver` | shadow versions (must be equal) |
| `audit_disabled_env_present` | whether `NEXUS_AUDIT_DISABLED` was set (must be false) |
| `runtime_read_source` | `"cp-admin"` if the structured read worked, else `"bash-exit-code"` |

`scripts/validate_benchmark.py` **BLOCKS** any hooks-off result where
`gateway_runtime_verified != true`, or `runtime_response_hook_count > 0`, or
`audit_disabled_env_present == true`. A DB-edit "hooks-off" run cannot pass this gate.

> If the harness has no admin key (`NEXUS_ADMIN_API_KEY` unset), the structured read
> falls back to `runtime_read_source: "bash-exit-code"` and `gateway_runtime_verified`
> is `false` — the bash script still proved convergence via its own poll+exit-code,
> but because Python could not independently read the runtime, the result is
> **conservatively flagged and blocked** rather than published on trust. Set the admin
> key to get a publishable hooks-off result.

---

## 4. The restore guarantee

`run-nexus-hooks-off` disables hooks, runs the scenario inside a `try`, and
re-enables hooks in a `finally` — **even if the scenario fails**. A governed box is
never left silently unhooked.

If re-enable itself fails, the command:
1. writes `results/HOOKS_NOT_RESTORED.json` (`severity: high`) with a remediation
   line, and
2. exits non-zero (`Exit(2)`).

Remediation: `python scripts/nexus_hooks_control.py on` (or `scripts/hooks_toggle.sh
on`), then `python scripts/nexus_hooks_control.py status` to confirm before any
production traffic.

---

## 5. Operator checklist (hooks-off arm)

1. `NEXUS_CP_URL` points at the Control Plane; `NEXUS_ADMIN_API_KEY` is set (for the
   structured runtime read — otherwise the result is blocked as unverified).
2. `NEXUS_AUDIT_DISABLED` is **unset** (it is an orthogonal diagnostic that drops
   audit enqueue; leaving it set taints a governed benchmark and the validator
   blocks it).
3. Run: `python cli.py run-nexus-hooks-off --scenario s02 --duration 300 --warmup 30`.
4. Confirm the result's `governance.gateway_runtime_verified == true` and
   `runtime_response_hook_count == 0`.
5. `python scripts/validate_benchmark.py results/results_*.json` → no BLOCK.
6. Confirm hooks are back on: `python scripts/nexus_hooks_control.py status`.

See also: [LITELLM_STABILITY_GUIDE.md](LITELLM_STABILITY_GUIDE.md), [RUNBOOK.md](RUNBOOK.md).
