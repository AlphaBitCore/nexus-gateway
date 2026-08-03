---
updated: 2026-07-25
---

# Container deployment

How to run Nexus Gateway from the published container images: the
`docker compose` quickstart, and the self-contained Linux tarball for
systemd-based installs. The design rationale — image inventory, the
Vectorscan instruction-set constraint, the release gates, and the tag
contract — is captured in
[`docs/developers/architecture/cross-cutting/deployment/container-image-architecture.md`](../../developers/architecture/cross-cutting/deployment/container-image-architecture.md).
This doc is the operational source of truth for `docker/**`, `deploy/**`,
and `scripts/release/**`.

## When to use this

- Standing up a Nexus Gateway instance from published images rather than
  building from source.
- Evaluating the product with the two-tier demo seed before connecting real
  provider credentials.
- Deploying to a host that runs systemd rather than containers, via the
  Linux tarball.

## Quickstart (Docker Compose)

### Prerequisites

- Docker with the Compose plugin (`docker compose version`).
- `openssl` on the host (used by `init-secrets.sh` to generate secrets).
- Outbound access to `ghcr.io` (or `docker.io`) to pull images.
- Outbound access to `registry.npmjs.org`, once. The migrator ships thin and
  installs its Node dependencies on the first run for a given image (see
  below); an egress allowlist built from the two registries above will hang and
  then fail that install.

### Two commands, not one

```bash
cd deploy
./init-secrets.sh
docker compose up -d
```

Then open <http://localhost:8080>.

The first `docker compose up` installs the migrator's Node dependencies —
it ships as a thin image and installs them at first run into a named volume
(`migrator-deps`), which needs network access to the npm registry that one
time. Every later `up` reuses the volume and skips the install; it reruns
only after an image upgrade changes the dependency lockfile. The migrator is
a one-shot container: it applies the schema, seeds, rotates the seed's public
credentials, and exits.

This is deliberately two commands. Five values —
`INTERNAL_SERVICE_TOKEN`, `HUB_CONFIG_TOKEN`, `ADMIN_KEY_HMAC_SECRET`,
`CREDENTIAL_ENCRYPTION_KEY`, `COMPLIANCE_PROXY_API_TOKEN` — must be
identical across the four services and are secrets by the project's
env-only-secrets rule (no secret may live in a committed yaml). A true
one-command quickstart would require either baking default values into a
public image, which scanners find within hours of a push, or reading
secrets from a file shipped in the repo, which is the same problem with
extra steps. `init-secrets.sh` generates all of them with `openssl rand`,
writes `deploy/.env` (mode `600`, refuses to overwrite an existing one), and
prints the generated administrator password once.

Only the UI (`:8080`, which also proxies the admin API and OAuth
endpoints) and the AI Gateway (`:3050`) are published to the host. Postgres,
Valkey, and NATS stay on the compose-internal network. The optional
compliance profile adds a third published port — see "The compliance proxy
profile" before you size a host firewall from this list.

**If one of those ports is already taken**, set `NEXUS_UI_PORT`,
`NEXUS_GATEWAY_PORT` or `NEXUS_PROXY_PORT` in `.env` rather than editing the
compose file's `ports:`. The console's origin follows the UI port, and that
origin is load-bearing: the migrator registers `<origin>/auth/callback` as an
OAuth redirect URI and `/oauth/authorize` rejects any redirect_uri it was not
given, so a published port that disagrees with the origin means nobody can log
in. Setting the variable moves both together. For a deployment behind a reverse
proxy or a real hostname, set `CONTROL_PLANE_PUBLIC_URL` instead — it overrides
the whole origin, port included.

### First login

Sign in at <http://localhost:8080> with:

```
admin@nexus.ai / <the password init-secrets.sh printed>
```

The administrator email is fixed by the seed and is not configurable.

### What the demo seed contains

By default (`SEED_DEMO=true`, the image's default) `db-migrator` loads two
tiers of **sample data**, not anything meant to carry production traffic:

- **Reference data** — the provider/model catalog, default routing rules,
  and IAM policies/groups every deployment needs to function.
- **A demo tenant** — 10 organizations, 6 projects, **13 sample users
  (login), 12 virtual keys, and 5 admin API keys** (two of them members of
  the `super-admins` IAM group), plus 5 placeholder provider credentials
  named `openai-prod` / `anthropic-prod` / `google-prod` / `deepseek-prod` /
  `moonshot-prod`.
  Every one of those plaintexts is deterministically derivable from the
  row's fixture id alone (`nexus-demo` for every user password,
  `nvk_demo_<id[:8]>` for every virtual key, `nak_demo_<id[:8]>` for every
  admin key) — and every fixture id is public in the OSS repository. They
  exist so a fresh `docker compose up` has something to look at
  immediately, not because the seed's own plaintexts are safe to leave in
  a reachable deployment; see "Credential rotation" below for how the
  migrator neutralizes them.

The demo tier is **created once and then yours**. `db-migrator` runs on every
`docker compose up`, but it only inserts the demo rows a database does not
have; it never writes over one that is already there. So a demo key you
revoked stays revoked, and a real provider key you paste into one of those
pre-wired `*-prod` credential rows survives every later upgrade. The
reference tier behaves the opposite way by design — the catalog is
maintained by the release, so each run reasserts it.

Two consequences worth knowing. **Disable demo rows rather than deleting
them**: absence is the only signal the seed has, so a deleted row is
indistinguishable from one that was never added and comes back on the next run
(carrying a fresh secret, which the rotation step then replaces). And the demo
tier removes the reference catalog's own default application-VK quota policy,
because it ships its own — that is the one row it deletes, identified by
identity, so quota policies you create are not affected.

Set `SEED_DEMO=false` in `deploy/.env` to load only the reference tier and skip
the demo tenant. It must be set before the first `docker compose up` against a
given database: the demo rows are not removed by a later run.

## Credential rotation (binding on first run)

Two separate rotation steps run inside `docker/db-migrator/entrypoint.sh`,
covering two separate sets of seeded credentials:

**1. The bootstrap pair.** The bootstrap seed ships `admin@nexus.ai` with
the password `nexus-demo` and mints the system-assistant Virtual Key ("Chat
with Nexus") from a deterministic plaintext — both public in the OSS
repository. The entrypoint replaces **both** on every run, before any port
is published: it hashes `NEXUS_ADMIN_PASSWORD` and overwrites the seeded
admin's password hash, and it hashes `NEXUS_ASSISTANT_SYSTEM_VK` (under
`ADMIN_KEY_HMAC_SECRET`) and overwrites the assistant key's stored hash.
The entrypoint refuses to start — `db-migrator` exits before touching the
schema — if `NEXUS_ADMIN_PASSWORD`, `NEXUS_ASSISTANT_SYSTEM_VK`, or
`ADMIN_KEY_HMAC_SECRET` is unset.

**2. The demo tier's own credentials.** The demo tenant (`SEED_DEMO=true`,
the default) ships its own 13 `NexusUser` rows, 12 `VirtualKey` rows, and 5
`AdminApiKey` rows — see "What the demo seed contains" above — each with a
plaintext derivable from its fixture id alone. Those are **not** the same
rows the bootstrap pair covers, and were left un-rotated in earlier
releases: a published quickstart exposed a working super-admin API key and
a dozen working virtual keys to anyone who read the repository.
`docker/db-migrator/rotate-demo-secrets.mjs` closes that gap: it selects
each row **by evidence**, never an enumerated id list, so a demo fixture
added later is covered automatically. A `VirtualKey`/`AdminApiKey` row is
rotated only if its stored hash still verifies against that row's own
public plaintext formula (`nvk_demo_<id[:8]>` / `nak_demo_<id[:8]>`) — both
plaintexts are server-random, so an operator cannot forge that collision.
A `NexusUser` row additionally requires its `organizationId` to be a member
of the demo-org set, derived at runtime from
`tools/db-migrate/seed/fixtures/demo/Organization.json` (never a hardcoded
id list): password-hash verification against the public seed password
`nexus-demo` alone is not enough evidence for that table, because an
operator can deliberately set their OWN account's password to the literal
string `nexus-demo` — the org condition is what keeps that operator's
account from being mistaken for a demo row. Every selected row is
re-stamped with a plaintext derived deterministically from
`ADMIN_KEY_HMAC_SECRET` and the row's own id
(`nexus-ami/scripts/rotate-demo-secret.js`, shared with the appliance's
credential-hashing helpers). Deterministic means idempotent:
re-running the migrator (the next `docker compose up`) reproduces the exact
same rotated credentials instead of invalidating ones an operator was
already handed — it does not touch the database with a fresh random value
on every run the way the bootstrap pair's admin-password rotation does.

The migrator's log lists **which** rows were rotated (name, kind, id) but not
the values: its stdout is the container log, which Docker persists and any log
collector ships onward, and two of these row classes carry super-admin
authority. Because the derivation is deterministic, nothing is lost — recover
any one of them on demand from the same helper the migrator used, using the
name/kind/id the log printed:

```bash
cd deploy
docker compose run --rm --entrypoint node \
  -e ROTATE_KIND=password \
  -e ROTATE_ID=<row id from the migrator log> \
  db-migrator /opt/nexus-scripts/rotate-demo-secret.js
```

`ROTATE_KIND` is `password`, `virtual-key`, or `admin-key`. The first
tab-separated field of the output is the plaintext. This reads
`ADMIN_KEY_HMAC_SECRET` from the compose environment, so it only works where
`deploy/.env` is readable — which is the point.

**If you supply your own `.env`** instead of running `init-secrets.sh`, it must
carry the eleven variables the compose file interpolates without a default
(`NEXUS_REGISTRY`, `CONTROL_PLANE_PUBLIC_URL` and `SEED_DEMO` fall back to
sensible values if omitted; nothing else does):

- the five `[MUST MATCH]` secrets above;
- `NEXUS_ADMIN_PASSWORD` and `NEXUS_ASSISTANT_SYSTEM_VK`, without which the
  migrator refuses to start and every dependent service stays blocked on
  `service_completed_successfully`;
- `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB`, which appear both in the
  database container's own environment and inside every service's
  `DATABASE_URL`;
- `NEXUS_VERSION`, the image tag.

Compose substitutes a blank string for anything missing rather than failing: it
prints one `variable is not set. Defaulting to a blank string` warning per
variable and then carries on, so an absent `NEXUS_VERSION` surfaces as `invalid
reference format` on an image reference ending in a bare colon, and absent
`POSTGRES_*` as a database that will not initialise. Read the warnings, not the
error — the error names none of them. `deploy/.env.example` lists all eleven.

`scripts/release/smoke-compose.sh` — the release gate for this whole form
factor — asserts every rotation above landed in both directions: the public
`nexus-demo` admin password, the public deterministic assistant key, and
the demo tier's public admin key / virtual key / user password are each
rejected with `401`, and the freshly rotated values each authenticate. A
rotation that silently did nothing is the one failure mode this form
factor cannot ship with, so every check is paired rather than one-sided.

## Choosing an image tag: default or `-avx2`

Every image ships two amd64 variants: the default multi-arch tag (built for
`x86-64-v2`) and a `-avx2` tag (amd64-only, built for `core-avx2`). arm64
has only the default tag.

Check before pulling `-avx2`:

```bash
grep -o avx2 /proc/cpuinfo | head -1
```

Output `avx2` means the CPU supports it. **No output means it does not** —
pulling the `-avx2` tag on such a host produces a `SIGILL` (illegal
instruction) the first time the gateway scans a request, not a graceful
error. **If you are unsure, use the default tag** — it runs everywhere the
`-avx2` tag does, at a small throughput cost on the content-scanning path.
`deploy/docker-compose.yml` pulls the default tag unless `NEXUS_VERSION` in
`.env` is explicitly set to a `-avx2` value (e.g. `1.3.0-avx2`).

## The compliance proxy profile

```bash
docker compose --profile compliance up -d
```

**This publishes a third host port.** `compliance-proxy` binds `3128` on all
interfaces, so a host firewall written from the two-port list above will not
filter it. Its only built-in client gate is a source-IP allowlist covering the
RFC1918 ranges (`10/8`, `172.16/12`, `192.168/16`); anything outside those
ranges is refused, but everything inside them is accepted. Restrict `3128` to
the clients that should be intercepted, or bind it to a private interface, before
enabling this profile on a host with a public address.

Works out of the box with a **generated self-signed CA**. `compliance-proxy`
requires its MITM CA certificate and key at `/etc/compliance-proxy/ca.crt`
and `/etc/compliance-proxy/ca.key`, and exits on startup if they are absent.
`./init-secrets.sh` generates this pair as `deploy/compliance-ca/{ca.crt,ca.key}`
(self-signed EC P-256, 10-year validity — the same shape
`nexus-ami/scripts/first-boot-ca.sh` generates for the AMI appliance) the
first time it runs, and only if the pair is not already present — re-running
`init-secrets.sh` never invalidates a CA an operator's clients already
trust. `deploy/docker-compose.yml` mounts `deploy/compliance-ca/` read-only
into the `compliance-proxy` container at the path its config expects.

**Replace the generated CA with your own** if you need a CA your
organization already trusts, or a longer/shorter validity period: stop the
stack, overwrite `deploy/compliance-ca/ca.crt` and `deploy/compliance-ca/ca.key`
with your own PEM-encoded EC (or RSA) CA cert/key pair, then
`docker compose --profile compliance up -d` again. `compliance-proxy` only
ever reads this pair; it never regenerates or rotates it.

**Import `deploy/compliance-ca/ca.crt` into every client the proxy
intercepts** — any agent, browser, or SDK that sends traffic through
`compliance-proxy` on `:3128` must trust this CA, or every intercepted TLS
connection fails certificate verification. `deploy/compliance-ca/` is
gitignored (it holds a private key); do not commit it.

## What the stack keeps on disk

Six named volumes, all removed by `docker compose down -v`:

| Volume | Service | Holds | Losing it means |
|---|---|---|---|
| `pgdata` | postgres | the database | everything |
| `natsdata` | nats | JetStream state | audit events published but not yet consumed |
| `valkeydata` | valkey | the cache's RDB snapshot | admin sessions invalidated, IAM cache cold, quota counters reset mid-period |
| `gateway-audit-spool` | ai-gateway | audit records that overflowed the in-memory buffer | traffic/billing rows the pipeline still owed |
| `proxy-audit-spool` | compliance-proxy | the same, for intercepted traffic | compliance records the pipeline still owed |
| `migrator-deps` | db-migrator | the migrator's `node_modules` | only time — the next `up` reinstalls |

The two audit spools are the ones worth sizing deliberately. Each is bounded by
its own service's configuration rather than by the volume — `audit.spoolMaxTotalMb`
(512 MB) for the gateway, `audit.ndjson.maxTotalSizeMB` (1000 MB) for the proxy —
so they cannot fill the host on their own. What that cap buys differs by service,
because their overflow policies differ:

- **ai-gateway** defaults to `spillblock`: overflow goes to the spool first, and
  the request path is back-pressured only when the spool is *also* full — at
  which point it parks and retries rather than dropping, so ingest self-throttles
  to the rate the recovery sweeper can drain. The cap is therefore a throughput
  governor under a sustained NATS or database outage: raise it
  (`AI_GATEWAY_AUDIT_SPOOL_MAX_TOTAL_MB`) if you would rather absorb a longer
  outage than throttle. With no writable spool the mode downgrades to `block` at
  startup — still no loss, but nothing absorbs a burst.
- **compliance-proxy** resolves the same policy through the same contract and
  also defaults to `spillblock`. Its spool matters for a different reason: it
  sits in an intercepted TLS path, so when the mode degrades to `block` — which
  is what happens with no writable spool — the back-pressure lands on somebody's
  connection rather than on an API client that can retry. Here the cap bounds how
  much of an outage you can absorb before that starts.

## Upgrading

Change `NEXUS_VERSION` in `.env` (e.g. `1.3.0` → `1.4.0`), then:

```bash
docker compose pull
docker compose up -d
```

`db-migrator` reruns as part of `up -d` because its image reference
changed. It reapplies the schema with `prisma db push --accept-data-loss`,
reasserts the reference catalog, and re-asserts the admin password and
assistant key against the values already in `.env` (safe no-ops if `.env` is
unchanged). It does **not** rewrite demo-tier rows — see "What the demo seed
contains" — so credentials you disabled or filled in stay as you left them.
If the seed itself fails partway, the migrator still runs every credential
rotation before exiting non-zero, so a seed failure does not leave a public
seed credential live. If a **rotation** fails, it says so explicitly — `FATAL:
at least one credential rotation failed … may still be live in this database` —
and the deployment must not be exposed until a run completes cleanly. Either
way the services stay on their previous containers, because the new ones wait
for a migrator that exited zero. `--accept-data-loss`
means a schema change that removed a field can silently drop that column's
data — take a `pg_dump` of the `postgres` volume before an upgrade that
crosses a major version.

## Where the secrets live

Everything `init-secrets.sh` generates — the five `[MUST MATCH]` tokens,
`POSTGRES_PASSWORD`, `NEXUS_ADMIN_PASSWORD`, and
`NEXUS_ASSISTANT_SYSTEM_VK` — lives in `deploy/.env` (mode `600`,
gitignored). There is no other copy on the host.

**Regenerating `CREDENTIAL_ENCRYPTION_KEY` makes every already-stored
provider credential unreadable.** It is the AES key every provider
credential (real ones added through the admin UI, and the demo tenant's
placeholders) is encrypted under before it reaches Postgres. This form
factor wires only the single `CREDENTIAL_ENCRYPTION_KEY` value, not the
versioned `CREDENTIAL_KEY_MAP` rotation mechanism `.env.example` documents
for other deployment paths — there is no non-disruptive rotation here. That
is why `init-secrets.sh` refuses to overwrite an existing `.env`: the only
way to change this key is to delete `.env` deliberately, regenerate it, and
re-enter every provider credential afterward.

## Linux tarball (systemd install)

### Prerequisites

- **glibc 2.34 or newer.** Only libstdc++ and libgcc are statically linked; the
  binaries still link the host's glibc dynamically, and the highest versioned
  symbol they require is `GLIBC_2.34` (measured with `objdump -T` on the release
  build — `build-tarball.sh` fails if a toolchain change raises it above the
  figure documented here). Debian 12+, Ubuntu 22.04+, RHEL 9+ and Amazon Linux
  2023 satisfy it. **RHEL 8 (2.28), Ubuntu 20.04 (2.31) and Amazon Linux 2
  (2.26) do not** — every unit exits immediately with
  `/lib64/libc.so.6: version 'GLIBC_2.34' not found`.
- systemd, and a reachable PostgreSQL, Valkey/Redis and NATS (the tarball
  carries none of them).
- `openssl`, for generating the compliance CA in step 4.

`scripts/release/build-tarball.sh` (or a GitHub Release download) produces
`nexus-gateway-<version>-linux-<arch>.tar.gz`:

```
bin/       nexus-hub, control-plane, ai-gateway, compliance-proxy
           (statically linked against libstdc++/libgcc — no distro C++
           runtime dependency)
ui/        the built control-plane-ui static assets
systemd/   nexus-hub.service, control-plane.service, ai-gateway.service,
           compliance-proxy.service
config/    nexus-hub.yaml, control-plane.yaml, ai-gateway.yaml,
           compliance-proxy.yaml (prod-shape templates), env.example
           (the full repository-root env-var contract)
```

### What it does not include

Unlike the compose quickstart, the tarball has no `init-secrets.sh` and no
`db-migrator`. Bring your own Postgres, Valkey, and NATS, and your own
reverse proxy / TLS termination for the UI — `deploy/nginx/nexus-ui.conf` is
a usable starting point, but it proxies to compose service names and needs
its upstreams changed to `127.0.0.1:<port>` for a single-host systemd
install.

### Install steps

1. Create the `nexus` system user and the directories every unit's
   `ProtectSystem=strict` sandbox needs to already exist (systemd cannot
   bind-mount a `ReadWritePaths=` entry that is missing — the unit fails to
   start, not just the write):
   - `/opt/nexus/{bin,ui}`, `/etc/nexus-gateway/` — binaries and config.
   - `/var/log/nexus` — every unit's `ReadWritePaths`.
   - `/var/lib/nexus` — `ai-gateway.service`'s additional `ReadWritePaths`;
     backs `audit.spoolDir` in `ai-gateway.yaml`
     (`/var/lib/nexus/audit-spool`), the NDJSON durability fallback used
     when the NATS publish buffer overflows.
   - `/var/lib/nexus-proxy` — `compliance-proxy.service`'s additional
     `ReadWritePaths`; backs `audit.ndjson.dir` in `compliance-proxy.yaml`
     (`/var/lib/nexus-proxy/audit-spool`), the same kind of fallback.
   - `/etc/compliance-proxy` — the MITM CA generated in step 4.

   All of the above owned by `nexus:nexus` (or root:nexus with group
   read/write, matching step 4's CA permissions).
2. Copy `bin/*` to `/opt/nexus/bin/`, `ui/` to wherever your reverse proxy
   serves static files from, and `config/*.yaml` to
   `/etc/nexus-gateway/<service>.yaml` (the units expect exactly that path).
3. Generate the five `[MUST MATCH]` secrets the same way `init-secrets.sh`
   does (`openssl rand -hex 32` each) plus `DATABASE_URL` / `REDIS_ADDRS` /
   `NATS_URL` for your own infrastructure, and write them as `KEY=VALUE`
   lines to `/etc/nexus-gateway/env` (referenced by every unit's
   `EnvironmentFile=`) — `config/env.example` lists every variable the four
   services read. Three more variables are REQUIRED — each service's config
   validation refuses to start without them, the same way it refuses to
   start without the `[MUST MATCH]` secrets — and are easy to miss because
   `deploy/docker-compose.yml` sets them for you; a hand-written systemd
   `env` file has no such default:
   - `NEXUS_HUB_ID=<a value unique to this install>` — this instance's
     identifier when it registers with the Hub. The shipped `nexus-hub.yaml`
     pins `hub.id` to an explicit empty string precisely so a missing
     override fails config validation rather than silently reusing the host
     name. Compose sets a fixed `nexus-hub-quickstart`; pick your own for a
     systemd install (the hostname is a reasonable default).
   - `NEXUS_HUB_PUBLIC_URL`, `CONTROL_PLANE_PUBLIC_URL`,
     `AI_GATEWAY_PUBLIC_URL`, `COMPLIANCE_PROXY_PUBLIC_URL` — each service
     reads only its own `<SERVICE>_PUBLIC_URL` and ignores the others, so
     setting all four in the shared `env` file is harmless. Each is the
     externally-reachable base URL the admin UI displays for that service
     and hands to external callers (an agent, a browser, a virtual-key
     holder). **Assumption below, adjust to your own topology:**
     mirroring how `deploy/docker-compose.yml` splits its four services —
     `nexus-hub` and `control-plane` sit entirely behind your reverse proxy
     for the UI/API (one external hostname), while `ai-gateway` and
     `compliance-proxy` are typically reachable directly on their own ports
     (the address a virtual-key holder or an agent actually uses):
     ```
     NEXUS_HUB_PUBLIC_URL=https://gateway.example.com
     CONTROL_PLANE_PUBLIC_URL=https://gateway.example.com
     AI_GATEWAY_PUBLIC_URL=https://gateway.example.com:3050
     COMPLIANCE_PROXY_PUBLIC_URL=https://gateway.example.com:3128
     ```
   - `MQ_DRIVER=nats` — `compliance-proxy.yaml` ships with `mq.driver`
     commented out (must come from the environment); `nexus-hub.yaml`,
     `control-plane.yaml`, and `ai-gateway.yaml` each default it to `nats`
     in-yaml, which is why this is easy to miss until `compliance-proxy` is
     the service actually being brought up.
4. Provision `compliance-proxy`'s MITM CA. Without it, `compliance-proxy`
   exits on startup — this is the same gap the Compose profile has (see
   above), except here it is two `openssl` commands run once on the host,
   the same pair the AMI appliance generates on first boot
   (`nexus-ami/scripts/first-boot-ca.sh`):
   ```bash
   openssl ecparam -genkey -name prime256v1 -noout \
     -out /etc/compliance-proxy/ca.key
   openssl req -x509 -new -nodes -key /etc/compliance-proxy/ca.key -sha256 -days 3650 \
     -subj "/CN=Nexus Compliance Proxy CA/O=Nexus Gateway" \
     -out /etc/compliance-proxy/ca.crt
   chown root:nexus /etc/compliance-proxy/ca.crt /etc/compliance-proxy/ca.key
   chmod 0640 /etc/compliance-proxy/ca.crt /etc/compliance-proxy/ca.key
   ```
   This CA signs leaf certificates for every upstream provider domain
   `compliance-proxy` intercepts; agents that go through it must trust
   `ca.crt`. Skip this step (and drop `compliance-proxy` from step 6's
   `systemctl enable --now` list) if you do not need the compliance proxy —
   it is the only one of the four services with an install-time
   prerequisite beyond the shared secrets.
5. Run the schema push, seed, and credential rotation once against your
   database — the published `db-migrator` image implements this and is the
   supported way to do it, even outside Compose:
   ```bash
   docker run --rm \
     -e DATABASE_URL=... -e NEXUS_ADMIN_PASSWORD=... \
     -e NEXUS_ASSISTANT_SYSTEM_VK=... -e ADMIN_KEY_HMAC_SECRET=... \
     -e CREDENTIAL_ENCRYPTION_KEY=... \
     -e CONTROL_PLANE_PUBLIC_URL=https://gateway.example.com \
     -e SEED_DEMO=false \
     ghcr.io/alphabitcore/db-migrator:<version>
   ```
   `CONTROL_PLANE_PUBLIC_URL` is required — the migrator registers
   `<that origin>/auth/callback` as an allowed OAuth redirect URI, and without
   it `/oauth/authorize` rejects every login attempt, so the entrypoint refuses
   to run at all rather than leave a console nobody can sign in to. Set it to
   the address administrators reach this install at. `SEED_DEMO=false` keeps the
   demo tenant (10 sample organizations, 13 logins, 12 virtual keys, 5 admin API
   keys) out of a production install; the entrypoint defaults it to `true` for
   the evaluation quickstart.
6. `cp systemd/*.service /etc/systemd/system/ && systemctl daemon-reload`,
   then `systemctl enable --now nexus-hub control-plane ai-gateway
   compliance-proxy`. The other three retry their Hub registration with
   backoff rather than failing to start, so strict ordering is not required,
   but starting `nexus-hub` first avoids an initial burst of reconnect
   attempts in the logs.

## Common failure modes

| Symptom | Root cause | Fix |
|---|---|---|
| `db-migrator` exits immediately, every other service stays `Created` forever | `NEXUS_ADMIN_PASSWORD` / `NEXUS_ASSISTANT_SYSTEM_VK` / `ADMIN_KEY_HMAC_SECRET` missing from a hand-written `.env` | Set all three (`docker compose logs db-migrator` names which one) |
| `Illegal instruction` / container exits with SIGILL on first request | Pulled a `-avx2` tag on a pre-AVX2 CPU | Switch `NEXUS_VERSION` back to the default (non-`-avx2`) tag |
| `compliance-proxy` restarts in a crash loop under Compose | `deploy/compliance-ca/{ca.crt,ca.key}` missing or unreadable to the container's uid — usually because `.env` (and thus the CA) was created before this pair existed, or the directory was deleted | Rerun `./init-secrets.sh`. It is idempotent: it backfills whatever is missing (the CA and/or `.env`), renormalises the CA's permissions on every run, and leaves key material untouched, exiting `0` either way. If exactly one of `ca.crt` / `ca.key` is present it refuses instead, rather than replacing the survivor with a new pair — restore the missing half, or move the remaining one aside and rerun |
| `compliance-proxy` exits immediately under systemd | Missing `/etc/compliance-proxy/ca.crt` / `ca.key` | Run the tarball install's step 4 (CA generation) |
| `control-plane-ui` exits immediately | An upstream host name in `nexus-ui.conf` did not resolve at nginx startup (nginx resolves `proxy_pass` hostnames once, at config load) | Confirm `control-plane`, `ai-gateway`, and `nexus-hub` are at least `Created` before the UI container starts (compose `depends_on` already enforces this) |
| A tarball-installed unit fails to start with `(code=exited, status=226/NAMESPACE)` | `ReadWritePaths=` in the unit names a directory that does not exist yet — systemd cannot bind-mount a missing path | Create `/var/log/nexus` (every unit) and, for `ai-gateway`/`compliance-proxy`, also `/var/lib/nexus` / `/var/lib/nexus-proxy` (install step 1) before `systemctl enable --now` |
| A systemd-installed service refuses to start, config validation citing a missing public URL (`nexus-hub` citing missing `hub.id`), or `compliance-proxy` citing an unset `mq.driver` | `NEXUS_HUB_ID` / one of the four `*_PUBLIC_URL` vars / `MQ_DRIVER` not set in `/etc/nexus-gateway/env` | These four are easy to miss because `deploy/docker-compose.yml` sets them for you — see install step 3 |

## Related docs

- `docs/developers/architecture/cross-cutting/deployment/container-image-architecture.md`
  — image inventory, the Vectorscan gates, the tag contract, registry model.
- `docker/README.md` — local build commands for these images.
- `deploy/.env.example` — the compose environment-variable contract.
