# Nexus Gateway — Examples

Runnable demonstrations of the things you do **once** the gateway is running. The repo's top-level [`README.md`](../README.md) covers getting all five services + the UI up; this directory covers using them.

Each example is a self-contained subdirectory with its own `README.md` and a script you can copy-paste.

---

## Examples

| # | Path | Demonstrates |
|---|---|---|
| 1 | [`01-hello-world/`](./01-hello-world/) | The canonical "make an AI request through the gateway and watch it land in the audit timeline" walkthrough. ~3 minutes. Curl + a peek at the resulting `traffic_event` Postgres row. |

More to come — see the per-section feature docs under [`docs/users/features/`](../docs/users/features/) for now.

---

## Prerequisites

Every example assumes you have run [`./scripts/dev-start.sh`](../scripts/dev-start.sh) and the local stack is up. Verify with:

```bash
curl -fsS http://localhost:3050/healthz   # AI Gateway
curl -fsS http://localhost:3060/healthz   # Hub
curl -fsS http://localhost:3001/healthz   # Control Plane
```

If any of those fail, return to the top-level README's "Quick start" section before continuing.

---

## Conventions

- Every example uses the **seeded** virtual key, providers, and models. The seed lives in `tools/db-migrate/seed/data/prod-data.sql.example` ([note: review this file for the demo dataset shape](../tools/db-migrate/seed/data/README.md)).
- Curl examples target `localhost` ports as documented in the top-level README. For your own deployment, replace `localhost:<port>` with your service URL.
- Examples assume the seeded super-admin (`admin@nexus.ai / admin123`) for any admin-API call. Use [`tests/lib/auth.sh`](../tests/lib/auth.sh)'s `cp_login` / `cp_curl` helpers for token-managed admin calls.
