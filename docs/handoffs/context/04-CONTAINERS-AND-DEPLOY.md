# 04 · Containers & Deploy — images, compose, Helm, publish pipeline

The container packaging that started the program (Chen Jin's 7/2–7/3 ask) and
became the Observatory's deployment mechanism. **Built + verified, in review as
PR #81** (`bde69d5`) against `AlphaBitCore/nexus-gateway` (from the fork). Not
yet published (needs Docker Hub secrets).

---

## The 4 product images (Chen Jin's authoritative plan)

| Image | Contents | Build |
|---|---|---|
| `alphabitcore/nexus-console` | nginx + control-plane + control-plane-UI dist (the admin console) | pure Go, CGO off |
| `alphabitcore/nexus-hub` | hub binary | pure Go, CGO off |
| `alphabitcore/nexus-ai-gateway` | ai-gateway binary | ⚠️ **vectorscan** (cgo + static libhs) |
| `alphabitcore/nexus-compliance-proxy` | compliance-proxy binary | ⚠️ **vectorscan** (cgo + static libhs) |

Plus a deploy-utility image `alphabitcore/nexus-db-migrate` (Prisma schema push +
seed — used by compose `db-init` and the Helm pre-install hook). Dependency
images (Postgres/Valkey/NATS) are pulled upstream, never built.

**Why split:** compliance-proxy is optional per customer (separate image = don't
ship dead weight); hub + ai-gateway are the load-bearing pair (scale
independently); console stays light.

## Vectorscan discipline (the security-critical part)
Agent regex scanning links libhs (Vectorscan) via cgo; the pure-Go RE2 matcher is
the fallback. A no-tag build **silently** uses RE2 → correct but slow, and PII
scanning could pass unredacted. Four rules enforced in the Dockerfiles:
1. Per-arch static libhs compiled in a builder stage.
2. **`FAT_RUNTIME=OFF`** (mandatory — ON links but `hs_alloc_scratch` silently fails in cgo) + `BUILD_AVX512=OFF` (portability).
3. **Hard-fail if vectorscan didn't link** — build-time `HSSelfTest` (Gate 1) + `go version -m` tag check (Gate 2).
4. **Runtime redaction/self-test in CI**, not "build succeeded" — the shipped `hs-selfcheck` binary; verified `scanRC=0 matches=1` at build AND runtime.

Link detail (hard-won): switched to the repo's `vsstatic` tag with archive-first
ordering (`CGO_LDFLAGS="/usr/local/lib/libhs.a -lstdc++ -lm"`) + both include
paths; libhs cmake needs `libsqlite3-dev`.

## docker-compose.full.yml (dev/demo)
9 services: postgres + valkey + nats + `db-init` + hub + ai-gateway +
compliance-proxy + console. Healthchecks + `service_healthy` ordering mirroring
the AMI systemd order. Secrets via `.env` only via `scripts/compose-init.sh`
(generates the **6** `[MUST MATCH]` shared secrets — incl. `HUB_CONFIG_TOKEN`,
which was missing from the initial 5-secret set). **Verified:** full stack came up
healthy end-to-end; gateway authenticated a seeded VK and ran the pipeline.

## Helm chart (production) — `deploy/helm/nexus-gateway/`
Umbrella chart: 4 Deployments + Services, Ingress → console, shared-secret Secret,
`db-init` pre-install hook, `values.yaml` + `values-production.yaml`. Backing
stores as subchart deps (default) or external. **Verified:** `helm lint` clean +
dev(34)/prod(10) renders valid; `complianceProxy.enabled=false` opt-out; image
refs quoted (fixed empty-tag YAML break).

## Publish pipeline — `.github/workflows/docker-publish.yml`
Tag `v*` (or dispatch), buildx, amd64 (arm64 fast-follow). Three publish gates:
Gate A (in-Dockerfile self-test + tag check), Gate B (`hs-selfcheck` in the runtime
image), Gate C (compose redaction smoke). Needs `DOCKERHUB_USERNAME` /
`DOCKERHUB_TOKEN` secrets. **Fork PRs can't run it (no secrets) — publishes post-merge.**

## Config/secret gaps found & fixed (only by actually building)
- Every Dockerfile COPYed gitignored `go.work.sum` → `GOWORK=off` + sibling replaces.
- console `npm ci` ran root `prepare` git-hooks → `--ignore-scripts`; UI build needs repo-root `scripts/` → COPY it.
- ai-gateway/hub/control-plane each need `publicURL` env; hub + CP need `HUB_CONFIG_TOKEN` (6th shared secret); compliance-proxy needs an **EC (P-256)** CA with pathlen:0, not RSA.

## Docs (code/doc lockstep)
New `container-deployment-architecture.md` + arch-README trigger row + updated
`deployment-models.md` + root README Docker section. `check:arch-doc-triggers` OK.

## Deployment models (3, all self-hosted variants)
- **AMI appliance** (existing, systemd) — single-instance.
- **docker-compose** — dev/demo (this work).
- **Helm/K8s** — production (this work).

## Status & blockers
- ✅ Built + verified; **PR #81** open (from fork `Kaushik985:feature/container-deploy`).
- ⛔ **Not published** — needs the `alphabitcore` Docker Hub org confirmed + a push token as Actions secrets (James).
- ⛔ Kind runtime install of the Helm chart not run (verified via lint+render).
- Durable: per-repo write access removes the fork-PR CI-secret limitation.

## Reproducible push/PR flow
`~/Desktop/Desktop2/nexus-open-prs.sh` — `MODE=fork` (used now) or `MODE=upstream`
(on write access); idempotent, skips existing PRs.

## The benchmark rig (context — AlphaBitCore/llm-gateway-benchmark)
Already fully automated: `./deploy.sh` (CloudFormation `perf-matrix-stack.yaml` +
Ansible) + `./down.sh`; us-east-1; c6i.4xlarge per gateway. Re-running needs no new
kickoff values. The rig's `benchmark-results/` + `compare-gateways.py` feed the
site exporter. The **Observatory Arena** (02-BACKEND) reuses these images + this
rig shape so live numbers match published ones.
