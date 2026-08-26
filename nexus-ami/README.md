# Nexus Gateway — AMI / appliance build

Single-instance, all-in-one Nexus Gateway packaged as an AWS Marketplace AMI.
Same artifacts are the foundation for the future on-prem appliance form factor
(bare-metal / VMware / KVM disk images).

> **Source of truth for everything in this directory:**
> [`docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md`](../docs/developers/architecture/cross-cutting/deployment/ami-appliance-architecture.md).
> Read it first before changing scripts, configs, or systemd units in this tree.

## What's in the AMI

| Layer | Component | Source |
|---|---|---|
| Runtime deps | PostgreSQL 16 | `dnf install postgresql16-server` (AL2023 default) |
| Runtime deps | Valkey 8 + `valkey-search` module | `scripts/install-valkey.sh` (source compile) |
| Runtime deps | NATS Server 2 (JetStream) | `scripts/install-nats.sh` (official binary) |
| Runtime deps | Node.js 20 + Prisma + tsx | `scripts/install-node-prisma.sh` (first-boot only) |
| Runtime deps | nginx | `dnf install nginx` |
| Nexus | Hub binary (3060) | `scripts/build-binaries.sh` on the instance, `-tags vectorscan` |
| Nexus | Control Plane binary (3001) | `scripts/build-binaries.sh` on the instance, `-tags vectorscan` |
| Nexus | AI Gateway binary (3050) | `scripts/build-binaries.sh` on the instance, `-tags vectorscan` |
| Nexus | Compliance Proxy binary (3128) | `scripts/build-binaries.sh` on the instance, `-tags vectorscan` |
| Nexus | Control Plane UI dist | `make control-plane-ui-build` → `packages/control-plane-ui/dist/` |
| Nexus | DB schema + seed | `tools/db-migrate/{schema.prisma, seed/}` |
| Nexus | 4 prod-shape `*.config.yaml` | `artifacts/configs/` |
| Nexus | 7 systemd units | `artifacts/systemd/` |

## Quick build

```bash
# Prerequisites: Node 20+, Packer 1.10+, git, AWS credentials. (No local Go: the
# Go services are built ON the Packer instance with the Vectorscan cgo engine —
# see below. Commit your changes first; the build ships `git archive HEAD`.)
cd nexus-ami
./build.sh                    # full pipeline: stage source + UI + packer build
./build.sh --skip-packer      # stop after staging (CI dry-run)
```

The full pipeline takes 20–30 minutes:

1. `git archive HEAD` — stage the committed Go source (≈ 5 s); the 4 services are
   compiled `-tags vectorscan` ON the instance by `scripts/build-binaries.sh`
   (a CGO_ENABLED=0 cross-compile would silently fall back to the RE2 matcher)
2. `make control-plane-ui-build` — Vite UI dist (≈ 30 s)
3. Stage `artifacts/{ui-dist,prisma,nexus-src.tar.gz}` (≈ 5 s)
4. `packer build` — launches an `m5.4xlarge`, runs `install.sh` (the Valkey +
   valkey-search source compile is the long pole) + `harden.sh`, snapshots
   the AMI (≈ 15–20 min)

Output: a registered AMI ID in your AWS account (region per
`nexus.pkr.hcl` `aws_region` variable, default `us-east-1`).

## Test a fresh AMI manually

```bash
# 1. Launch a t3.xlarge from the AMI you just built. Wait for it to boot.
# 2. SSH in with your EC2 key pair:
ssh -i ~/.ssh/your-key.pem ec2-user@<public-ip>

# 3. Read the per-instance admin credentials (generated on first boot,
#    mode 0600, root-only — delete after first read):
sudo cat /root/nexus-admin-credentials.txt

# 4. Verify all 7 Nexus-related services are green:
systemctl status nexus-first-boot postgresql valkey nats \
                  nexus-hub nexus-control-plane nexus-gateway nexus-proxy nginx

# 5. Open https://<public-ip>/ in a browser (accept the self-signed cert),
#    log in with the credentials from step 3.

# 6. Launch a SECOND instance from the same AMI and confirm
#    /root/nexus-admin-credentials.txt contains a DIFFERENT password.
#    Per-instance secret uniqueness is the most important first-boot invariant.
```

## Service endpoints (nginx reverse proxy on :443)

Everything is reached over HTTPS on port 443 (the self-signed cert from
`first-boot-ca.sh`); only 443, 3128 (Compliance Proxy CONNECT), and 22 need
to be open in the EC2 Security Group. `nginx-nexus.conf` maps:

| Path | Backend | Purpose |
|---|---|---|
| `/` | UI (static) | Control Plane SPA |
| `/downloads/` | UI (static) | Agent installer artifacts (`/opt/nexus/ui/downloads/`) |
| `/api/`, `/oauth/`, `/authserver/`, `/.well-known/`, `/scim/` | Control Plane :3001 | Admin API, OAuth/OIDC, SCIM provisioning |
| `/v1/`, `/v1beta/`, `/openai/deployments/`, `/api/paas/` | AI Gateway :3050 | LLM ingress — OpenAI / Gemini / Azure / GLM wire formats |
| `/ws`, `/api/internal/things/`, `/api/internal/spill/blob/` | Nexus Hub :3060 | Remote endpoint-agent enrollment + control WebSocket + spill-body upload |
| `/healthz`, `/ready` | Control Plane :3001 | Unauthenticated health / readiness |
| `/api/internal/*` (all other) | — | **Refused (403)** — service-to-service surface, loopback-only |

Verify the OpenAI provider you configured in the UI:

```bash
# List the models the gateway serves (OpenAI providers show owned_by:"openai"):
curl -sk https://<public-ip>/v1/models | jq '.data[] | select(.owned_by=="openai")'

# End-to-end round-trip through the gateway (needs a virtual key from the UI):
curl -sk https://<public-ip>/v1/chat/completions \
  -H "Authorization: Bearer <virtual-key>" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"ping"}]}'
```

The admin UI's **Simulator** page and the per-credential **Test** button
(which probes the provider's real `/v1/models` with your stored key) also work
without any client-side setup.

Remote agents enroll against the Hub over the same 443 endpoint:

```bash
nexus-agent enroll --hub-url https://<public-ip> --token <enrollment-token> \
  --hub-ca <appliance-tls-cert.pem>
```

**Security note:** `/v1/*`, `/ws`, and the agent-facing Hub paths
(`/api/internal/things/*`, `/api/internal/spill/blob/*`) are internet-reachable.
They are not anonymous — `/v1/*` requires a virtual key, and the Hub agent
surface is gated by per-device / enrollment / internal-service tokens. Every
**other** `/api/internal/*` path (the Control Plane's service-to-service API)
is refused with 403 at nginx — its only callers are the co-located services
on loopback. The Hub admin API (`/api/hub/*`), Prometheus `/metrics`, and
`/debug/runtime` are deliberately NOT proxied and stay loopback-only.

**Demo accounts:** the seeded `alice@/bob@/carol@/diana@/macmini@nexus.ai`
users are **disabled automatically on first boot** — only the per-instance
randomised `admin@nexus.ai` can log in. (Their password hashes are public in
the OSS repo, so leaving them active on an internet-facing instance would be
default-credential exposure.)

**Bind posture:** Hub / Control Plane / AI Gateway bind `127.0.0.1` only
(`server.host` in their yaml); compliance-proxy metrics bind `127.0.0.1:9090`.
nginx (same host) fronts everything on `:443`, so a mis-scoped security group
can't expose them — the sockets are loopback. Only `:3128` (the MITM CONNECT
proxy agents dial) and `:443`/`:22` are on all interfaces. **Restrict the
`:3128` security-group rule to your agent CIDR** rather than `0.0.0.0/0` — the
proxy enforces an app-layer source-IP + domain allowlist, but the SG is the
outer fence.

## Self-Service AMI Scan iteration

Run AWS's Self-Service Scan from the Partner Central → Marketplace
Management Portal. Expect 2–3 rebuild cycles before the scan returns
zero findings. Common first-build hits the scan catches:

- A package update landed a new CVE — `dnf update -y` is in `install.sh`
  so the rebuild self-fixes; just re-run `packer build`.
- An overlooked `authorized_keys` file — re-run `harden.sh` (already
  hardened with recursive `find / -name authorized_keys -delete`).
- SSH config not strict enough — `harden.sh` already enforces
  `PasswordAuthentication=no`, `PermitRootLogin=no`,
  `PermitEmptyPasswords=no`. If the scanner cites a new sshd directive,
  add it to `harden.sh`.

## Directory layout

```
nexus-ami/
├── README.md                        ← this file
├── nexus.pkr.hcl                    ← Packer template
├── build.sh                         ← orchestrator (compile → stage → packer)
├── artifacts/                       ← Packer file-provisioner source
│   ├── bin/                         ← populated by build.sh (gitignored)
│   ├── ui-dist/                     ← populated by build.sh (gitignored)
│   ├── prisma/                      ← populated by build.sh (gitignored)
│   ├── configs/
│   │   ├── nexus-hub.config.yaml
│   │   ├── control-plane.config.yaml
│   │   ├── ai-gateway.config.yaml
│   │   ├── compliance-proxy.config.yaml
│   │   └── nginx-nexus.conf
│   └── systemd/
│       ├── nexus-first-boot.service
│       ├── valkey.service
│       ├── nats.service
│       ├── nexus-hub.service
│       ├── nexus-control-plane.service
│       ├── nexus-gateway.service
│       └── nexus-proxy.service
└── scripts/
    ├── install.sh                   ← orchestrator (runs at Packer time)
    ├── install-postgres.sh
    ├── install-valkey.sh
    ├── install-nats.sh
    ├── install-node-prisma.sh
    ├── first-boot.sh                ← orchestrator (runs once per instance)
    ├── first-boot-secrets.sh
    ├── first-boot-ca.sh
    ├── first-boot-db.sh
    ├── set-admin-password.js        ← Node helper, deployed to /opt/nexus/prisma/
    └── harden.sh                    ← Marketplace cleanup (LAST provisioner)
```

## What's intentionally NOT here

- **Multi-instance HA / Kubernetes manifests** — the appliance form factor
  is single-instance by design. Container / K8s deployment is a separate
  product line with its own architecture doc.
- **Schema migration across Nexus versions** — pre-GA policy. Customers
  re-launch a new AMI version and re-create their workloads through the
  admin API. Documented in the Marketplace listing as an evaluation
  product.
- **Real TLS certificate provisioning** — first-boot generates a self-signed
  cert at `/etc/nexus/tls.{crt,key}`. Operators replace with a real cert
  and `systemctl reload nginx`.
- **S3 spill storage** — the appliance spills oversized captured bodies to
  the local filesystem (`/var/lib/nexus/spill`, localfs backend, 5 GiB cap,
  7-day retention) rather than S3. All four services share that one root.
  A multi-node deployment that needs shared spill across hosts is a separate
  (non-appliance) topology.

## Maintenance cadence

Plan a **monthly rebuild** to absorb AL2023 + Postgres + Valkey + NATS
CVE patches. `build.sh` is the single command; wire it into a CI cron
once the AMI is stabilised.
