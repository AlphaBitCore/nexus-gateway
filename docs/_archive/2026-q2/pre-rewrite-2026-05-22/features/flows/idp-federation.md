# Flow — IdP federation login

## What this flow accomplishes

A user signs into Nexus via an external IdP (Okta / Azure AD / Google / OIDC); if first-time, a Nexus user is JIT-provisioned; an OAuth+PKCE bearer is issued.

## Actors

User (in browser) · External IdP · CP authserver · CP admin API · IAM evaluator.

## Sequence (OIDC variant — shipped)

1. **User → Nexus admin UI sign-in page** → clicks "Sign in with Okta" (per IdP config).
2. **CP authserver** redirects browser to IdP's `authorization_endpoint` with `response_type=code`, Nexus-side `state`, and PKCE. Begin endpoint: `GET /authserver/oidc/begin`.
3. **User authenticates at IdP**, consents.
4. **IdP redirects** to Nexus `/authserver/oidc/callback?code=...&state=...`.
5. **CP authserver** validates `state` → exchanges `code` at IdP `token_endpoint` → receives ID token + access token.
6. **CP JWT verifier** validates the ID token: JWKS-fetched signature (RS256), `iss` whitelist, `aud` match, time-window (`iat`/`nbf`/`exp`), and non-empty `sub` (ghost-principal defence).
7. **CP** maps claims:
   - `email` → Nexus user email.
   - `name` / `given_name` / `family_name` → display name.
   - Configured group/role claim → initial IAM-group membership (e.g., IdP group `nexus-admins` → IAM group `NexusAdmin`).
8. **JIT** — if the user doesn't exist, `FederatedStore.JITProvisionUser` creates it (`source='oidc'`, `canAccessControlPlane=false` initially). Lands in the configured default org. Assigns initial IAM-group memberships.
9. **CP authserver** issues a Nexus OAuth+PKCE bearer token to the browser via `/oauth/token`.
10. **Browser** stores the bearer; subsequent admin API calls use it.

## SAML variant (planned)

**Planned**: SAML AuthnRequest / signed-assertion validation. The IdP type enum supports `local | oidc | saml`, but the SAML runtime handler is not yet implemented — see `docs/developers/roadmap.md` for the queue.

## Failure modes

- **`state` mismatch** — CSRF protection rejects with a generic error; user retries.
- **JWT signature invalid** — JWKS rotated unexpectedly; the verifier's stale-while-revalidate cache + kid-miss refresh handle short rotations automatically; if still failing, surface to admin.
- **Empty `sub`** — verifier rejects as malformed (ghost-principal defence); IdP misconfiguration.
- **Claim mapping missing** — IdP did not include the expected claim; user lands in default org with default IAM-group memberships; admin reviews via the Users page and adjusts.
- **IdP disabled mid-session** — existing bearer still works until expiry; new sign-ins fail; user falls back to Nexus Local if break-glass.
- **Allowlist-mode IdP** — user not on allowlist → `user_not_provisioned` response; admin adds them.

## Verification

```bash
# Local dev simulation:
# (Requires a configured IdP; see docs/operators/ops/runbooks/idp-config-*.md when written)

# Confirm a JIT-provisioned user landed in the DB after a successful OIDC login:
docker exec postgres psql -U postgres -d nexus_gateway \
  -c "SELECT id, email, source, \"organizationId\", \"createdAt\" FROM \"NexusUser\" WHERE source='oidc' ORDER BY \"createdAt\" DESC LIMIT 5"
```

There is no dedicated `admin:user.jit_provisioned` admin-audit event today; observability of JIT runs through the `NexusUser` row (`source='oidc'`, `createdAt`) plus standard login-audit. A dedicated JIT-provisioning audit event is queued — see `docs/developers/roadmap.md`.

## References

- `docs/developers/architecture/services/control-plane/idp-sso-architecture.md` — full federation flow.
- `docs/developers/architecture/services/control-plane/iam-identity-architecture.md` §7 — JIT + managed policies.
- `docs/developers/architecture/services/control-plane/tenancy-architecture.md` §7 — JIT landing zone.
- `docs/developers/architecture/services/control-plane/jwt-verifier-architecture.md` — ID token validation order.
- `docs/users/features/cp-ui/iam.md` — Identity Providers admin surface.
- `feedback_sp_idp_positioning` (memory) — binding terminology.
