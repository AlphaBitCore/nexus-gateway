# E55-S4 — HTTP/2 support on the agent bumped path

**Epic:** E55 (`docs/developers/specs/e55/e55-tls-bump-trinity.md`)
**Depends on:** E55-S5 (agent uses `shared/tlsbump`)

## User Story

> As a user running the macOS agent in front of HTTP/2-only clients (Cursor
> Electron renderer, Anthropic SDK, OpenAI SDK with `httpx[http2]`), I want
> the agent's TLS-bumped path to support HTTP/2 so my client doesn't fail
> the ALPN handshake or fall back to HTTP/1.1 with degraded performance.

## Background

Build #21 of the agent shipped `NextProtos: []string{"http/1.1"}` on the
agent's MITMRelay TLS server config (forced ALPN downgrade) because
`agent/internal/proxy/proxy.go`'s per-request loop relied on
`http.ReadRequest` which is HTTP/1.1-only. After E55-S5 deletes that file
and routes the agent through `shared/tlsbump.HandleConnection`, the same
`bump.go` ALPN dispatch (`negotiatedProto == "h2"` → `serveHTTP2`) used
by compliance-proxy applies on the agent path automatically.

## Tasks

### S4.T1 — Verify ALPN restoration
- After E55-S5 lands, `tlsbump.BumpConnection` already sets `NextProtos: []string{"h2", "http/1.1"}`. No agent-side code change required — the deletion of `agent/internal/proxy/proxy.go` removes the override.

### S4.T2 — Manual verification matrix

| Client | Test command | Expected ALPN | Expected outcome |
|---|---|---|---|
| `curl` (default) | `curl -v https://api.openai.com/v1/chat/completions ...` | `http/1.1` | Bumped, captured, 200 OK |
| `curl --http2` | `curl --http2 -v https://api.openai.com/v1/chat/completions ...` | `h2` | Bumped, captured, 200 OK |
| Anthropic SDK (Python) | `python -c "import anthropic; ..."` | `h2` (httpx default) | Bumped, captured, body decoded |
| OpenAI SDK | as above | `h2` (httpx default) | Bumped, captured, body decoded |
| Claude CLI | `claude` interactive prompt | `h2` | Streaming SSE flows in real time, captured |
| Cursor inline edit | (in app) | `h2` | Captured, body in admin UI |

## Acceptance Criteria

- [ ] All six rows in the verification matrix above pass.
- [ ] No instance of `NextProtos: []string{"http/1.1"}` remains anywhere under `packages/agent/`.
- [ ] `agent.log` shows `"protocol":"h2"` for h2 clients and `"protocol":"http/1.1"` for h1 clients on the existing `"TLS handshake completed"` log line in `shared/tlsbump/bump.go`.

## Risks

- Some clients pin h1 due to legacy behavior. Forcing h2 is fine — `NextProtos: ["h2", "http/1.1"]` advertises both, the client picks. No risk of breakage relative to compliance-proxy's behavior, which has shipped this dual-ALPN config in prod for months.
