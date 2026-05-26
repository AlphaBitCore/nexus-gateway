# Your First Compliance Capture

The Compliance Proxy intercepts HTTPS traffic at the network layer without requiring any code change in the client application. Any process that honours the `HTTPS_PROXY` environment variable — the OpenAI Python SDK, the Anthropic SDK, curl, Cursor IDE, and most HTTP clients — routes through the proxy once the variable is set and the dev root CA is trusted. This page walks through configuring the OpenAI Python SDK, triggering a capture, and verifying the resulting `traffic_event` row.

---

## How the Compliance Proxy works

The Compliance Proxy listens on two ports:

- **`:3128`** — the HTTPS proxy port. Clients point `HTTPS_PROXY=http://localhost:3128` here. The proxy handles `CONNECT` tunnels and performs TLS interception (MITM) for allowed hosts.
- **`:3040`** — the runtime API (healthz, kill-switch).

For TLS interception to work the dev root CA certificate must be trusted by the client. The bootstrap script generates the CA at `packages/compliance-proxy/dev-certs/ca.crt`.

The proxy writes a `traffic_event` row for every intercepted request. The `source` column is set to `'compliance-proxy'`; this is the primary way to distinguish proxy-captured rows from AI Gateway rows.

## Step 1 — Verify the Compliance Proxy is running

```bash
curl -fsS http://localhost:3040/healthz && echo "Compliance Proxy runtime API up"
test -f packages/compliance-proxy/dev-certs/ca.crt && echo "Dev CA present"
```

If the CA file is missing, regenerate it:

```bash
cd packages/compliance-proxy
mkdir -p dev-certs
openssl ecparam -name prime256v1 -genkey -noout -out dev-certs/ca.key
openssl req -new -x509 -key dev-certs/ca.key -out dev-certs/ca.crt -days 365 \
  -subj "/O=Nexus Dev/CN=Nexus Compliance Proxy Dev CA"
```

Then restart the Compliance Proxy.

## Step 2 — Configure the OpenAI Python SDK

In a Python environment with `openai` installed, set the proxy and CA trust:

```bash
export HTTPS_PROXY="http://localhost:3128"
export SSL_CERT_FILE="$(pwd)/packages/compliance-proxy/dev-certs/ca.crt"
```

`SSL_CERT_FILE` is the standard env var read by Python's `ssl` module (and therefore by the `httpx` / `requests` libraries that the OpenAI SDK uses). Setting it makes the SDK trust the dev CA in addition to the system bundle — no system CA store modification is needed.

Alternatively, pass the CA path directly to the OpenAI client:

```python
import httpx
import openai

client = openai.OpenAI(
    api_key="sk-...",          # your real OpenAI API key
    http_client=httpx.Client(
        proxies="http://localhost:3128",
        verify="packages/compliance-proxy/dev-certs/ca.crt",
    ),
)
```

## Step 3 — Trigger a completion

With `HTTPS_PROXY` and `SSL_CERT_FILE` set, run a standard OpenAI call:

```python
response = client.chat.completions.create(
    model="gpt-4o-mini",
    messages=[{"role": "user", "content": "Reply with one word: hello"}],
)
print(response.choices[0].message.content)
```

Or with curl:

```bash
curl -sS \
  --proxy http://localhost:3128 \
  --cacert packages/compliance-proxy/dev-certs/ca.crt \
  https://api.openai.com/v1/chat/completions \
  -H "Authorization: Bearer sk-..." \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Reply with one word: hello"}]}' \
  | jq .choices[0].message.content
```

The request flows through the proxy, which decrypts the TLS layer, inspects the body, applies configured hooks, re-encrypts to OpenAI, and relays the response back.

## Step 4 — Verify the traffic_event row

Allow 1–2 seconds for the Hub's MQ consumer to write the row, then query:

```bash
docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway -c \
  "SELECT id, source, kind, cost FROM traffic_event ORDER BY created_at DESC LIMIT 1;"
```

For the compliance-proxy path, `source = 'compliance-proxy'`. A successful capture looks like:

```
                  id                  |       source        |  kind  |  cost
--------------------------------------+---------------------+--------+---------
 evt_...                              | compliance-proxy    | chat   | 0.00002
```

To also see the target host and extraction status:

```bash
docker exec $(docker ps --filter "name=postgres" -q | head -1) \
  psql -U postgres -d nexus_gateway -c "
    SELECT
      id, source, target_host,
      model_name, total_tokens,
      usage_extraction_status
    FROM traffic_event
    WHERE source = 'compliance-proxy'
    ORDER BY created_at DESC LIMIT 1;"
```

`target_host` should be `api.openai.com`. `usage_extraction_status` should be `ok` or `streaming_reported`.

## Host allowlist requirement

The Compliance Proxy only intercepts hosts listed in the `interception_domain` table. If a provider host is not listed, the proxy passes through the connection without inspection and does NOT write a `traffic_event`. The dev seed includes entries for the major providers (OpenAI, Anthropic, Google). If you are testing a custom provider host, add an entry via the Control Plane UI under **Compliance Proxy → Interception Domains**.

## Full skill recipe

For a comprehensive multi-provider smoke test of the Compliance Proxy — covering non-streaming, streaming, DB verification, and Prometheus metrics delta — invoke the `/test-compliance-proxy` skill from Claude Code. The skill encodes the exact provider adapter paths, DB queries, and fix-build-restart loop for each failure mode. The canonical recipe lives in [`.claude/skills/test-compliance-proxy/SKILL.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/.claude/skills/test-compliance-proxy/SKILL.md).

---

## Canonical docs

- [`compliance-proxy-details-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/compliance-proxy/compliance-proxy-details-architecture.md) — proxy internals, MITM mechanics, hook pipeline
- [`compliance-pipeline-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/compliance-proxy/compliance-pipeline-architecture.md) — TLS interception and root CA install details

**Adjacent wiki pages**: [Quickstart](Quickstart) · [Your First AI Request](Your-First-AI-Request) · [Compliance Proxy Overview](Compliance-Proxy-Overview) · [Compliance Proxy TLS Interception](Compliance-Proxy-TLS-Interception) · [Compliance Proxy Connecting Your SDK](Compliance-Proxy-Connecting-Your-SDK)
