# Examples Catalog

The `examples/` directory contains runnable demonstrations of the gateway after the local stack is up. Each example is a self-contained subdirectory with its own `README.md` and copy-pasteable shell commands. Examples assume the dev seed is applied and the services are running — they pick up the seeded virtual key, provider rows, and routing rules rather than requiring fresh admin configuration.

---

## Current examples

The directory contains one example:

| # | Path | What it demonstrates | Time |
|---|---|---|---|
| 1 | [`examples/01-hello-world/`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/examples/01-hello-world/) | End-to-end chat completion through the AI Gateway, then reading the audit `traffic_event` row. | ~3 minutes |

More examples are roadmap items. The per-section feature docs under [`docs/users/features/`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/) cover additional use cases in prose form until dedicated runnable examples land.

## Example 01 — Hello world

`examples/01-hello-world/` is the canonical first-run demo. It walks the following sequence:

1. The gateway authenticates a virtual key and resolves the matching routing rule.
2. The request is forwarded to the configured provider (OpenAI by default).
3. The provider response streams to the terminal.
4. The Hub's MQ consumer writes a `traffic_event` row with latency phases, token counts, and the routing trace.
5. The example queries that row directly in Postgres and prints it.

**What you learn:** how to use a virtual key, how the routing rule resolves `model: gpt-4o-mini` to a provider, and what a `traffic_event` row looks like after a real round-trip. The `README.md` also pointers to the seven gateway-internal subpackages in play (vkauth, requestcontext, hooks, router, executor, streaming, audit) so contributors can trace the flow into the code.

**Prerequisites:** local stack up (`./scripts/dev-start.sh` completed), a real OpenAI API key set in Settings → Providers. The example retrieves the seeded virtual key from the DB rather than requiring a manually created one.

**Extending the example:** the `README.md` lists three follow-on exercises — adding a second turn to the request to watch `total_tokens` grow, enabling the `keyword-filter` hook to see a block, and finding the request in the Traffic drawer in the Control Plane UI.

## Running any example

Every example assumes `./scripts/dev-start.sh` has been run and all five services are up. Verify before starting:

```bash
curl -fsS http://localhost:3050/healthz   # AI Gateway
curl -fsS http://localhost:3060/healthz   # Hub
curl -fsS http://localhost:3001/healthz   # Control Plane
```

If any of those fail, return to [Quickstart](Quickstart) before continuing.

## Conventions shared across examples

- Examples use the **seeded** virtual key, providers, and models. No manual configuration is needed beyond adding a real provider API key via Settings → Providers.
- `curl` targets `localhost` on the documented ports. For a self-hosted deployment substitute `localhost:<port>` with the service URL.
- Examples use `admin@nexus.ai / admin123` for any admin-API call. The `tests/lib/auth.sh` helpers (`cp_login` / `cp_curl`) wrap the OAuth + PKCE flow for scripted admin calls.

## Contributing a new example

New examples follow the same directory convention: a self-contained subdirectory under `examples/` with a `README.md` that includes: prerequisites, step-by-step run instructions, expected output, what to try next, and a "what's happening under the hood" section that links to the relevant architecture docs. Examples must use the seeded data rather than creating state in the DB — this keeps them runnable without side effects on a fresh clone.

The capability areas most likely to yield useful new examples include: streaming completions (SSE), multi-turn conversation with token growth, compliance-proxy MITM capture, routing rule override via header, cost and cache ROI via the Traffic drawer, and provider failover via a smart routing rule. Each of these is already documented in feature docs under [`docs/users/features/`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/features/) — an example would add the runnable shell companion.

## What examples are intentionally not

Examples are runnable demonstrations on a local stack, not production integration recipes. They do not cover HA deployments, production credential rotation, SIEM forwarding configuration, or agent fleet management at scale. Those topics live in the [Operations](Operations-Runbook-Index) and [Deployment](Deployment-Models) wiki sections.

## Using examples as test fixtures

The `examples/01-hello-world/` curl commands are copy-pasteable into a smoke-test script. The AI Gateway smoke infrastructure in `tests/scripts/smoke-gateway.py` uses the same conceptual pattern — iterate over providers, send a minimal request, verify a `traffic_event` row — but targets 29 models across 4 ingresses with detailed assertion logic. If building an integration test for a fork, the hello-world example is a natural starting point: it covers the happy path end-to-end in the fewest lines. The smoke script covers the full provider matrix and failure mode taxonomy.

The seeded virtual key's exact value changes on each fresh `./scripts/dev-start.sh --force-reset` run because the seed applies a snapshot that contains a specific row; the key itself is deterministic from the snapshot. The DB query in the example (`SELECT key FROM "VirtualKey" WHERE "isActive" = true LIMIT 1`) is the correct way to retrieve it rather than hardcoding the value.

## Adding examples to the repo

New example directories follow the same naming convention: `examples/NN-short-name/` where `NN` is a zero-padded integer. Each directory requires a `README.md`. The top-level `examples/README.md` table is updated to include the new entry. Examples do not have their own `go.mod` — they use the repo's `go.work` workspace if they contain Go code. Shell-only examples have no Go files and do not require any language setup beyond the base prerequisites.

---

## Canonical docs

- [`examples/README.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/examples/README.md) — conventions and prerequisites for the examples directory
- [`examples/01-hello-world/README.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/examples/01-hello-world/README.md) — the annotated first example

**Adjacent wiki pages**: [Quickstart](Quickstart) · [Your First AI Request](Your-First-AI-Request) · [AI Gateway Overview](AI-Gateway-Overview) · [Troubleshooting First Run](Troubleshooting-First-Run)
