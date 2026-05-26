# Compliance Proxy Connecting Your SDK

The Compliance Proxy captures AI traffic by acting as an HTTPS proxy between your application and the AI provider. Configuring your SDK or tool to route through it requires two things: telling the SDK (or OS) to use the proxy address, and trusting the Nexus CA certificate so TLS validation passes. No code changes to business logic are needed — only proxy and CA settings. This page covers the five most common SDK and tool configurations: OpenAI SDK, Anthropic SDK, Cursor IDE, Claude Code, and the `curl` command line.

---

## Prerequisites

Before configuring any client, confirm the Compliance Proxy is running and the CA certificate is available:

```bash
# Check proxy liveness (runtime API on :3040)
curl -fsS http://127.0.0.1:3040/healthz

# Confirm the CA certificate exists
openssl x509 -in packages/compliance-proxy/dev-certs/ca.crt \
             -noout -subject -dates
```

In production, the CA certificate path depends on your deployment. Ask your Nexus admin for the root CA certificate file and proxy address.

The proxy port is `:3128`. The runtime API port `:3040` is not a proxy — do not configure SDKs to point at `:3040`.

## OpenAI SDK (Python)

The OpenAI SDK reads proxy configuration from the standard `HTTPS_PROXY` environment variable and from the `httpx` transport layer.

**Option A — environment variable (simplest):**

```bash
export HTTPS_PROXY=http://localhost:3128
export REQUESTS_CA_BUNDLE=/path/to/nexus-ca.crt
# or for httpx-based clients:
export SSL_CERT_FILE=/path/to/nexus-ca.crt
```

```python
from openai import OpenAI
# No proxy config in code — reads from environment
client = OpenAI(api_key="<VIRTUAL_KEY>")
response = client.chat.completions.create(
    model="gpt-4o-mini",
    messages=[{"role": "user", "content": "Hello"}],
)
```

**Option B — explicit httpx proxy in code:**

```python
import httpx
from openai import OpenAI

client = OpenAI(
    api_key="<VIRTUAL_KEY>",
    http_client=httpx.Client(
        proxy="http://localhost:3128",
        verify="/path/to/nexus-ca.crt",
    ),
)
```

After sending a request, confirm a `traffic_event` row appeared:

```sql
SELECT id, target_host, model_name, prompt_tokens
FROM traffic_event
WHERE source = 'compliance-proxy'
ORDER BY timestamp DESC LIMIT 1;
```

## Anthropic SDK (Python)

The Anthropic SDK also uses `httpx` under the hood. The same patterns apply:

**Environment variable approach:**

```bash
export HTTPS_PROXY=http://localhost:3128
export SSL_CERT_FILE=/path/to/nexus-ca.crt
```

```python
import anthropic

client = anthropic.Anthropic(api_key="<YOUR_ANTHROPIC_KEY>")
message = client.messages.create(
    model="claude-sonnet-4-6",
    max_tokens=64,
    messages=[{"role": "user", "content": "Hello"}],
)
```

**Explicit proxy in code:**

```python
import httpx
import anthropic

client = anthropic.Anthropic(
    api_key="<YOUR_ANTHROPIC_KEY>",
    http_client=httpx.Client(
        proxy="http://localhost:3128",
        verify="/path/to/nexus-ca.crt",
    ),
)
```

The Compliance Proxy captures the Anthropic wire format directly (`/v1/messages`). The resulting `traffic_event` row has `target_host='api.anthropic.com'` and `source='compliance-proxy'`.

## Cursor IDE

Cursor sends its AI traffic to `api2.cursor.sh` using a Connect-RPC + protobuf wire format. The Compliance Proxy's `ConnectRPCProtobufDetector` handles this automatically — no special configuration beyond proxy routing is needed.

To route Cursor through the proxy:

1. Open Cursor settings (gear icon or `Cmd+,` on macOS).
2. Navigate to **Network** or **Proxy** settings.
3. Set **HTTP Proxy** to `http://localhost:3128`.
4. Add the Nexus CA to your system trust store (macOS: System Keychain; see [Compliance Proxy TLS Interception](Compliance-Proxy-TLS-Interception) for platform-specific instructions).

Alternatively, set the environment variable before launching Cursor:

```bash
export HTTPS_PROXY=http://localhost:3128
open -a Cursor
```

Traffic captured from Cursor appears in the audit log as `kind=ai-chat` with `detectedSpec=cursor` and `confidence≈0.95`. The normalized payload contains the full conversation (user + assistant turns) extracted from the protobuf encoding.

To verify: open the Control Plane UI at `http://localhost:3000/traffic`, filter by `target_host=api2.cursor.sh`, and confirm normalized rows appear within a few seconds of your Cursor chat interaction.

For automated end-to-end verification of the Cursor adapter (without requiring live Cursor IDE traffic), use the synthetic test at `tests/manual/cursor_synthetic_chat.py`:

```bash
python3 tests/manual/cursor_synthetic_chat.py --proxy localhost:3128
```

## Claude Code

Claude Code (Anthropic's CLI) sends traffic to `api.anthropic.com` using the standard Anthropic Messages API. It reads proxy configuration from `HTTPS_PROXY`:

```bash
export HTTPS_PROXY=http://localhost:3128
export NODE_EXTRA_CA_CERTS=/path/to/nexus-ca.crt
claude  # or: claude "your prompt"
```

Claude Code uses Node.js under the hood. `NODE_EXTRA_CA_CERTS` adds the Nexus CA to Node's trust bundle without replacing the system CA bundle.

Verify capture:

```sql
SELECT id, target_host, model_name, prompt_tokens, completion_tokens
FROM traffic_event
WHERE source = 'compliance-proxy'
  AND target_host = 'api.anthropic.com'
ORDER BY timestamp DESC LIMIT 3;
```

## curl (direct testing)

Use `curl` with `--proxy` and `--cacert` to test proxy connectivity and verify traffic capture manually:

```bash
# Non-streaming chat via OpenAI
curl --proxy http://localhost:3128 \
     --cacert packages/compliance-proxy/dev-certs/ca.crt \
     -H "Authorization: Bearer <OPENAI_KEY>" \
     -H "Content-Type: application/json" \
     -X POST "https://api.openai.com/v1/chat/completions" \
     -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"ping"}],"max_tokens":4}'

# Streaming chat via Anthropic
curl --proxy http://localhost:3128 \
     --cacert packages/compliance-proxy/dev-certs/ca.crt \
     -H "x-api-key: <ANTHROPIC_KEY>" \
     -H "anthropic-version: 2023-06-01" \
     -H "Content-Type: application/json" \
     -N \
     -X POST "https://api.anthropic.com/v1/messages" \
     -d '{"model":"claude-haiku-4-5","max_tokens":4,"stream":true,"messages":[{"role":"user","content":"ping"}]}'
```

A 200 response means the proxy successfully MITM'd the connection. Check the `traffic_event` table for a matching row as described above.

## Common issues

| Symptom | Likely cause | Fix |
|---|---|---|
| `SSL: CERTIFICATE_VERIFY_FAILED` or `curl: (35) self-signed cert` | Nexus CA not in trust store | Install the CA certificate (see [Compliance Proxy TLS Interception](Compliance-Proxy-TLS-Interception)) |
| `curl: (56) Recv failure` after CONNECT | Target host not in `interception_domain` allowlist | Add the host via CP admin UI or ask admin to add it |
| Request succeeds but no `traffic_event` row | Audit pipeline backlog or `source != 'compliance-proxy'` filter mismatch | Check `nexus_compliance_proxy_audit_queue_depth` metric; wait a few seconds |
| `target_host` column empty | SNI parse failure on CONNECT | Report as a proxy bug |
| Gemini web traffic not captured | `gemini.google.com` uses `batchexecute` format; verify domain is in allowlist | Add `*.google.com` or `gemini.google.com` to interception domains |

---

## Canonical docs

- [`compliance-proxy-details-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/compliance-proxy/compliance-proxy-details-architecture.md) — access control, cert cache, runtime API
- [`compliance-pipeline-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/compliance-proxy/compliance-pipeline-architecture.md) — CONNECT flow, domain allowlist, TLS bump phase model
- [`test-compliance-proxy` skill](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/workflow/ai-skill-catalog.md) — automated smoke test covering multiple providers

**Adjacent wiki pages**: [Compliance Proxy TLS Interception](Compliance-Proxy-TLS-Interception) · [Compliance Proxy Overview](Compliance-Proxy-Overview) · [Your First Compliance Capture](Your-First-Compliance-Capture) · [Compliance Proxy Domain Device Predicates](Compliance-Proxy-Domain-Device-Predicates)
