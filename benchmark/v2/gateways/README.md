# Local LLM Gateways — LiteLLM & Bifrost

Two local OpenAI-compatible gateways for benchmarking, both proxying to
`openai/gpt-4o-mini` using `OPENAI_API_KEY` from `../.env.local`.

| Service | Port | Auth | Model name in requests |
|---------|------|------|------------------------|
| LiteLLM | 4000 | `Authorization: Bearer sk-local-dev` (required) | `gpt-4o-mini` |
| Bifrost | 8080 | none (open) | `openai/gpt-4o-mini` |

Keys live in `../.env.local`: `LITELLM_API_KEY=sk-local-dev`, `BIFROST_API_KEY=local-dev`.

## View the keys
```bash
grep -E 'LITELLM_API_KEY|BIFROST_API_KEY|OPENAI_API_KEY' ../.env.local
# or load into shell:
set -a; source ../.env.local; set +a; echo "$LITELLM_API_KEY"
```

## Health checks
```bash
curl -H "Authorization: Bearer sk-local-dev" http://localhost:4000/health   # 200 (plain curl = 401, auth-gated)
curl http://localhost:4000/health/liveliness                                # "I'm alive!"
curl http://localhost:8080/health                                           # {"status":"ok"}
```

## Test a completion
```bash
# LiteLLM
curl http://localhost:4000/v1/chat/completions \
  -H "Authorization: Bearer sk-local-dev" -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"say pong"}]}'

# Bifrost  (model prefixed with openai/)
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"say pong"}]}'
```

## Run / manage
```bash
docker ps
docker logs -f litellm        # or bifrost
docker restart litellm bifrost
docker rm -f litellm bifrost  # stop + remove

# Re-create from configs in this folder:
OPENAI_KEY=$(grep '^OPENAI_API_KEY=' ../.env.local | cut -d= -f2-)
docker run -d --name litellm -p 4000:4000 \
  -e OPENAI_API_KEY="$OPENAI_KEY" -e LITELLM_MASTER_KEY=sk-local-dev \
  -v "$PWD/litellm-config.yaml:/app/config.yaml:ro" \
  ghcr.io/berriai/litellm:main-latest --config /app/config.yaml --port 4000
docker run -d --name bifrost -p 8080:8080 \
  -e OPENAI_API_KEY="$OPENAI_KEY" -v "$PWD/bifrost-data:/app/data" \
  maximhq/bifrost:latest
```
