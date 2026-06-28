---
updated: 2026-05-28
---

# AMI / appliance deployment architecture

Single-box deployment form factor for Nexus Gateway. Packages **all** runtime
dependencies (PostgreSQL 16, Valkey 8 with `valkey-search`, NATS JetStream,
4 Go services, the React UI, and an nginx reverse proxy) into one disk image
managed by systemd. The same artifacts ship as:

| Target | Wrapped by |
|---|---|
| **AWS Marketplace AMI** | `nexus-ami/nexus.pkr.hcl` (Packer + Amazon Linux 2023) |
| **VMware / KVM image** | future — same `install.sh`, different Packer builder |
| **Bare-metal appliance** | future — same `install.sh` invoked from a kickstart / preseed |

This doc is the architecture source of truth for **everything under
`nexus-ami/`**. Any change to a config file, systemd unit, install script,
or first-boot script in that directory MUST update this doc in the same
commit (Code/Doc Lockstep — see `.cursor/rules/code-doc-lockstep.mdc`).

## 1. Why one form factor for AMI + bare-metal

Two distribution channels share the same install logic:

- **Cloud appliance** — AWS Marketplace AMI (initial target). Customer hits
  "Launch", gets a working single-instance Nexus in ~5 minutes.
- **On-prem appliance** — pre-installed disk image / ISO for hardware
  shipped to customer sites (future). Same systemd-managed services, same
  first-boot secret generation.

Containerised / Kubernetes deployment is **out of scope** for this doc. If
the project later ships a Helm chart or container Marketplace listing, that
is a separate architecture (`<future>-container-architecture.md`) with its
own dependency wiring (RDS / ElastiCache / managed MQ).

## 2. Boot sequence (every fresh instance / fresh hardware)

```
1.  cloud-init / kickstart    →  network + ec2-user / nexus shell login
2.  firewalld                 →  open 443, 3128, 22; close everything else
3.  nexus-first-boot.service  →  oneshot, gated by /etc/nexus/.initialized
       ├─ first-boot-secrets.sh  → generate 5 [MUST MATCH] secrets, write
       │                          /etc/nexus/{nexus-hub,control-plane,
       │                          ai-gateway,compliance-proxy}.env
       ├─ first-boot-ca.sh       → generate compliance-proxy MITM CA at
       │                          /etc/compliance-proxy/{ca.crt,ca.key}
       └─ first-boot-db.sh       → start postgresql, wait, prisma db push,
                                   apply schema-extras.sql, seed Tier-A +
                                   bootstrap only (SEED_DEMO=false; no demo rows),
                                   randomise admin password, mint per-instance
                                   system-assistant VK, write
                                   /root/nexus-admin-credentials.txt + /etc/motd
                                   (admin tail guarded by .db-initialized)
4.  postgresql.service        →  After=nexus-first-boot
5.  valkey.service            →  After=nexus-first-boot
6.  nats.service              →  After=nexus-first-boot
7.  nexus-hub.service         →  After=postgresql valkey nats
8.  nexus-control-plane.service  →  After=nexus-hub
9.  nexus-gateway.service     →  After=nexus-hub
10. nexus-proxy.service       →  After=nexus-hub
11. nginx.service             →  After=nexus-control-plane (reverse proxy)
```

`/etc/nexus/.initialized` is the idempotency marker. Removing it triggers a
fresh init on next boot (destructive — generates new secrets, re-seeds DB).
Customers should never touch it.

## 3. Filesystem layout

| Path | Owner | Mode | Contents |
|---|---|---|---|
| `/opt/nexus/bin/` | root:root | 0755 | 4 Go service binaries (immutable, part of AMI) |
| `/opt/nexus/ui/` | root:root | 0755 | Vite-built UI dist (immutable, part of AMI); `downloads/` holds the `build.sh`-cross-compiled nexus CLI (darwin-arm64 / linux-amd64 / windows-amd64) and is the drop-zone for the signed macOS `.pkg` / Windows `.msi` agent installers — served by nginx `location /downloads/` |
| `/opt/nexus/prisma/` | root:root | 0755 | Prisma schema + seed (immutable, part of AMI) |
| `/etc/nexus/` | root:nexus | 0750 | 4 prod-shape `*.config.yaml` + 4 `*.env` + nginx-nexus.conf + `.initialized` marker |
| `/etc/compliance-proxy/` | root:nexus | 0750 | MITM CA cert + key (generated first-boot) |
| `/var/lib/nexus/` | nexus:nexus | 0750 | Service runtime state (agent CA dir, NDJSON spool, file-backed alerting state, `spill/` localfs SpillStore root) |
| `/var/lib/postgresql/data/` | postgres:postgres | 0700 | PostgreSQL data directory (AL2023 dnf default) |
| `/var/lib/valkey/` | valkey:valkey | 0750 | Valkey AOF + RDB |
| `/var/lib/nats/` | nats:nats | 0750 | NATS JetStream file store |
| `/var/log/nexus/` | nexus:nexus | 0750 | Service log files (rotated by logrotate) |
| `/root/nexus-admin-credentials.txt` | root:root | 0600 | Read-once first-boot admin credentials (Marketplace policy: outside `/var/log`, root-only) |

**Spill storage (localfs, no S3).** The appliance deliberately carries no S3
dependency: all four service yamls enable the spillstore with
`backend: localfs`, `root: /var/lib/nexus/spill` (the shared root is what
lets any service resolve refs produced by any other — single host, single
volume). `totalSizeCap` is pinned to 5 GiB and `retentionDays` to 7 because
the localfs defaults (50 GiB / 30 days) exceed the appliance's 30 GB root
volume. The directory is created by `install.sh` and sits inside the
systemd units' `ReadWritePaths=/var/lib/nexus` sandbox allowance.

## 4. Secret generation (`first-boot-secrets.sh`)

Six environment variables MUST be unique-per-instance and identical across
the services that share them (see `.env.example` `[MUST MATCH]` tags):

| Env var | Used by | Generation |
|---|---|---|
| `INTERNAL_SERVICE_TOKEN` | all 4 | `openssl rand -hex 32` |
| `HUB_CONFIG_TOKEN` | control-plane, nexus-hub | `openssl rand -hex 32` (Hub config-write surface; Hub fails closed at boot if unset) |
| `ADMIN_KEY_HMAC_SECRET` | control-plane, ai-gateway | `openssl rand -hex 32` |
| `CREDENTIAL_ENCRYPTION_KEY` | control-plane, ai-gateway, nexus-hub | `openssl rand -hex 32` (AES-256, 64 hex chars; Hub encrypts alert-channel secrets at rest) |
| `COMPLIANCE_PROXY_API_TOKEN` | control-plane, compliance-proxy | `openssl rand -hex 32` |
| `AI_GATEWAY_API_TOKEN` | ai-gateway only | `openssl rand -hex 32` |

Each is written to the appropriate per-service `.env` file under `/etc/nexus/`
which the systemd unit picks up via `EnvironmentFile=`. File mode `0640`,
owner `root:nexus` (services run as `nexus` and read; only root can rewrite).

Infra URLs (`NEXUS_HUB_URL`, `AI_GATEWAY_URL`, `COMPLIANCE_PROXY_URL`,
`COMPLIANCE_PROXY_RUNTIME_URL`) bind to `localhost` with fixed ports (see
§6) and are baked into the env files of the services that actually read
them — the Hub's own env file does NOT carry `NEXUS_HUB_URL` (that is the
registration URL the *other three* use to reach the Hub; the Hub never
reads it). Loopback `REDIS_ADDRS` / `NATS_URL` / `AUTH_SERVER_URL` /
`AUTH_SERVER_JWKS_URL` come from the yaml configs, not env.

Two values are stamped later by `first-boot.sh` because they depend on the
instance's detected IP: `publicURL` (sed-stamped into all four yamls —
required by every service's config validator) and `AUTH_SERVER_ISSUER`
(appended to **both** `control-plane.env` and `nexus-hub.env`; CP mints
tokens with this issuer and the Hub pins the same value via
`jwt.WithIssuer` when verifying enrollment JWTs — if only the CP side were
stamped, every remote agent enrollment would fail with an issuer-mismatch
401). `DATABASE_URL` is appended to all four env files by
`first-boot-db.sh` once the role + database exist.

## 5. Database initialisation (`first-boot-db.sh`)

1. `systemctl start postgresql` (synchronous via `--wait`).
2. `psql` create role `nexus` with a per-instance random password; create
   database `nexus_gateway` owned by `nexus`.
3. Write the matching `DATABASE_URL=postgresql://nexus:<pw>@localhost:5432/nexus_gateway?sslmode=disable`
   into every `*.env` file under `/etc/nexus/`.
4. `cd /opt/nexus/prisma && npx prisma db push --accept-data-loss` to
   materialise the schema (no migration history table — fresh instance, no
   upgrade path to preserve).
4b. Apply `tools/db-migrate/schema-extras.sql` (as the DB superuser) for the
   PostgreSQL-native objects `db push` can't express — the `metric_ops_raw`
   RANGE partitioning the Hub ops-raw-partition job requires, the
   `cache_key_source` function + `cache_provider_effective` view, and the
   partial/expression/GIN indexes including `thing_type_physical_id_uniq`.
5. `SEED_DEMO=false npx tsx seed/seed.ts` to load **only** the Tier-A reference
   catalog (providers, models, rules, IAM policies/groups, config defaults) and
   the bootstrap tenant (one org, one project, the `admin@nexus.ai` super-admin,
   its IAM binding, and the system-assistant VK). The Tier-B demo playground —
   whose users, virtual keys, and credentials carry plaintexts that are PUBLIC in
   the OSS repo — is **not** seeded; shipping any repo-committed credential on an
   internet-facing appliance is the default-credential exposure AWS Marketplace
   prohibits. See `tools/db-migrate/seed/seed.ts`.
6. Generate a 24-character random admin password, hash it with the same
   scrypt parameters the seed uses (`set-admin-password.js` /
   `tools/db-migrate/seed/lib.ts` `hashPassword()` — N=2^17, r=8, p=1, salt=32,
   key=64), and `UPDATE "NexusUser" SET "passwordHash" = $1 WHERE email = 'admin@nexus.ai'`.
   The bootstrap seed ships `admin@nexus.ai` with the public dev-default password
   (`nexus-demo`); this step replaces it before the appliance is ever reachable.
7. **Mint a per-instance system-assistant VK.** The bootstrap seed's
   system-assistant VK ships with a public deterministic plaintext. `mint-assistant-vk.js`
   generates a random `nvk_`-prefixed secret, hashes it under
   `ADMIN_KEY_HMAC_SECRET` (the same HKDF→HMAC derivation the gateway verifier
   uses), `UPDATE`s the VK row's `keyHash`/`keyPrefix`, and writes the plaintext
   into `control-plane.env` as `NEXUS_ASSISTANT_SYSTEM_VK` — so Chat-with-Nexus
   works once a provider key is added, with no public credential ever active.
8. Write the plaintext admin password + login URL + provider-key/chat reminders to
   `/root/nexus-admin-credentials.txt` (mode 0600, root:root — Marketplace
   policy requires the read-once file outside `/var/log`) and append
   a one-screen summary to `/etc/motd` so the operator sees it on first SSH.

`admin@nexus.ai` is the only account on a fresh instance, and its password plus
the system-assistant VK are unique per instance — there are no demo accounts to
disable because none are seeded.

**Idempotency.** The fixture seed is idempotent (Prisma upserts keyed on each
row's unique key), so a re-run converges rather than aborting. `first-boot-db.sh`
still fast-skips the seed on a `NexusUser` row-count probe, and guards the
admin-password-reset + assistant-VK-mint + creds-write tail behind
`/etc/nexus/.db-initialized` — so a reboot before the outer
`/etc/nexus/.initialized` marker is written (e.g. a later OAuth-redirect step
failing) cannot re-randomise the admin password / assistant VK or orphan the
creds the operator already read. `harden.sh` wipes both markers so a re-baked AMI
re-initialises.

## 6. Port map (every service binds `127.0.0.1` except the two agent-facing ports)

| Port | Service | Binding | Exposed via firewall? |
|---|---|---|---|
| 5432 | PostgreSQL | 127.0.0.1:5432 | no |
| 6379 | Valkey | 127.0.0.1:6379 | no |
| 4222 | NATS client | 127.0.0.1:4222 | no |
| 8222 | NATS HTTP monitoring | 127.0.0.1:8222 | no |
| 3060 | Nexus Hub | 127.0.0.1:3060 (`server.host`) | no (nginx fronts `/ws`, `/api/internal/things`, spill blob) |
| 3001 | Control Plane API | 127.0.0.1:3001 (`server.host`) | no (nginx fronts `/api/*`) |
| 3050 | AI Gateway | 127.0.0.1:3050 (`server.host`) | no (nginx fronts `/v1`; keeps unauthenticated `/internal/*` off-host) |
| 3040 | Compliance Proxy runtime API | 127.0.0.1:3040 | no |
| 9090 | Prometheus metrics | 127.0.0.1:9090 (`metrics.address`) | no |
| 3128 | Compliance Proxy CONNECT | 0.0.0.0:3128 | **yes** (agents proxy through it) |
| 443 | nginx (UI + reverse proxy) | 0.0.0.0:443 | **yes** |
| 22 | sshd | 0.0.0.0:22 | yes (Marketplace standard) |

**Binding is enforced, not just firewalled.** Hub / Control Plane / AI Gateway
set `server.host: 127.0.0.1` in their yaml (env override `NEXUS_HUB_HOST` /
`CONTROL_PLANE_HOST` / `AI_GATEWAY_HOST`; empty default = all interfaces for
container / k8s). The compliance-proxy metrics port uses `metrics.address:
127.0.0.1:9090`. So even a mis-scoped EC2 security group cannot expose these —
the socket is on loopback. Only `:3128` (the MITM CONNECT proxy agents dial)
and `:443` (nginx) bind all interfaces.

**Security-group guidance for `:3128`.** The CONNECT proxy must stay reachable
by agents, but it should NOT be open to `0.0.0.0/0` in the EC2 security group —
restrict the `:3128` ingress rule to the agent CIDR (the corporate/VPC range
the devices egress from). The proxy also enforces an app-layer
`accessControl.sourceIpAllowlist` (RFC1918 by default) and a provider
`domainAllowlist`, but the SG is the outer fence; narrow it in the Marketplace
launch guidance.

The compliance-proxy CA file path (`/etc/compliance-proxy/ca.crt`,
`/etc/compliance-proxy/ca.key`) is hardcoded into the prod-shape config
because the path is also baked into the systemd unit's `ReadWritePaths` and
into the `first-boot-ca.sh` generator — three places must agree.

## 7. Hardening (`harden.sh`)

Runs as the **last** Packer provisioner (after `install.sh`). Standard
AWS Marketplace AMI cleanup; without this the AMI fails the Self-Service
Scan and is rejected on submission.

| Action | Why |
|---|---|
| `rm -f /root/.ssh/authorized_keys /home/*/.ssh/authorized_keys` | No shared SSH keys (customers BYO) |
| `rm -f /etc/ssh/ssh_host_*` | Regenerated on first boot — no shared host keys across instances |
| `sed -i sshd_config` (PasswordAuthentication=no, PermitRootLogin=no) | Hard requirement for AWS Marketplace |
| `passwd -l root` | Lock root password |
| `find / -name authorized_keys -delete` | Recursive scrub |
| `rm -rf /var/lib/postgresql/data/* /var/lib/valkey/* /var/lib/nats/*` | Clear any pg/valkey/nats state accumulated during install validation |
| `truncate -s 0 /etc/machine-id` | Regenerated on first boot |
| `cloud-init clean --logs` | Fresh cloud-init state |
| `dnf clean all` | Shrink AMI size |
| `find /var/log -type f -exec truncate -s 0 {} \;` | No leaked build-time logs |
| `dd if=/dev/zero of=/zerofile && rm /zerofile && sync` | Free-space zeroing — EBS snapshot dedupes better |

## 8. AMI build pipeline (`nexus-ami/build.sh` → `nexus.pkr.hcl`)

```
git archive HEAD                        → nexus-ami/artifacts/nexus-src.tar.gz (built ON instance)
make control-plane-ui-build             → packages/control-plane-ui/dist/ (portable, built locally)
build.sh stages → nexus-ami/artifacts/  → flatten + copy + tar
packer init . && packer build           → AMI ID in us-east-1
```

**The four Go services are compiled ON the build instance with the Vectorscan
(Hyperscan) content-scanning engine — not cross-compiled locally.** The services
link `libhs` via cgo; a `CGO_ENABLED=0` cross-compile silently selects the
pure-Go RE2 fallback matcher, and libhs is C++ that cannot cross-compile from a
macOS arm64 control host. So `build.sh` ships the committed source tree
(`git archive HEAD` — commit first) and `scripts/build-binaries.sh` builds it
natively on the linux/amd64 instance, the same on-instance-compile model
`install-valkey.sh` already uses. The runtime needs `libstdc++` (installed by
`install.sh`); libhs itself is statically linked, built **`FAT_RUNTIME=OFF`** (a
`FAT_RUNTIME=ON` archive resolves its CPU-dispatch IFUNCs to a no-op stub in a Go
cgo binary, silently disabling `hs_scan` — `build-binaries.sh` runs a firing
self-test to gate that). The Go toolchain + libhs/ragel build trees **and the
Nexus source tree are deleted** after the build, so neither ships in the AMI.

Packer steps:

1. Launch an `m5.4xlarge` builder instance (16 vCPU / 64 GB) from the
   latest Amazon Linux 2023 AMI. **Must be `m5.4xlarge` (or larger), not
   `t3.2xlarge`** — valkey-search 1.x vendors gRPC + Protobuf + Abseil
   + ICU as submodules; template-heavy parallel C++ compile is heap-hungry
   per translation unit. Empirically, `t3.2xlarge` (32 GB) is OOM-killed
   silently mid-ICU-compile after ~11 minutes (kernel OOM-killer kills sshd
   before the script can write stderr — no trace in Packer build logs);
   64 GB clears the failure mode. 2026-05-28 build evidence.
1a. **Linker = lld, not GNU ld.** `install-valkey.sh` installs `lld${ver}`
    alongside `clang${ver}` and exports `LDFLAGS=-fuse-ld=lld` before
    invoking valkey-search's `./build.sh`. Reason: valkey-search compiles
    with `-flto`, and linking `libsearch.so` requires LTO bitcode handling.
    GNU ld delegates LTO to the LLVMgold.so plugin, but AL2023's `clang20`
    package **omits** LLVMgold.so (verified 2026-05-28: link failed with
    `cannot open /usr/lib64/llvm20/lib64/LLVMgold.so`). lld is LLVM's
    native linker and handles LTO bitcode directly without a plugin.
2. `file` provisioner uploads `nexus-ami/artifacts.tar.gz` (single file,
   ~120 MB) to `/tmp/nexus-artifacts.tar.gz`. We deliberately do NOT upload
   `artifacts/` as a directory — Packer's file provisioner uses recursive
   SCP under the hood, which silently drops individual files on slow links
   (a problem we hit on China → us-east-1 at ~250 KB/s). A single-file
   transfer is atomic and fails loudly.
3. `shell` provisioner runs `scripts/install.sh`. The script first extracts
   the tarball to `/tmp/nexus/`, then (~15 minutes total) runs
   `build-binaries.sh` (installs a transient Go + libhs `FAT_RUNTIME=OFF` +
   ragel toolchain, compiles the 4 Go services `-tags vectorscan`, self-tests
   that Vectorscan fires, then deletes the toolchain + Nexus source), installs
   Postgres, builds Valkey from source, installs NATS, installs Node + Prisma,
   and places the binaries + configs + systemd units.
4. `shell` provisioner runs `scripts/harden.sh` (~30 seconds).
5. Packer snapshots the EBS root volume → registers the AMI.

Total build time: 15–20 minutes per region (on good links;
+5–10 minutes for the cross-Pacific tarball upload from China).

### 8a. Supply-chain integrity of externally-fetched infra (SEC-W4-01)

The appliance image is the trust anchor that terminates and inspects all
customer TLS, so every third-party artifact baked into it must be
integrity-verified against a **pinned cryptographic digest** before it is
compiled/installed — a build that trusts whatever bytes `curl` / `git clone`
returns has no defense against a substituted dependency (moved tag, replaced
release tarball, build-time on-path MITM).

`scripts/lib-verify.sh` provides two fail-closed helpers (sourced by the infra
installers; a non-zero return aborts the `set -euo pipefail` build):

| Artifact | Script | Pin | Mechanism |
| --- | --- | --- | --- |
| Valkey core source tarball | `install-valkey.sh` | `VALKEY_SHA256` | `verify_sha256` before `tar`/compile |
| valkey-search (+ vendored gRPC/Protobuf/Abseil submodules) | `install-valkey.sh` | `VALKEY_SEARCH_COMMIT` | `verify_git_commit` — asserts the tag still resolves to the pinned immutable commit; submodule gitlinks live in that commit's tree, so the top-commit pin transitively pins every vendored dep |
| NATS server release tarball | `install-nats.sh` | `NATS_SHA256` (per-arch) | `verify_sha256` against the NATS-published `SHA256SUMS` |
| Node.js runtime tarball | `install-node-prisma.sh` | `NODE_SHA256` (per-arch) | `verify_sha256` against the nodejs.org-published `SHASUMS256.txt` |

The digests are recorded next to the version constant and code-reviewed in the
PR that bumps the version, so the build's trust root is the committed digest,
not the live upstream connection. Bumping any version **requires** re-recording
its digest (the re-record command is in a comment next to each constant). The
Nexus Go binaries are built locally from `go.sum`-pinned deps and Postgres comes
from the GPG-verified AL2023 dnf repo, so neither needs an explicit pin here.
The fail-closed gate is regression-tested by `scripts/lib-verify_test.sh`.

## 9. Instance sizing recommendation (Marketplace listing)

| Tier | Instance type | When |
|---|---|---|
| Minimum | `t3.large` (2 vCPU / 8 GB) | PoC, ≤ 100 traffic events/hour |
| Recommended | `t3.xlarge` (4 vCPU / 16 GB) | Small production, ≤ 10k events/hour |
| Performance | `m5.2xlarge` (8 vCPU / 32 GB) | Production, ≤ 100k events/hour |

Root volume: **≥ 30 GiB** (Postgres + Valkey + NATS file store + log
retention). Marketplace listing should state this requirement explicitly.

## 9a. High-concurrency tuning (first-boot + systemd + sysctl)

The appliance ships sized for concurrent traffic out of the box. Per-knob
values and where each is set:

| Layer | Setting | Value | Set by |
|---|---|---|---|
| Postgres | `max_connections` | 200 | `first-boot-db.sh` (`ALTER SYSTEM`) |
| Postgres | `shared_buffers` | ~25% RAM (clamped 128 MB–8 GB) | `first-boot-db.sh` |
| Service DB pools | `database.maxConns` | hub 40 · ai-gateway 25 · control-plane 15 · compliance-proxy 4 (config-only) | per-service yaml |
| systemd | `LimitNOFILE` / `LimitNPROC` | 1048576 / 65535 | the four `nexus-*.service` units |
| Go runtime | `GOMEMLIMIT` | per-service share of ~40% RAM | `first-boot.sh` → `/etc/nexus/<svc>.env` |
| Audit stream | `NEXUS_EVENTS_STORAGE` | `file` (durable; overrides the in-memory code default) | `first-boot.sh` → `/etc/nexus/nexus-hub.env` |
| Kernel | `somaxconn` / `tcp_max_syn_backlog` / `nr_open` / `file-max` | 65535 / 65535 / 2097152 / 2097152 | `/etc/sysctl.d/99-nexus.conf` (`install.sh`) |
| NATS | `max_payload` | 16 MB | `install-nats.sh` |
| Valkey | `maxmemory` | ~15% RAM, `allkeys-lru` | `first-boot.sh` |

**Load-bearing invariant — Σ(service `database.maxConns`) < Postgres
`max_connections`.** The service pools are *ceilings*; under a concurrency
burst they can all fill at once. If their sum exceeds `max_connections` the
overflow connections are refused with "too many clients already", which
surfaces only under load — exactly when it hurts. So **raising any service's
`maxConns` requires re-checking the sum against `max_connections` and raising
the latter if needed.** The default 200 leaves headroom for the shipped sum
(~84) and for a hand-tuned-higher deployment (e.g. hub 50 + control-plane 50
+ ai-gateway 25 ≈ 129). The steady-state hot path is cache-served (in-memory
config cache + Redis), so Postgres is a cold-path backstop for most services —
only the Hub (the traffic_event drain) and the Control Plane (analytics)
exercise their pools heavily; size those first.

The audit pipeline never drops a `traffic_event` silently on overflow: the
producer applies bounded backpressure, then spills to NDJSON on disk
(`audit.spoolDir`, default `/var/lib/nexus/audit-spool`) — see
[observability-architecture.md](../observability/observability-architecture.md) §3.

## 10. Out of scope (intentionally)

- **HA / multi-instance** — by design single-instance. Customers wanting HA
  use the Kubernetes / container deployment form factor (separate listing).
- **Schema migration across versions** — pre-GA policy is "fresh install
  on every AMI version bump"; customers re-launch a new AMI and re-load
  their data via the admin API. Documented as an evaluation product in
  the Marketplace listing.
- **External SSO** — AMI ships with the embedded auth server bound to
  `localhost`; OIDC federation requires the customer to edit
  `/etc/nexus/control-plane.config.yaml` `authServer:` block and restart
  the service.
- **TLS termination on a real domain** — AMI ships nginx with a self-signed
  cert generated at first boot; documented as "replace with your domain's
  cert in `/etc/nexus/tls.{crt,key}` and restart nginx".
- **Agent fleet enrollment from this AMI** — works, but the agent's
  bootstrap URL needs to be reachable from the agent host; this is a
  network-topology concern documented in the user-facing deployment guide,
  not an AMI-side decision.

## 11. Memory anchors

- `[[ami_first_boot_5_secrets]]` — five `[MUST MATCH]` secrets must be
  written before any Nexus service starts, or services 401 each other.
- `[[ami_random_admin_password_marketplace_safe]]` — random per-instance
  admin password is the cheapest defence against the AWS Marketplace
  default-credentials finding category.

## 12. Related docs

- `.env.example` — canonical env var contract (the AMI honours every
  `[MUST MATCH]` tag).
- `docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md` — 4-layer config model the AMI plugs into at L2 (yaml) + L3 (env).
- `nexus-ami/README.md` — operator-facing build / test / publish runbook.
