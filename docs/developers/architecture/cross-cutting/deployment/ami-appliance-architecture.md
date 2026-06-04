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
                                   prisma db seed, randomise admin password,
                                   write /var/log/nexus/admin-credentials.txt
                                   and /etc/motd
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
| `/opt/nexus/ui/` | root:root | 0755 | Vite-built UI dist (immutable, part of AMI) |
| `/opt/nexus/prisma/` | root:root | 0755 | Prisma schema + seed (immutable, part of AMI) |
| `/etc/nexus/` | root:nexus | 0750 | 4 prod-shape `*.config.yaml` + 4 `*.env` + nginx-nexus.conf + `.initialized` marker |
| `/etc/compliance-proxy/` | root:nexus | 0750 | MITM CA cert + key (generated first-boot) |
| `/var/lib/nexus/` | nexus:nexus | 0750 | Service runtime state (agent CA dir, NDJSON spool, file-backed alerting state) |
| `/var/lib/postgresql/data/` | postgres:postgres | 0700 | PostgreSQL data directory (AL2023 dnf default) |
| `/var/lib/valkey/` | valkey:valkey | 0750 | Valkey AOF + RDB |
| `/var/lib/nats/` | nats:nats | 0750 | NATS JetStream file store |
| `/var/log/nexus/` | nexus:nexus | 0750 | Service log files (rotated by logrotate); also holds `admin-credentials.txt` (mode 0640, root:nexus) |

## 4. Secret generation (`first-boot-secrets.sh`)

Five environment variables MUST be unique-per-instance and identical across
the four services that share them (see `.env.example` `[MUST MATCH]` tags):

| Env var | Used by | Generation |
|---|---|---|
| `INTERNAL_SERVICE_TOKEN` | all 4 | `openssl rand -hex 32` |
| `ADMIN_KEY_HMAC_SECRET` | control-plane, ai-gateway | `openssl rand -hex 32` |
| `CREDENTIAL_ENCRYPTION_KEY` | control-plane, ai-gateway | `openssl rand -hex 32` (AES-256, 64 hex chars) |
| `COMPLIANCE_PROXY_API_TOKEN` | control-plane, compliance-proxy | `openssl rand -hex 32` |
| `AI_GATEWAY_API_TOKEN` | ai-gateway only | `openssl rand -hex 32` |

Each is written to the appropriate per-service `.env` file under `/etc/nexus/`
which the systemd unit picks up via `EnvironmentFile=`. File mode `0640`,
owner `root:nexus` (services run as `nexus` and read; only root can rewrite).

`DATABASE_URL`, `REDIS_ADDRS`, `NATS_URL`, `NEXUS_HUB_URL`,
`AUTH_SERVER_URL`, `AUTH_SERVER_JWKS_URL`, `AUTH_SERVER_ISSUER`,
`AI_GATEWAY_URL`, `COMPLIANCE_PROXY_URL`, `COMPLIANCE_PROXY_RUNTIME_URL` —
all bind to `localhost` with fixed ports (see §6), baked into the per-service
`.env` files at first boot.

## 5. Database initialisation (`first-boot-db.sh`)

1. `systemctl start postgresql` (synchronous via `--wait`).
2. `psql` create role `nexus` with a per-instance random password; create
   database `nexus_gateway` owned by `nexus`.
3. Write the matching `DATABASE_URL=postgresql://nexus:<pw>@localhost:5432/nexus_gateway?sslmode=disable`
   into every `*.env` file under `/etc/nexus/`.
4. `cd /opt/nexus/prisma && npx prisma db push --skip-generate` to materialise
   the schema (no migration history table — fresh instance, no upgrade path
   to preserve).
5. `npx tsx seed/seed.ts` to load baseline rows (organisations, IAM,
   roles, default settings — see `tools/db-migrate/seed/seed.ts`).
6. Generate a 24-character random admin password, hash it with the same
   scrypt parameters the seed uses (`tools/db-migrate/seed/lib.ts`
   `hashPassword()` — N=16384, r=8, p=1, salt=32, key=64), and
   `UPDATE "NexusUser" SET "passwordHash" = $1 WHERE email = 'admin@nexus.ai'`.
7. Write the plaintext password + login URL + warning to
   `/var/log/nexus/admin-credentials.txt` (mode 0640, root:nexus) and append
   a one-screen summary to `/etc/motd` so the operator sees it on first SSH.

`admin@nexus.ai` is the only seeded user that ships with a password. All
other seeded users (alice / bob / carol / diana etc., listed in
`packages/control-plane-ui/README.md`) keep their dev-time passwords from
the seed and are documented as "demo accounts — disable for production"
in the operator-facing docs.

## 6. Port map (all bound to `localhost` except nginx + compliance-proxy)

| Port | Service | Binding | Exposed via firewall? |
|---|---|---|---|
| 5432 | PostgreSQL | localhost:5432 | no |
| 6379 | Valkey | localhost:6379 | no |
| 4222 | NATS client | localhost:4222 | no |
| 8222 | NATS HTTP monitoring | localhost:8222 | no |
| 3060 | Nexus Hub | localhost:3060 | no |
| 3001 | Control Plane API | localhost:3001 | no (nginx proxies `/api/*`) |
| 3050 | AI Gateway | 0.0.0.0:3050 | **yes** (SDK clients hit this directly) |
| 3040 | Compliance Proxy runtime API | localhost:3040 | no |
| 3128 | Compliance Proxy CONNECT | 0.0.0.0:3128 | **yes** (network-proxied apps) |
| 9090 | Prometheus metrics | localhost:9090 | no |
| 443 | nginx (UI + `/api/*` reverse proxy) | 0.0.0.0:443 | **yes** |
| 22 | sshd | 0.0.0.0:22 | yes (Marketplace standard) |

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
make build-all                          → dist/bin/<svc>/<svc> (4 Go binaries)
make control-plane-ui-build             → packages/control-plane-ui/dist/
build.sh stages → nexus-ami/artifacts/  → flatten + copy + tar
packer init . && packer build           → AMI ID in us-east-1
```

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
   the tarball to `/tmp/nexus/`, then (~10 minutes total) installs
   Postgres, builds Valkey from source, installs NATS, installs Node +
   Prisma, places binaries + configs + systemd units.
4. `shell` provisioner runs `scripts/harden.sh` (~30 seconds).
5. Packer snapshots the EBS root volume → registers the AMI.

Total build time: 15–20 minutes per region (on good links;
+5–10 minutes for the cross-Pacific tarball upload from China).

## 9. Instance sizing recommendation (Marketplace listing)

| Tier | Instance type | When |
|---|---|---|
| Minimum | `t3.large` (2 vCPU / 8 GB) | PoC, ≤ 100 traffic events/hour |
| Recommended | `t3.xlarge` (4 vCPU / 16 GB) | Small production, ≤ 10k events/hour |
| Performance | `m5.2xlarge` (8 vCPU / 32 GB) | Production, ≤ 100k events/hour |

Root volume: **≥ 30 GiB** (Postgres + Valkey + NATS file store + log
retention). Marketplace listing should state this requirement explicitly.

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
