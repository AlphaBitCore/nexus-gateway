# Your First AI Request

Nexus Gateway's AI Gateway accepts OpenAI-format requests at `/v1/chat/completions`, authenticates them with a virtual key, applies routing rules, and forwards to the configured provider. Every request produces a `traffic_event` row in PostgreSQL with latency phases, token counts, cost, and the routing trace. This page walks from a running stack to a verified end-to-end request in four steps.

---

## Prerequisites

The local stack must be running. Verify with:

```bash
curl -fsS http://localhost:3050/healthz && echo "AI Gateway up"
curl -fsS http://localhost:3001/healthz && echo "Control Plane up"
```

A real provider API key (e.g. an OpenAI key) must be configured. Without it the gateway returns a 502 when it tries to forward. See "Step 2 — Add a provider credential" below if this is the first time.

## Step 1 — Find the seeded virtual key

The dev seed creates an active virtual key. Retrieve it from the database:

```bash
docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway -t -c \
  "SELECT key FROM \"VirtualKey\" WHERE \"isActive\" = true LIMIT 1;"
```

Copy the `vk-...` value into an environment variable:

```bash
export VK="vk-<paste-here>"
```

## Step 2 — Add a provider credential

If this is a fresh clone, the seed's provider `Credential` rows contain fake encrypted values (see `tools/db-migrate/seed/seed.ts` — the seed script re-encrypts every credential with a placeholder). Add a real API key via the Control Plane UI:

1. Navigate to **Settings → Providers** in the Control Plane UI at `http://localhost:3000`.
2. Find the **OpenAI** provider and click **Add credential** (or **Edit credential** if a placeholder already exists).
3. Paste a real OpenAI API key and save.

The credential is AES-256-GCM encrypted immediately and stored. The AI Gateway decrypts it at request time using `CREDENTIAL_ENCRYPTION_KEY` from the repo-root `.env`.

## Step 3 — Send the request

```bash
curl -sS http://localhost:3050/v1/chat/completions \
  -H "Authorization: Bearer $VK" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "messages": [{"role": "user", "content": "What is the capital of Japan?"}]
  }' | jq .
```

Expected response shape (HTTP 200):

```json
{
  "id": "chatcmpl-...",
  "object": "chat.completion",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "The capital of Japan is Tokyo."
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 15,
    "completion_tokens": 8,
    "total_tokens": 23
  }
}
```

The `x-nexus-request-id` response header contains the request identifier. Copy it if you want to look up the exact row in the next step.

## Step 4 — Inspect the traffic_event row

The Hub's MQ consumer writes the `traffic_event` row asynchronously after the response completes. Allow 1–2 seconds, then query:

```bash
docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway -c \
  "SELECT id, source, kind, cost FROM traffic_event ORDER BY created_at DESC LIMIT 1;"
```

A successful round-trip produces a row similar to:

```
                  id                  |   source   |    kind     |  cost
--------------------------------------+------------+-------------+---------
 req_01j9abc...                       | ai-gateway | chat        | 0.00003
```

For richer output including routing trace and latency phases:

```bash
docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway -c "
    SELECT
      request_id,
      ingress_model,
      resolved_provider,
      resolved_model,
      total_tokens,
      latency_phase_total_ms
    FROM traffic_event
    ORDER BY ts DESC
    LIMIT 1;"
```

`resolved_provider` confirms which provider the gateway forwarded to. `latency_phase_total_ms` is the end-to-end wall time; the gateway's own overhead (routing, virtual-key auth, hook evaluation) is captured in the `latency_phase_gateway_*` columns.

## Exploring further

After the first successful request, the Control Plane UI surfaces several things worth exploring:

- **Traffic page** — click the request row to open the full drawer: routing trace, hook decisions inline, token counts, cost, and latency phases by stage.
- **Hooks** — navigate to **Hooks** and enable the `keyword-filter` built-in with the keyword `Japan`. Re-run the same request. The hook blocks the request at the request stage; the `traffic_event` row's `request_hook_decision` column records the block reason.
- **AI Gateway** virtual key detail — shows per-VK quota usage, request count, and cost totals.

The full annotated walkthrough for this scenario lives in [`examples/01-hello-world/README.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/examples/01-hello-world/README.md).

---

## Canonical docs

- [`examples/01-hello-world/README.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/examples/01-hello-world/README.md) — the full annotated walkthrough with "what's happening under the hood" pointers
- [`routing-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/routing-architecture.md) — how routing rules are evaluated
- [`cost-estimation-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md) — where the `cost` column value comes from

**Adjacent wiki pages**: [Quickstart](Quickstart) · [First Admin Login](First-Admin-Login) · [Your First Compliance Capture](Your-First-Compliance-Capture) · [Examples Catalog](Examples-Catalog) · [AI Gateway Overview](AI-Gateway-Overview)
