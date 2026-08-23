# User documentation

Documentation for administrators, integrators, and end users of Nexus Gateway.

## Contents

- [`product/`](./product/) — what Nexus is and how it is deployed:
  - [`overview.md`](./product/overview.md) — product overview and positioning.
  - [`features.md`](./product/features.md) — the capability matrix.
  - [`architecture.md`](./product/architecture.md) — the user-facing architecture.
  - [`deployment-models.md`](./product/deployment-models.md) — SaaS, self-hosted, and air-gapped.
- [`features/`](./features/) — per-surface feature guides:
  - [`cp-ui/`](./features/cp-ui/) — one guide per Control Plane console section.
  - [`agent-ui/`](./features/agent-ui/) — one guide per Desktop Agent dashboard page.
- [`api/`](./api/) — API reference:
  - [`api/openai-sdk-compatibility.md`](./api/openai-sdk-compatibility.md) — using the official OpenAI SDKs against the gateway: supported surface, deliberate divergences, error mapping.
  - [`api/openapi/`](./api/openapi/) — OpenAPI 3.1 YAML files per epic/story (Control Plane admin API, AI Gateway /v1 surface).

For the full documentation map, see [`docs/README.md`](../README.md).
