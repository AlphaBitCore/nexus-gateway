# E55-S3 — Payload Capture trinity (cp + agent share `shared/payloadcapture` + spillstore)

**Epic:** E55 (`docs/developers/specs/e55/e55-tls-bump-trinity.md`)
**Depends on:** E55-S1

## User Story

> As an admin enabling request/response body capture for compliance review,
> I want bodies captured at the agent ingress to land in the same
> `traffic_event_payload` table — including spill-to-S3 for oversized
> bodies — that compliance-proxy already populates, so my "Body" tab in the
> admin UI Traffic detail page shows agent-flow bodies the same as
> compliance-proxy-flow bodies.

## Tasks

### S3.T1 — Agent thingclient registers `payload_capture.config`
- Per `shared/payloadcapture/store.go`, register the Cat B Thing config key `payload_capture.config` with `needsPull: true`.
- Decode via the existing `payloadcapture.DecodeConfigJSON` and atomically swap into a `*payloadcapture.Store`.
- Wire that store into `tlsbump.Deps.PayloadCapture` per inbound bridge connection.

### S3.T2 — Per-host overrides
- `shared/tlsbump/forward_handler.go` already reads per-host capture override from the matched `interception_domain` row (`captureRequestBody`, `captureResponseBody`, `rawBodySpillEnabled` columns) merged over the global Store config. After S1.T3 this code path runs identically for cp and agent.

### S3.T3 — SpillStore wiring on agent
- The agent process must initialise a `spillstore` backend at boot per the runtime config:
  - **localfs** (default for dev / first launch) — writes spill files under `${platformPaths.DataDir}/spill/`.
  - **s3** (enabled when admin configures S3 credentials via the agent settings shadow blob — same shape as the cp / hub config).
- `tlsbump.Deps.AuditEmitter.WithSpillStore(store)` is called once at agent boot with the chosen backend. After that the existing emitter logic in `shared/compliance/audit_emitter.go` handles inline-vs-spill decision per request based on `payloadcapture.Config.MaxInlineBodyBytes`.

### S3.T4 — Network caps
- `payloadcapture.Config.MaxRequestBytes` (default 10 MiB) — agent forward handler rejects oversized inbound request bodies with HTTP 413 before forwarding upstream. Already done by shared forward_handler; verify the agent ingress wrapper does not bypass this.
- `payloadcapture.Config.MaxResponseBytes` (default 10 MiB) — non-streaming upstream response cap. Streaming responses are bounded by `streaming/policy.Policy.MaxBufferBytes` instead.

## Acceptance Criteria

- [ ] After enabling `storeRequestBody = true` in admin UI, an agent-bridged POST to `api.openai.com/v1/chat/completions` produces a `traffic_event_payload` row with `inline_request_body` populated (≤ 256 KiB) or `request_body_spill_ref` populated (> 256 KiB).
- [ ] `storeResponseBody = true` captures both streaming (SSE) and non-streaming response bodies; SSE bodies appear after stream end.
- [ ] Per-host override `captureRequestBody = false` on `api.anthropic.com` while global is `true` results in zero capture for anthropic flows and capture for other inspect-mode flows.
- [ ] Oversized request body (> `MaxRequestBytes`) returns HTTP 413 to the client without reaching the upstream.
- [ ] Local spill files appear under `${DataDir}/spill/` when global storage backend is `localfs`; S3 objects appear in the configured bucket when backend is `s3`.

## Risks / open items

- Agent's `${platformPaths.DataDir}` differs per OS (per `feedback_agent_platform_paths_abstraction` memory). All path construction goes through `platform.DefaultPaths()`.
- Agent must NOT send spill files larger than admin-set caps — `payloadcapture.Config.MaxResponseBytes` for non-streaming, `streaming/policy.Policy.MaxBufferBytes` for streaming.
