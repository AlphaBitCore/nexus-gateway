# E55-S5 — Agent NE Bridge ingress wraps `shared/tlsbump.HandleConnection`

**Epic:** E55 (`docs/developers/specs/e55/e55-tls-bump-trinity.md`)
**Depends on:** E55-S1, E55-S2, E55-S3

## User Story

> As an agent process accepting hijacked flows from the macOS NETransparentProxy
> Swift extension, I want to terminate that bridge connection in a thin
> ingress wrapper that hands the bytes off to the same `shared/tlsbump`
> core compliance-proxy uses, so any feature shipped in tlsbump
> automatically applies to agent flows.

## Background

The Swift NETransparentProxyProvider redirects inspect-mode flows (host on
`interception_domain` with `action = inspect` AND not bundle-allowlisted)
to `127.0.0.1:9443` on the daemon. The Swift side prepends a textual header
`BRIDGE host:port flowID\n` then relays opaque bytes. The daemon currently
hand-rolls TLS bump, HTTP/1.1 request loop, inspect, and forward in
`packages/agent/core/network/proxy/proxy.go` (~965 LOC).

## Tasks

### S5.T1 — Replace `agent/internal/proxy/proxy.go` with `bridge.go`
- Delete `packages/agent/core/network/proxy/proxy.go`, `marker.go`, `tags.go`, `relay_integration_test.go`, `marker_test.go`.
- Create `packages/agent/core/network/proxy/bridge.go` (~150 LOC target):

```go
package proxy

// StartBridge listens on 127.0.0.1:9443 for Swift NE bridge connections.
// Each accepted conn:
//  1. parses the BRIDGE header (host, port, flowID)
//  2. constructs a tlsbump.Deps from the agent's runtime state
//  3. calls tlsbump.HandleConnection(ctx, conn, dst, deps)
//  4. on return, stamps the inspect outcome back to the Swift side via StampInspectFromBridge
func StartBridge(ctx context.Context, addr string, deps Deps) error
```

- The `agent/internal/proxy.Deps` struct embeds `tlsbump.Deps` plus agent-specific extras (the `StampInspectFromBridge` callback into the Swift IPC layer, the bridge handler logger).

### S5.T2 — Wire agent's audit pipeline as `tlsbump.Deps.AuditEmitter`
- Agent's `internal/audit.Queue.WriterAdapter` (defined in S1.T1) satisfies `shared/audit.Writer`.
- Construct `compliance.NewAuditEmitter(queueWriter, logger)` once at agent boot.
- Inject the same emitter into `tlsbump.Deps` per bridge connection.

### S5.T3 — Wire agent's device CA as `tlsbump.Deps.GetCert`
- Agent's existing `agentTLS.IssueLeafCert(host)` already mints leaf certs from the device CA established at enrollment. Wrap it in the `func(*tls.ClientHelloInfo) (*tls.Certificate, error)` shape `tlsbump` expects.

### S5.T4 — Wire agent's PolicyResolver + DomainEngine + AdapterRegistry
- Agent already runs the same `shared/compliance.PolicyResolver` (ingressType = "agent") via `internal/policy/engine.go`. Reuse that instance for `tlsbump.Deps.PolicyResolver`.
- After S1.T3 moves `domainpolicy` to shared, agent constructs a `domainpolicy.Engine` from the snapshot it already pulls via thingclient.
- `traffic.AdapterRegistry` is constructed once at agent boot from `shared/traffic`'s registered adapters.

### S5.T5 — Wire `StampInspectFromBridge` after `HandleConnection` returns
- The bridge wrapper preserves the existing flow-stamp callback so the Swift NE side gets the per-flow `provider`, `model`, `method`, `path`, `requestBytes`, `responseBytes`, `decision`, `hookDecision` for the agent UI's recent-events dashboard. Existing `BridgeInspectStamp` struct in `agent/internal/platform/darwin.go` is the destination.
- After the refactor: `tlsbump.HandleConnection` returns a `tlsbump.Outcome` struct with all fields needed for the stamp; the bridge wrapper translates `Outcome` → `BridgeInspectStamp` and calls `StampInspectFromBridge`.

### S5.T6 — Self-intercept guard preserved
- The existing daemon-PID write to `/var/run/nexus-agent/daemon.pid` (read by `DaemonPIDFilter.swift`) stays — it's a Swift-side concern. The bridge wrapper need not change.

### S5.T7 — Detailed branch logging
- Per F-7 in the requirements doc: structured INFO logs at every decision branch in:
  - `bridge.go` — accept, header parse, dest resolved, tlsbump call entered, tlsbump returned (with outcome summary), stamp called.
  - `tlsbump.HandleConnection` — adds an entry log + an exit log with the Outcome (already present in many spots; sweep for gaps).
  - `forward_handler.go` — domain match, inspect/passthrough decision, request hook outcome, upstream relay outcome, response hook outcome, body capture decision.
- Acceptance: `tail -200 agent.log | grep -E "bridge|tlsbump|forward|inspect"` after one bridged curl shows ≥ 8 lines covering the full lifecycle with non-empty fields.

## Acceptance Criteria

- [ ] `find packages/agent/core/network/proxy -type f -name '*.go' | wc -l` ≤ 4 (bridge.go + bridge_test.go + go files for the Deps wiring).
- [ ] `wc -l packages/agent/core/network/proxy/*.go` total ≤ 400.
- [ ] `go test -race -count=1 ./packages/agent/...` passes.
- [ ] After build + install, `curl https://api.openai.com/v1/chat/completions -d '{"model":"gpt-3.5-turbo","messages":[{"role":"user","content":"hi"}]}'` through the agent: `traffic_event` row has `method='POST'`, `path='/v1/chat/completions'`, `provider_name='openai'`, `model_name='gpt-3.5-turbo'`, `inline_request_body` populated, `inline_response_body` populated.
- [ ] Streaming variant (`-d '{... "stream":true}'`) — bytes flow real-time, response body captured on stream end.
- [ ] Claude CLI command completes successfully (no `Retrying in 34s · attempt 8/10`); request + response captured in admin UI.
- [ ] Cursor inline edit: captured in admin UI.

## Risks

- Several E55-related bugs (non-streaming hang, method/path empty, bridge stamp race) likely auto-resolve once the duplicate proxy.go is gone and agent is on the proven cp code path. If any persist after S5.T1, instrument with logs (S5.T7) + reproduce + fix in this same PR series — no defer.
- macOS NE fail-open invariant: if `tlsbump.HandleConnection` panics or hangs, the bridge wrapper MUST recover/timeout and close the conn — never let a hung tlsbump block the NE provider's flow lifecycle. Add a 30 s outer timeout in bridge.go.
