# Flow — Agent enrollment

## What this flow accomplishes

A workstation goes from "no agent" to "fully enrolled and reporting Thing", with mTLS to Hub and SSO-signed-in user.

## Actors

Admin · CP · Hub · Agent installer · End-user.

## Sequence

1. **Admin → CP UI → Devices → Device Auth → Issue Enrollment Token** → choose org / default role / OS constraint → token plaintext shown ONCE.
2. **Admin → end-user (out of band)** — share the token + agent installer link.
3. **End-user installs the agent** — macOS `.pkg`, Windows `.msi`, Linux `.deb` / `.rpm`.
4. **Agent first boot** → generates ECDSA P-256 keypair → stores private key in platform keystore (Keychain / Credential Manager / libsecret).
5. **Agent → Hub `POST /api/hub/enrollment/redeem`** with `{ token, csr_pem }`.
6. **Hub** validates token (hash lookup, single-use, expiry, OS constraint) → validates CSR → signs with Hub CA → inserts `thing` row (type `Agent`) with assigned `thing_id` → returns `{ device_cert_pem, intermediate_chain_pem, thing_id, hub_ca_fingerprint }`.
7. **Agent** stores cert + fingerprint → establishes mTLS WebSocket to Hub.
8. **Hub → change-signal** new agent's shadow → agent pulls config (hook configs, exemptions, interception domains, kill-switch state, agent_settings).
9. **End-user opens the agent UI** → click "Sign in" → browser opens → OAuth+PKCE → external IdP (if configured) → JIT user provision if first-time → CP issues bearer for the agent UI.
10. **Agent UI shows: Connected, Signed in as Alice, Recent activity: 0 events** (now ready to intercept).
11. **CP UI Devices** lists the new device with status `online`, version, last-seen.

## Failure modes

- **Token expired or already redeemed** — 410 / 409 from Hub. Admin re-issues.
- **CSR invalid** — 400. Agent regenerates and retries.
- **Hub CA missing** — admin escalation; usually a Hub mis-deploy.
- **macOS NE extension not approved** — system prompt; user must allow in System Settings. Cross-ref `agent-ne-fail-open-architecture.md` (binding fail-open contract).
- **SSO failure** — user sees actionable error linking to admin's IdP config.
- **Platform keystore unavailable** — agent surfaces in Health & Diagnostics; cannot persist device key safely.

## Verification

```bash
# 1) Admin issues token (token plaintext printed by CP)
cp_curl -X POST /api/admin/enrollment-tokens -d '{"org_id":"...", ...}'

# 2) Run agent on a fresh machine; capture the redemption log line.

# 3) Confirm thing row in DB:
docker exec postgres psql -U postgres -d nexus_gateway \
  -c "SELECT thing_id, status FROM thing WHERE thing_type='Agent' ORDER BY created_at DESC LIMIT 1"

# 4) Confirm agent UI shows: Connected + Cert valid + Recent activity panel.
```

## References

- `docs/developers/architecture/services/agent/agent-enrollment-architecture.md` — full cryptographic flow.
- `docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md` — macOS NE safety.
- `docs/developers/architecture/services/control-plane/idp-sso-architecture.md` — user-side SSO.
- `docs/developers/architecture/cross-cutting/foundation/multi-endpoint-coordination-architecture.md` §5 — flow diagram.
- `docs/users/features/cp-ui/devices.md` — Devices admin surface.
- `docs/users/features/agent-ui/dashboard.md` + `about.md` — what the user sees on success.
- `reference_agent_pkg_distribution` (memory) — current PKG distribution channel.
