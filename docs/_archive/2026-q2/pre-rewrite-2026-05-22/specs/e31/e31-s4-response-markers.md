# E31-S4: Nexus Response Markers

**Epic:** E31
**Story:** S4 (response markers — S1/S2/S3 are simulator / cache / passthrough)
**Status:** Implemented (2026-04-27)

## User Story

> As an operator, I want to identify which Nexus services processed a request and what they did from the response headers alone, so I can debug interception behavior without reading audit logs.

## Tasks (mapped to implementation plan phases)

| Phase | Task | Package(s) |
|---|---|---|
| 0 | Extract `packages/shared/transport/responseio` HTTP response copy helper (reused by all data-plane services) | `shared/responseio` |
| 1 | Add marker helpers to `packages/shared/traffic`: `PrependVia`, `MergeExposeHeaders`, `SetExposeHeaders`, `FormatHookOutcome`, `ExposeHeaders` slice | `shared/traffic` |
| 2 | AI Gateway header rename + new fields (mode, hook, coerced) + CORS expose on success and reject paths | `ai-gateway` |
| 3 | Compliance Proxy markers on MITM success (non-streaming + SSE) and reject paths; dynamic hop-by-hop strip | `compliance-proxy` |
| 4 | Agent markers on MITM success and reject paths; negative test confirming markers absent on tunnel flows | `agent` |
| 5 | Cross-service E2E integration test + documentation (this story) | `shared/traffic`, docs |

## Acceptance Criteria

| ID | Criterion |
|---|---|
| AC-1 | AI Gateway emits the full `x-nexus-aigw-*` set (request-id, mode, hook, provider, model, routed-provider, routed-model, routing-rule, cache, stream flag, latency, retry-count, quota fields, coerced) on both success and reject paths. |
| AC-2 | Compliance Proxy emits `x-nexus-cp-*` (request-id, mode, hook, domain-rule) on MITM success and reject paths; markers are absent on tunnel/forwarding flows. |
| AC-3 | Agent emits `x-nexus-agent-*` (flow-id, mode, hook, domain-rule) on MITM success and reject paths; markers are absent on tunnel flows (verified by negative assertion in Phase 4 test). |
| AC-4 | `x-nexus-via` chain reads in request flow order: `agent, compliance-proxy, ai-gateway` for the full chain; single-service paths emit just that service's name. |
| AC-5 | Hook outcome format matches spec §4.5: `none`, `passed:<hook1>,<hook2>`, `transformed:<hook1>`, `rejected:<hook>:<reason-slug>`; reject reason is sanitized to `[a-z0-9-]+`. |
| AC-6 | All affected service test suites pass under `go test -race -count=1` with zero data-race findings. |
| AC-7 | `Access-Control-Expose-Headers` on every marked response includes the full marker list so browser-side `fetch()` can read them. |
| AC-8 | Streaming responses omit response-side dimensions (cache, latency, quota-used, quota-remaining) per spec §5; those values are captured in the audit event instead. |

## Related Documents

- Spec: `docs/_archive/2026-q2/brainstorms/2026-04-27-nexus-response-markers-design.md`
- Plan: `docs/_archive/2026-q2/brainstorms/plans/2026-04-27-nexus-response-markers.md`
- Reference: `docs/developers/architecture/cross-cutting/foundation/nexus-response-markers.md`
- Requirements: `docs/developers/specs/e31/e31-response-markers.md`
- OpenAPI: `docs/users/api/openapi/ai-gateway/e31-s4-response-markers.yaml`
