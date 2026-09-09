# System overview

Nexus Gateway is an enterprise AI-traffic gateway: it governs, routes, caches, and audits the AI traffic an organization generates, for compliance and control. This page is the architecture front door — it names the running parts, how they coordinate, and how AI traffic reaches the compliance pipeline, then points into the per-service and cross-cutting docs.

## The five services

Each service is a Go process with its own `cmd/` entry point; they share libraries through `packages/shared/` over a `go.work` workspace, plus `packages/httpclient/` — a deliberately tiny module so that even the ones which avoid `packages/shared` can construct an HTTP client the same way.

| Service | Package | Role |
|---|---|---|
| **Nexus Hub** | `packages/nexus-hub` | The coordination core. Every other service and every Agent registers with Hub and is managed through it; Hub owns the registry, config templates, drift reconciliation, and cross-service calls. |
| **Control Plane** | `packages/control-plane` | The admin surface. Serves the admin HTTP API (Echo) and the React admin UI, writes configuration through Hub, and records the admin audit log. |
| **AI Gateway** | `packages/ai-gateway` | The server-side API entry point. Applications send AI requests authenticated by a virtual key; the gateway applies routing, caching, and the compliance pipeline, then forwards to the provider. |
| **Compliance Proxy** | `packages/compliance-proxy` | An explicit forward proxy that intercepts outbound HTTPS to AI provider and consumer surfaces, runs the compliance pipeline on the decrypted traffic, and emits an audit record. |
| **Agent** | `packages/agent` | The endpoint agent. It intercepts AI traffic locally on a user's device and applies the same compliance pipeline at the source. |

The Control Plane UI (`packages/control-plane-ui`) is the React + TypeScript admin console served by the Control Plane; the user-facing feature docs live under `docs/users/features/cp-ui/` and `docs/users/features/agent-ui/`.

## How the services coordinate

Coordination runs through the **Thing model**: every backend service and every Agent registers with Hub as a *Thing* and carries a *shadow* — Hub-managed desired state and Thing-reported applied state. Admins change configuration in the Control Plane, which writes it to Hub; Hub updates the desired shadow and signals the Thing, which pulls and applies it and reports back. Drift between desired and reported is what the fleet views surface as out-of-sync nodes. See [thing-model.md](cross-cutting/foundation/thing-model.md) for the kernel, [thing-config-sync-architecture.md](cross-cutting/foundation/thing-config-sync-architecture.md) and [configuration-architecture.md](cross-cutting/foundation/configuration-architecture.md) for the config cascade, and [service-call-framework.md](cross-cutting/foundation/service-call-framework.md) plus [multi-endpoint-coordination-architecture.md](cross-cutting/foundation/multi-endpoint-coordination-architecture.md) for cross-service calls.

## How AI traffic is governed

AI traffic enters governance through three paths, all sharing the same compliance pipeline and audit model:

- **AI Gateway** — for applications that call the gateway directly with a virtual key (server-side integrations).
- **Compliance Proxy** — for traffic captured at the network layer by MITM-intercepting outbound HTTPS to AI surfaces (including browser and consumer surfaces).
- **Agent** — for traffic intercepted on the endpoint device itself.

The shared compliance pipeline (hooks and rule packs), the traffic and audit records, virtual keys, and the IAM model are common across these paths.

## Per-service docs

Each service has its own front-door architecture doc that details its request flow and indexes its per-concern docs:

- [nexus-hub-architecture.md](services/hub/nexus-hub-architecture.md)
- [control-plane-architecture.md](services/control-plane/control-plane-architecture.md)
- [ai-gateway-architecture.md](services/ai-gateway/ai-gateway-architecture.md)
- [compliance-proxy-architecture.md](services/compliance-proxy/compliance-proxy-architecture.md)
- [agent-architecture.md](services/agent/agent-architecture.md)

## Operator toolkit

Alongside the five services and the web admin console, `nexus` (`packages/nexus-cli`) is a single Go binary that operates and observes the gateway from the terminal as a TUI and a scriptable CLI. It is a pure client over the existing admin API and `/v1/*` — it adds no backend endpoint and is governed by the same IAM as any other caller. See [nexus-operator-toolkit-architecture.md](nexus-operator-toolkit-architecture.md).

## Cross-cutting concerns

Concerns that span services live under `cross-cutting/`: `foundation/` (the Thing model, configuration, the service-call framework, jobs, the message queue, endpoint typology), `storage/` (database and migrations), `observability/`, `safety/` (kill switch and related controls), `shared/`, and `ui/`.

## Deployment

The five Go services run alongside PostgreSQL (persistent state, via Prisma-managed migrations with hand-written `pgx` at runtime), Redis (cache only — sessions, IAM cache, rate limiting, response cache, quota counters), and NATS JetStream (the message queue). The Control Plane UI is a Vite build served by the Control Plane. Agents run on user devices and connect back to Hub.

## References

- `packages/nexus-hub/cmd/nexus-hub/` — Hub entry point
- `packages/control-plane/cmd/control-plane/` — Control Plane entry point
- `packages/ai-gateway/cmd/ai-gateway/` — AI Gateway entry point
- `packages/compliance-proxy/cmd/compliance-proxy/` — Compliance Proxy entry point
- `packages/agent/cmd/agent/` — Agent entry point
- `packages/shared/transport/thingclient/` — the client every Thing uses to register with Hub
- `packages/httpclient/` — the single source of outbound `*http.Client` construction, and
  its own module: the dependency closure is the standard library plus `golang.org/x/net`,
  which is what lets `nexus-agent-core` and the standalone test modules use it without
  taking on `packages/shared`
- `docs/developers/architecture/cross-cutting/foundation/thing-model.md` — the coordination kernel
- `docs/developers/architecture/services/` — per-service architecture docs
