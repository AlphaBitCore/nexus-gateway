# Security Secrets Handling

*Audience: operators deploying Nexus Gateway and contributors adding new configuration variables.*

All secrets in Nexus Gateway are environment variables — no secret field may appear in any YAML file committed to the repository. This is a hard binding, not a convention. Every secret has a corresponding entry in [`.env.example`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/.env.example); services read values via `os.Getenv` or `bootenv`. This page explains the policy, the `[MUST MATCH]` cross-service contract, and the deployment patterns for local and production environments.

---

## The env-only rule

YAML files carry only service-shape and non-secret tunings: ports, timeouts, feature flags, log levels, allowlists. The following classes of value must never appear in committed YAML:

- Auth tokens (internal-service tokens, API bearer tokens)
- HMAC keys (virtual key hashing, admin key hashing)
- Credential-encryption keys, passphrases, and salts
- Database passwords
- Any value that, if read from version control, would grant access to a live system or allow decryption of stored data

Adding a new YAML `secret` / `password` / `token` / `key` field requires explicit approval. The canonical reference is [`CLAUDE.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CLAUDE.md) "Secrets are env-only — never yaml".

## The [MUST MATCH] cross-service contract

Several secrets must be identical across multiple services. Drift between consumers is the most common source of inter-service 403 errors. These variables are tagged `[MUST MATCH]` in `.env.example`:

| Variable | Services | Effect of mismatch |
|---|---|---|
| `INTERNAL_SERVICE_TOKEN` | All 4 server services | Hub rejects Thing registration with 401 |
| `ADMIN_KEY_HMAC_SECRET` | Control Plane + AI Gateway | VK lookups return "key not found" (hash mismatch) |
| `CREDENTIAL_ENCRYPTION_KEY` | Control Plane + AI Gateway | Credential decryption fails silently or with error |
| `CREDENTIAL_KEY_MAP` | Control Plane + AI Gateway | Rotated credentials unreadable by one service |
| `COMPLIANCE_PROXY_API_TOKEN` | Control Plane + Compliance Proxy | Admin proxy operations return 401 |

When updating any `[MUST MATCH]` variable, update it in all consuming services simultaneously — a staggered rollout leaves one service unable to authenticate to another.

## Secret catalog

The full catalog of environment variables, their scope, and their purpose is maintained in [`.env.example`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/.env.example). Key entries by category:

**Inter-service authentication**

- `INTERNAL_SERVICE_TOKEN` — bearer on Hub WebSocket/HTTP calls and `X-RS-Token` on classification endpoints.
- `ADMIN_KEY_HMAC_SECRET` — HMAC-SHA256 key for hashing virtual key and admin API key bytes before DB storage.

**Credential encryption**

- `CREDENTIAL_ENCRYPTION_KEY` — AES-256 master key, 64 hex chars. Generate with `openssl rand -hex 32`.
- `CREDENTIAL_KEY_MAP` — optional multi-key rotation map (`"v1:<hex64>,v2:<hex64>"`).
- `CREDENTIAL_ENCRYPTION_PASSPHRASE` + `CREDENTIAL_ENCRYPTION_SALT` — alternative passphrase-derived mode (Control Plane only).

**Runtime API tokens**

- `COMPLIANCE_PROXY_API_TOKEN` — bearer for compliance-proxy `/runtime/*` endpoints.
- `AI_GATEWAY_API_TOKEN` — bearer for AI Gateway `/runtime/*` admin endpoints (not shared).

**Infrastructure**

- `DATABASE_URL` — PostgreSQL connection string; carries password.
- `REDIS_PASSWORD` — Valkey/Redis auth (if auth is enabled).

## Deployment patterns

### Local development

The repo root `.env` file (gitignored) is loaded automatically by `bootenv.LoadFromRepoRoot()` at service startup. Copy `.env.example` to `.env` and replace `CHANGE_ME_*` placeholders:

```bash
cp .env.example .env
# edit .env — set DATABASE_URL, INTERNAL_SERVICE_TOKEN, ADMIN_KEY_HMAC_SECRET,
# CREDENTIAL_ENCRYPTION_KEY, and any other REQUIRED values
```

No `source .env` is needed; `bootenv` handles loading. Precedence: existing process environment beats `.env` values, so `MY_VAR=x ./svc` overrides the file for that run.

### Production (systemd)

Secrets are injected via `EnvironmentFile=` in each service's systemd unit:

```ini
[Service]
EnvironmentFile=/etc/nexus-gateway/env
ExecStart=/usr/local/bin/nexus-hub ...
```

The env file at `/etc/nexus-gateway/env` is mode `0600`, owned by the service user, and managed outside version control. No `.env` file exists in the working directory in production.

### Production (Kubernetes)

Secrets are mounted via a Kubernetes `Secret` object referenced from the pod spec:

```yaml
envFrom:
  - secretRef:
      name: nexus-secrets
```

The Secret is managed via the cluster's secrets management system (Vault, Sealed Secrets, etc.), not committed to the values file.

## Adding a new secret

When a new secret variable is needed:

1. Add it to `.env.example` with a `CHANGE_ME_*` placeholder and a comment explaining its purpose and any `[MUST MATCH]` relationship.
2. Have all consuming services read it via `os.Getenv(...)` or `bootenv`.
3. Do not add a YAML field for the secret value.
4. If the variable must match across services, add the `[MUST MATCH]` tag in `.env.example` and note the consuming services.

---

## Canonical docs

- [`.env.example`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/.env.example) — every environment variable, its scope, and its `[MUST MATCH]` relationship
- [`docs/developers/workflow/local-dev-debugging.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/local-dev-debugging.md) — `bootenv` contract, `tests/.env.<target>` loader contract, and environment variable operational detail
- [`CLAUDE.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/CLAUDE.md) — "Secrets are env-only — never yaml" binding rule

**Adjacent wiki pages**: [Security Credential Storage](Security-Credential-Storage) · [Security Threat Model](Security-Threat-Model) · [Deployment Environment Variables](Deployment-Environment-Variables) · [Troubleshooting First Run](Troubleshooting-First-Run)
