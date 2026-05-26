# Recipe Adding A Runbook

*Audience: contributors and operators documenting a new operational procedure.*

A runbook captures a repeatable operational procedure — incident response, migration, credential rotation, version rollout, or smoke testing — in a form an on-call engineer can follow under pressure. Runbooks live in `docs/operators/ops/runbooks/` and follow a consistent shape so operators find the same sections in the same order across every runbook. This recipe shows how to write a new runbook, keep it synchronized with code changes, and cross-link it to the canonical architecture doc.

---

## Runbook structure

Every runbook has four sections:

1. **State model / overview** — what the procedure accomplishes, which services are involved, and what success looks like.
2. **Common actions** — step-by-step procedures for the normal case. Numbered prose, not bare bullets.
3. **Failure modes** — what can go wrong, how to recognize each failure, and the remediation command.
4. **Verification** — commands the operator runs to confirm the procedure worked.

A runbook that skips any of these four sections is incomplete for on-call use. The "Verification" section is the most commonly omitted — it is also the most valuable when an operator is unsure whether their action took effect.

---

## Step 1 — Choose a filename

Runbook filenames follow two patterns:

- **Incident/topic runbooks**: `<topic>.md` — for example `alerts.md`, `compliance-proxy-smoke.md`, `ops-metrics-smoke-test.md`.
- **Epic-scoped runbooks**: `<epic>-<description>.md` — for example `e61-valkey-migration.md`, `prod-deploy-data-changes.md`.

Place the file under `docs/operators/ops/runbooks/<filename>.md`. Add it to the index in `docs/operators/ops/runbooks/` (if an `index.md` or `README.md` exists there) and to the wiki's [Operations Runbook Index](Operations-Runbook-Index) page.

## Step 2 — Write the state model

Open with a short paragraph explaining what the runbook covers and which services are affected. If the procedure involves state transitions (alert states, migration phases, service restates), add a state diagram:

```markdown
## State model

```
INITIAL → IN_PROGRESS → COMPLETE
INITIAL → FAILED (rollback to INITIAL)
```

| State | Meaning | How to reach it |
|---|---|---|
| `INITIAL` | Procedure not yet started | Default |
| `IN_PROGRESS` | Procedure underway | Step 3 below |
| `COMPLETE` | All steps confirmed | Verification commands return expected output |
```

## Step 3 — Write common actions as numbered prose

Use numbered prose, not bare bullets. Each step names what to do, which file or command is involved, and the expected output. For example:

```markdown
## Common actions

### Performing a rotation

1. Generate the new credential via the admin UI or API:
   `cp_curl -X POST /api/admin/credentials/<id>/rotate`
   Expected response: `{"status":"rotation_pending","rotatesAt":"..."}`.

2. Confirm the new credential is stored: the `Credentials` page in CP shows
   the updated `rotatedAt` timestamp within 30 seconds.

3. Restart the dependent service so it picks up the new credential from its
   next config pull:
   `sudo systemctl restart nexus-ai-gateway`

4. Verify the service came up with the new credential by checking the log:
   `journalctl -u nexus-ai-gateway -n 50 | grep "credential loaded"`
```

## Step 4 — Document failure modes

Add a failure-modes table covering the most likely failure paths:

```markdown
## Failure modes

| Symptom | Cause | Remediation |
|---|---|---|
| Service fails to start after rotation | New credential not yet propagated | Run `cp_curl /api/admin/credentials/<id>` to confirm status is `active`; wait 60s and retry |
| `403 Forbidden` on dependent API calls | Old credential still in cache | Force-flush cache: `cp_curl -X POST /api/admin/cache/flush?key=credentials` |
| Hub shows Thing as `offline` | Service restarted before Hub received the change-signal | Wait 90s for reconnect; if still offline, check service logs for `thingclient` errors |
```

## Step 5 — Write the verification section

Include runnable commands that confirm the procedure succeeded. These commands should be copy-pasteable:

```markdown
## Verification

```bash
# Confirm the procedure's end state in Postgres:
docker exec postgres psql -U postgres -d nexus_gateway \
  -c "SELECT <relevant columns> FROM <table> WHERE <condition>;"

# Confirm the service is healthy:
curl http://localhost:3050/healthz

# Confirm the change-signal completed (Config Sync page):
cp_curl /api/admin/things?kind=<service_kind> | jq '.[].status'
# Expected: all Things show "online"
```
```

## Step 6 — Cross-link to the architecture doc

At the end of the runbook, add a "References" section that links the canonical architecture doc(s) for the subsystem the runbook covers. This link is the "why" companion to the runbook's "how":

```markdown
## References

- `docs/developers/architecture/<area>/<doc>-architecture.md` — canonical architecture for this subsystem
- `docs/operators/ops/runbooks/` — sibling runbooks for related procedures
```

## Step 7 — Register in the doc-lockstep map

If the runbook covers a code area that is already in `scripts/doc-lockstep.config.mjs`, add the runbook path to that area's doc list. This ensures future code changes in the area automatically trigger a reminder to update the runbook.

If the runbook covers a new code area not yet in the lockstep map, add a mapping entry. The config format is:

```javascript
{ globs: ['packages/<service>/**/*.go'], docs: ['docs/operators/ops/runbooks/<filename>.md'] }
```

Without this, code changes silently diverge from the runbook.

## Step 8 — Add to the wiki Operations Runbook Index

Update the [Operations Runbook Index](Operations-Runbook-Index) wiki page (if it exists) by adding a row for the new runbook:

```markdown
| `<filename>.md` | <One-line description of what procedure it covers> |
```

---

## What links break if you skip this

- **Skipping the verification section**: on-call engineers complete the steps but cannot confirm success, leading to either re-running the procedure unnecessarily or leaving a failed state undetected.
- **Skipping the lockstep map entry**: a code change in the runbook's area (new config option, renamed API endpoint, changed service restart command) does not trigger a PR check, so the runbook silently becomes stale. Operators following stale runbooks in incidents cause secondary failures.
- **Skipping the architecture doc cross-link**: the runbook's "how" floats without a "why", making it harder for engineers unfamiliar with the subsystem to adapt the procedure to an unexpected failure mode.

---

## Canonical docs

- [`docs/operators/ops/runbooks/alerts.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/alerts.md) — example runbook: state model + common actions + failure modes + verification
- [`docs/operators/ops/runbooks/compliance-proxy-smoke.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/compliance-proxy-smoke.md) — example runbook: smoke-test procedure pattern
- [`scripts/doc-lockstep.config.mjs`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/scripts/doc-lockstep.config.mjs) — code-glob → doc-file lockstep registry

**Adjacent wiki pages**: [Operations Runbook Index](Operations-Runbook-Index) · [Operations Day 2 Cheatsheet](Operations-Day-2-Cheatsheet) · [Dev Code Doc Lockstep](Dev-Code-Doc-Lockstep) · [Recipe Index](Recipe-Index)
