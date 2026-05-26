# Compliance Proxy

Transparent forward HTTPS proxy. Sits in the outbound path between an
application's HTTPS client and any AI provider, terminates the TLS
session via "TLS bump" (MITM with an enrolled CA), runs the same
compliance pipeline the AI Gateway runs (hooks, payload capture,
exemption matching), and forwards the request to the original
destination. Used when integrating clients you can't make point at the
AI Gateway's `/v1/*` directly (consumer-surface chat tools, agent
toolchains, etc.).

## Where it sits

| | |
|---|---|
| **Port** | `3040` (HTTP CONNECT proxy port; also serves `/runtime/*` ops endpoints) |
| **DB** | PostgreSQL (`traffic_event` rows with `source='compliance-proxy'`, audit spool) |
| **Cache** | Redis (desired-state cache, rate-limit) |
| **Spill store** | S3-compatible (`shared/spillstore`) for payload capture |
| **Registers as Thing** | `type=compliance-proxy`; receives shadows for hooks, allowlists, kill switch, payload capture, exemptions |

## Build

```bash
make compliance-proxy-build   # outputs to dist/bin/compliance-proxy/compliance-proxy
# or
cd packages/compliance-proxy && go run ./cmd/compliance-proxy/ -config compliance-proxy.dev.yaml
```

## Test

```bash
make compliance-proxy-test    # go test -race -count=1 ./...
```

## Key directories

| Path | Purpose |
|---|---|
| `cmd/compliance-proxy/` | Process entry. Wires the CONNECT proxy listener, TLS-bump engine, hook engine, Hub client, runtime API. |
| `internal/proxy/` | Bump path + forward handler. Terminates TLS, dispatches to hooks, relays to upstream. |
| `internal/access/` | DNS + domain allowlist engine (matched before bump). |
| `internal/cert/` | Intermediate CA, per-host leaf issuance cache. |
| `internal/compliance/` | Hook executor stack (request hooks, response hooks, payload capture). |
| `internal/audit/` | Outbound audit event spool + drain to Hub. |
| `internal/configloader/` | Shadow loader for the eight Cat-B config keys (`hooks`, `allowlists`, `kill_switch`, etc.). |
| `internal/runtimeapi/` | Operator-facing `/runtime/*` endpoints (gated by `COMPLIANCE_PROXY_API_TOKEN`). |

## Configuration

- `compliance-proxy.dev.yaml` — local boot defaults.
- `compliance-proxy.prod.yaml.example` — production template.
- Secrets via env: `INTERNAL_SERVICE_TOKEN` (must match Hub),
  `COMPLIANCE_PROXY_API_TOKEN` (shared with CP's BFF proxy + ops curl).

## Architecture references

- `docs/developers/architecture/services/compliance-proxy/compliance-proxy-details-architecture.md`
- `docs/developers/architecture/services/compliance-proxy/compliance-pipeline-architecture.md`
- `docs/developers/architecture/services/ai-gateway/hook-architecture.md`
- `docs/developers/architecture/services/compliance-proxy/forward-header-allowlist-architecture.md`
