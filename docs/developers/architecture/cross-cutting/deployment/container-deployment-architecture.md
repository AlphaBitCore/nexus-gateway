# Container deployment architecture

How Nexus Gateway ships as container images and deploys via docker-compose
(dev / demo) and Helm (production). Sibling to the appliance form factor in
[`ami-appliance-architecture.md`](./ami-appliance-architecture.md); the two
share the same binaries, config-env contract, and shared-secret set — they
differ only in packaging and orchestration.

## Images

Four **product images** are built and published to Docker Hub under the
`alphabitcore` org. Backing stores (PostgreSQL, Valkey, NATS; ClickHouse in a
later phase) are pulled from upstream images, never rebuilt.

| Image | Contents | Ports | Build |
|---|---|---|---|
| `alphabitcore/nexus-console` | nginx + control-plane binary + control-plane-UI dist | 80, 443, 3001 | pure Go + static assets (`CGO_ENABLED=0`) |
| `alphabitcore/nexus-hub` | hub binary | 3060 | pure Go |
| `alphabitcore/nexus-ai-gateway` | ai-gateway binary | 3050 | ⚠ cgo + static libhs (Vectorscan) |
| `alphabitcore/nexus-compliance-proxy` | compliance-proxy binary | 3128, 3040, 9090 | ⚠ cgo + static libhs (Vectorscan) |

Plus one **deploy-utility image**, `alphabitcore/nexus-db-migrate` (not a
product image): wraps `tools/db-migrate` so schema push + prod seed can run as
a Helm hook Job. docker-compose uses the same image for its `db-init` service.

**Why split rather than one monolith:** compliance-proxy is optional per
customer — a separate image means deployments that don't intercept egress TLS
simply don't pull it. hub + ai-gateway are the load-bearing pair and scale
independently; console and compliance-proxy stay light and independently
versioned.

### The console image (composition)

`deploy/docker/console/Dockerfile` is a three-stage build: Go builder
(control-plane) + Node builder (UI dist) + `nginx:alpine` final. The entrypoint
renders `nginx.conf.tpl` and `config.yaml.tpl` with `envsubst`, ensures TLS
material exists (mount real certs at `/etc/nexus/tls.{crt,key}` for production;
a self-signed pair is generated otherwise), starts the control-plane on
loopback `:3001`, waits for its `/healthz`, then runs nginx in the foreground.
Both processes are supervised: if either exits, the container exits and the
orchestrator restarts it atomically.

The nginx config is derived from `nexus-ami/artifacts/configs/nginx-nexus.conf`
and keeps the same location blocks (SPA fallback, `/api/`+auth → control-plane,
`/v1|/v1beta|/openai/deployments|/api/paas` → ai-gateway, `/ws`+enrollment →
hub). The only change: the AI-gateway and hub upstreams are **env-parameterized
service names** (`NEXUS_AI_GATEWAY_UPSTREAM`, `NEXUS_HUB_UPSTREAM`) instead of
`127.0.0.1`, so one image works under compose service DNS and K8s cluster DNS.
Keep the two nginx files' location blocks in lockstep.

## Vectorscan build discipline (ai-gateway + compliance-proxy)

Both services link libhs (Vectorscan) via cgo for accelerated content
scanning; the pure-Go RE2 matcher is the differential oracle and the fallback.
The Matcher seam selects the engine by build tag: `-tags vectorscan` compiles
`packages/shared/policy/hooks/matcher/vectorscan.go` (cgo); the default build
compiles `compile_default_re2.go`. **A no-tag image silently uses RE2** —
correct but far slower under load. The Dockerfiles enforce four rules so a
fallback image can never ship by accident:

1. **Per-arch static libhs.** Each Dockerfile compiles libhs from source in a
   builder stage; buildx builds one matching static archive per target arch.
2. **`FAT_RUNTIME=OFF` (mandatory).** With it ON the library links fine but
   `hs_alloc_scratch` silently fails inside a cgo binary and scanning never
   fires — PII passes through unredacted with no error. `BUILD_AVX512=OFF` for
   portability (an AVX512 build SIGILLs on CPUs without avx512vbmi).
3. **Build-time self-test (Gate 1).** A tiny program calls
   `matcher.HSSelfTest()` (`hs_compile` + `hs_alloc_scratch` + `hs_scan`);
   a non-zero return or zero matches fails the build. The `hs-selfcheck` binary
   ships in the image so CI and operators can re-run it in the exact runtime:
   `docker run --rm --entrypoint hs-selfcheck <image>`.
4. **Binary tag check (Gate 2).** `go version -m` must show `-tags=vectorscan`
   on the shipped binary, else the build fails.

Arch scope: `linux/amd64` first; `linux/arm64` is an additive fast-follow
(per-arch libhs makes it structural-free). Runtime base is `debian:bookworm-slim`
(glibc) rather than alpine/musl to avoid cgo linking friction.

## docker-compose (dev / demo)

`docker-compose.full.yml` brings up the whole stack: postgres + valkey + nats
(upstream images) + `db-init` (one-shot, `service_completed_successfully` gate)
+ hub → ai-gateway → compliance-proxy → console, ordered by healthchecks and
`depends_on: {condition: service_healthy}` mirroring the AMI systemd order. It
coexists with the dev-infra `docker-compose.yml` (used by `scripts/dev-start.sh`)
via distinct container names and volumes.

`scripts/compose-init.sh` is the compose analog of the AMI
`first-boot-secrets.sh`: it generates the shared secrets once into
`deploy/docker/.env.compose` (gitignored, mode 0600) and refuses to overwrite
(regenerating would invalidate encrypted rows / issued keys). The compose file
reads secrets only from that env file — never inline yaml.

## Helm (production)

`deploy/helm/nexus-gateway/` is an umbrella chart: four service Deployments +
Services, an Ingress that terminates at the console (which reverse-proxies
gateway + hub internally — one external surface), a shared-secret Secret, and a
`db-init` pre-install/pre-upgrade hook Job. `aiGateway.replicas` defaults to 2
(the load-bearing tier); `complianceProxy.enabled=false` drops that layer
entirely.

Backing stores are subchart dependencies (Bitnami PostgreSQL/Valkey, NATS)
enabled by default for dev/demo; production sets `*.enabled=false` and points
`external.{databaseUrl,redisAddr,natsUrl}` at managed endpoints
(`values-production.yaml`). TLS and the MITM CA are provided via existing
Secrets in production; the pre-install path generates self-signed material for
dev/demo.

## Shared-secret contract

Same five `[MUST MATCH]` secrets as the appliance (see `.env.example` and
`ami-appliance-architecture.md` §secrets): `INTERNAL_SERVICE_TOKEN`,
`ADMIN_KEY_HMAC_SECRET`, `CREDENTIAL_ENCRYPTION_KEY`,
`COMPLIANCE_PROXY_API_TOKEN`, `AI_GATEWAY_API_TOKEN`. In compose they live in
the generated `.env.compose`; in Helm they come from `secrets.existingSecret`
or a chart-generated Secret whose values **persist across upgrades** via
`lookup` (never rotated underneath encrypted data). Secrets are injected as env
vars only — no secret ever appears in a committed yaml.

## Publish pipeline

`.github/workflows/docker-publish.yml` builds on `v*` tags (or manual dispatch)
and gates publishing in three stages:

- **Gate A** — the in-Dockerfile Vectorscan self-test + binary tag check.
- **Gate B** — `hs-selfcheck` executed inside each built runtime image, proving
  the engine works in the exact shipped environment (catches `FAT_RUNTIME`
  mistakes a successful build would miss).
- **Gate C** — a full-stack compose smoke (`deploy/docker/smoke-redaction.sh`):
  brings the stack up, logs in as the bootstrap admin, enables the pii-detector
  hook, and sends a PII-bearing request through the gateway asserting the
  compliance pipeline blocks/redacts it (never echoes the raw SSN).

Only after all three pass are the images tagged (`<semver>`, `<sha>`, `latest`)
and pushed to `docker.io/alphabitcore/*`. Correctness on every PR remains the
job of `ci.yml` / `go-ci.yml`; this workflow gates *publishing*.
