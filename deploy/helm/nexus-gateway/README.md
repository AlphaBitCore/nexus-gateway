# nexus-gateway Helm chart

Production Kubernetes deploy for Nexus Gateway. Installs the four service
Deployments — **console** (nginx + control-plane + UI), **hub**, **ai-gateway**,
**compliance-proxy** — with backing Postgres / Valkey / NATS either in-cluster
(default subcharts, for dev/demo) or external (managed, for production).

> docker-compose (`docker-compose.full.yml`) is the quick dev/demo path; this
> chart is the production path. Design + secret contract:
> [`container-deployment-architecture.md`](../../../docs/developers/architecture/cross-cutting/deployment/container-deployment-architecture.md).

## Prerequisites

- Kubernetes 1.26+, Helm 3.12+
- An ingress controller (nginx-ingress assumed by `values-production.yaml`)
- The published images on Docker Hub (`docker.io/alphabitcore/nexus-*`) or your
  own registry mirror set via `images.registry`

## Install (dev / demo — in-cluster datastores)

```bash
cd deploy/helm/nexus-gateway
helm dependency build          # fetch postgresql / valkey / nats subcharts
helm install nexus . \
  --namespace nexus --create-namespace
kubectl -n nexus wait --for=condition=ready pod --all --timeout=600s
```

The pre-install hook Job (`nexus-db-migrate`) pushes the schema and runs the
production seed before the services start. Shared secrets are generated once
into `<release>-nexus-gateway-secrets` and persist across upgrades (they are
never rotated underneath encrypted rows / issued keys).

## Install (production — external datastores, real TLS + secrets)

Provide secrets out-of-band (sealed-secrets / external-secrets / vault) as a
Secret with keys `internalServiceToken`, `adminKeyHmacSecret`,
`credentialEncryptionKey`, `complianceProxyApiToken`, `aiGatewayApiToken`,
then:

```bash
helm install nexus . \
  --namespace nexus --create-namespace \
  -f values-production.yaml \
  --set images.tag=v1.1.0 \
  --set secrets.existingSecret=nexus-shared-secrets \
  --set consoleTls.existingSecret=nexus-console-tls \
  --set complianceProxy.caExistingSecret=nexus-proxy-ca \
  --set external.databaseUrl='postgresql://…' \
  --set external.redisAddr='…:6379' \
  --set external.natsUrl='nats://…:4222'
```

## Key values

| Key | Default | Notes |
|---|---|---|
| `images.registry` / `images.tag` | `docker.io/alphabitcore` / `latest` | pin a semver in prod |
| `aiGateway.replicas` | `2` | the load-bearing tier — scale to traffic |
| `complianceProxy.enabled` | `true` | set `false` if you don't intercept egress TLS |
| `dbInit.enabled` / `dbInit.seedDemo` | `true` / `false` | pre-install schema+seed hook |
| `secrets.existingSecret` | `""` | supply your own Secret; else auto-generated once |
| `postgresql/valkey/nats.enabled` | `true` | in-cluster subcharts; disable + set `external.*` for managed |
| `ingress.host` | `nexus.local` | must match `authServer.issuer` |

## Verify

```bash
helm lint .
helm template nexus . | kubectl apply --dry-run=client -f -
```

`compliance-proxy` and `ai-gateway` link Vectorscan statically; verify the
engine in a running pod:

```bash
kubectl -n nexus exec deploy/nexus-nexus-gateway-ai-gateway -- hs-selfcheck
```
