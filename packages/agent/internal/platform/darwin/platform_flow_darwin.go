//go:build darwin

// platform_flow_darwin.go — the NE per-flow decision path split out of
// platform_darwin.go by responsibility: the lock-free flow registry
// (sync.Map accessors), the bounded-worker decision dispatch, and the
// flow lifecycle handlers (new / update-host / closed). The decision is
// computed off the IPC reader goroutine so one slow flow never stalls
// the others; flow state is published single-writer via flow.State.Ready.
package darwin

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/darwin/flow"
	nepkg "github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/darwin/ne"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/darwin/proc"
)

// KillSwitchGater is implemented by handlers that can short-circuit
// flow tracking when protection is paused or the kill switch is
// engaged. Without this gate the daemon would still write audit
// rows for every flow during a user-paused session, which
// contradicts user expectation that "paused = invisible".
type KillSwitchGater interface {
	IsKillSwitchEngaged() bool
}

// passthroughFrame returns the newline-framed JSON for a fail-open /
// gated passthrough decision.
func passthroughFrame(flowID string) []byte {
	data, _ := json.Marshal(nepkg.DecisionMsg{FlowID: flowID, Decision: "passthrough"})
	return append(data, '\n')
}

// decisionString maps the platform decision to the NE wire token.
func decisionString(d api.Decision) string {
	switch d {
	case api.DecisionInspect:
		return "inspect"
	case api.DecisionDeny:
		return "deny"
	default:
		return "passthrough"
	}
}

// storeFlow registers a flow and keeps the count accurate.
func (p *DarwinPlatform) storeFlow(fs *flow.State) {
	if _, loaded := p.activeFlows.LoadOrStore(fs.FlowID, fs); !loaded {
		p.activeFlowsLen.Add(1)
	}
}

// loadAndDeleteFlow removes a flow, returning it if present.
func (p *DarwinPlatform) loadAndDeleteFlow(flowID string) (*flow.State, bool) {
	v, ok := p.activeFlows.LoadAndDelete(flowID)
	if !ok {
		return nil, false
	}
	p.activeFlowsLen.Add(-1)
	return v.(*flow.State), true
}

// loadFlow returns the tracked flow if present.
func (p *DarwinPlatform) loadFlow(flowID string) (*flow.State, bool) {
	v, ok := p.activeFlows.Load(flowID)
	if !ok {
		return nil, false
	}
	return v.(*flow.State), true
}

func (p *DarwinPlatform) handleNewFlow(nc *neConn, msg nepkg.FlowMsg) {
	if p.handler == nil {
		slog.Warn("NE flow_new dropped — handler not yet wired",
			"flow_id", msg.FlowID, "remote", msg.RemoteHost,
		)
		return
	}

	p.lastFlowAtNS.Store(time.Now().UnixNano())

	// Pause / kill-switch gate: when protection is off we still reply
	// passthrough (so the flow runs natively) but MUST NOT track the
	// flow or write an audit row — paused state must stay invisible.
	if g, ok := p.handler.(KillSwitchGater); ok && g.IsKillSwitchEngaged() {
		nc.reply(passthroughFrame(msg.FlowID))
		return
	}

	// Backpressure gate: shed load to passthrough when the audit queue
	// is over-full; do not track (an audit row for a shed flow would
	// just deepen the backlog).
	if p.backpressure != nil && p.backpressure.IsThrottled() {
		slog.Info("NE flow_new shed via backpressure (audit queue over high-water)",
			"flow_id", msg.FlowID, "remote", msg.RemoteHost,
		)
		nc.reply(passthroughFrame(msg.FlowID))
		return
	}

	// Register the flow SYNCHRONOUSLY here on the reader goroutine, with
	// the identity fields known from flow_new, BEFORE handing the slow
	// decision off to a worker. Because the IPC socket delivers frames in
	// order and a flow's flow_update_host / flow_closed can only follow
	// its flow_new, this guarantees those later handlers always find the
	// flow — the worker fills the process + decision fields and publishes
	// them via fs.Ready.
	fs := &flow.State{
		FlowID:    msg.FlowID,
		DstIP:     msg.RemoteIP,
		DstPort:   msg.RemotePort,
		SrcPort:   msg.LocalPort,
		StartedAt: time.Now(),
	}
	fs.SetDstHost(msg.RemoteHost)
	p.storeFlow(fs)

	// Hand the slow part (cached process lookup + policy eval +
	// connection-stage hook pipeline) to a bounded worker so one slow
	// flow no longer stalls every other flow behind the single IPC
	// reader. The flowSem slot is acquired INSIDE the worker, not here,
	// so even a saturated pool never blocks the reader — flow_closed /
	// flow_update_host frames keep flowing, and parked workers cost only
	// a goroutine each (bounded by the NE's own in-flight flow count). A
	// decision delayed past the NE's 2 s timeout fails open to passthrough
	// on the extension side.
	nc.workers.Add(1)
	go func() {
		defer nc.workers.Done()
		p.flowSem <- struct{}{}
		defer func() { <-p.flowSem }()
		p.decideFlow(nc, fs, msg)
	}()
}

// decideFlow runs the per-flow decision off the IPC reader goroutine:
// resolve the process (cached), evaluate policy + connection-stage hooks,
// publish the result onto the flow state, and reply to the extension.
func (p *DarwinPlatform) decideFlow(nc *neConn, fs *flow.State, msg nepkg.FlowMsg) {
	var procMeta api.ProcessMeta
	if msg.PID > 0 {
		// Cached by PID: a browser opening many connections from one
		// process must not re-read the same Info.plist per flow.
		if m, err := proc.ProcessInfoCached(msg.PID); err == nil {
			procMeta = api.ProcessMeta{
				PID:      m.PID,
				Path:     m.Path,
				Name:     m.Name,
				BundleID: m.BundleID,
				User:     m.User,
			}
		} else {
			slog.Debug("NE flow_new: ProcessInfo failed (non-fatal)",
				"flow_id", msg.FlowID, "pid", msg.PID, "error", err,
			)
		}
	}

	// Prefer the NE's kernel-attested source-app signing identifier over
	// the PID→Info.plist lookup above. The PID path is racy (the OS can
	// reuse a PID before the bridge connection arrives) and resolves to
	// empty / garbage for sandboxed and CLI-helper processes (e.g. Cursor's
	// `node` helper reports a version string as its name and no bundle).
	// sourceAppSigningIdentifier is the stable, authoritative attribution
	// macOS hands the flow; use it whenever the extension supplied one.
	if msg.BundleID != "" {
		procMeta.BundleID = msg.BundleID
	}

	intercepted := api.InterceptedConn{
		FlowID:  msg.FlowID,
		DstHost: msg.RemoteHost,
		DstIP:   msg.RemoteIP,
		DstPort: msg.RemotePort,
		SrcPort: msg.LocalPort,
		Process: procMeta,
	}
	decision := p.handler.HandleConnection(intercepted)

	// Publish the worker-owned fields, then store Ready LAST so any
	// reader (flow_closed / LookupFlowDestination) that observes Ready
	// sees a fully-populated state (see flow.State doc). DstHost is NOT
	// touched here — it belongs to the reader goroutine (registration +
	// flow_update_host).
	fs.ProcPID = procMeta.PID
	fs.ProcPath = procMeta.Path
	fs.ProcName = procMeta.Name
	fs.ProcBundleID = procMeta.BundleID
	fs.ProcUser = procMeta.User
	fs.DecisionInt = int(decision)
	fs.Ready.Store(true)

	decStr := decisionString(decision)
	slog.Debug("NE flow_new",
		"flow_id", msg.FlowID,
		"remote_host", msg.RemoteHost,
		"remote_ip", msg.RemoteIP,
		"remote_port", msg.RemotePort,
		"local_port", msg.LocalPort,
		"pid", msg.PID,
		"proc_name", procMeta.Name,
		"proc_bundle", procMeta.BundleID,
		"decision", decStr,
		"active_flows", p.activeFlowsLen.Load(),
	)

	resp := nepkg.DecisionMsg{FlowID: msg.FlowID, Decision: decStr}
	data, _ := json.Marshal(resp)
	data = append(data, '\n')
	nc.reply(data)
}

// handleFlowUpdateHost rewrites the destination hostname of an
// in-flight flow when the Swift extension has just extracted a
// real hostname from the TLS ClientHello SNI. This recovers the
// hostname for browsers / Electron apps / DoH clients that
// pre-resolve DNS and leave NEAppProxyFlow.remoteHostname nil.
// No-op when the flow is unknown (race with flow_closed) or the
// hostname is empty.
func (p *DarwinPlatform) handleFlowUpdateHost(msg nepkg.FlowMsg) {
	if msg.Hostname == "" {
		return
	}
	// Runs on the IPC reader goroutine; DstHost is reader-owned (see
	// flow.State doc), so writing it here needs no lock against the
	// decision worker, which never touches DstHost.
	fs, ok := p.loadFlow(msg.FlowID)
	if ok {
		// Atomic store: a per-connection bridge goroutine may be reading
		// DstHost via LookupFlowDestination at the same time.
		fs.SetDstHost(msg.Hostname)
		slog.Debug("NE flow_update_host applied",
			"flow_id", msg.FlowID, "hostname", msg.Hostname,
		)
	}
}

func (p *DarwinPlatform) handleFlowClosed(msg nepkg.FlowMsg) {
	fs, ok := p.loadAndDeleteFlow(msg.FlowID)
	remaining := p.activeFlowsLen.Load()

	if !ok {
		slog.Warn("NE flow_closed for unknown flow — possible double-close or out-of-order frame",
			"flow_id", msg.FlowID,
			"bytes_in", msg.BytesIn,
			"bytes_out", msg.BytesOut,
		)
		return
	}

	// Usually the worker has stored Ready=true before the NE relayed and
	// closed the flow, so the decision + proc fields are published. But
	// it is genuinely reachable for them NOT to be: if the daemon was
	// slow and the NE's 2 s requestDecision timeout fired, the extension
	// synthesizes passthrough WITHOUT a reply, relays, and can emit
	// flow_closed while the worker is still computing. In that case there
	// is no published decision to audit, so skip the per-flow row rather
	// than read unpublished fields (a small audit-trail gap on a
	// timed-out flow, not a correctness break).
	if !fs.Ready.Load() {
		slog.Debug("NE flow_closed before decision ready — skipping flow row",
			"flow_id", msg.FlowID,
		)
		return
	}

	slog.Debug("NE flow_closed",
		"flow_id", msg.FlowID,
		"remote_host", fs.DstHost(),
		"decision", fs.DecisionInt,
		"bytes_in", msg.BytesIn,
		"bytes_out", msg.BytesOut,
		"duration_ms", msg.DurationMs,
		"active_flows_remaining", remaining,
	)

	// One greppable verdict per flow at INFO. This is the single line that
	// answers "what happened to <app>'s traffic to <host>?" — the bundle
	// (now NE-attested, not the racy PID guess), the daemon's decision, the
	// bump outcome, and whether the HTTP body was actually captured. An
	// inspect flow only yields per-request audit rows when the bump
	// succeeded; a pinned host (bump rejected → opaqueRelay fallback)
	// reports decision=inspect but captured=false, which is exactly the
	// "I configured inspection but see no body" case operators hit. Cheap
	// (one log line per flow close) and INFO so it lands in agent.log at the
	// default level — no debug toggle needed mid-incident.
	decStr := decisionString(api.Decision(fs.DecisionInt))
	captured := api.Decision(fs.DecisionInt) == api.DecisionInspect && msg.BumpStatus == "BUMP_SUCCESS"
	slog.Info("flow verdict",
		"flow_id", msg.FlowID,
		"bundle", fs.ProcBundleID,
		"proc", fs.ProcName,
		"host", fs.DstHost(),
		"decision", decStr,
		"bump_status", msg.BumpStatus,
		"captured", captured,
		"bytes_in", msg.BytesIn,
		"bytes_out", msg.BytesOut,
	)

	// Inspect flows have already been recorded as N per-HTTP-request
	// rows by tlsbump.AuditEmitter inside BumpFlow. DO NOT write an
	// additional flow-level row here. The transport-level metrics
	// (bytes/duration) from Swift's flow_closed message are not
	// currently merged into the per-request rows.
	if api.Decision(fs.DecisionInt) == api.DecisionInspect {
		return
	}

	// Passthrough/deny flows DID NOT run through the bridge → no
	// per-request rows exist. Write one transport-level row here so
	// the agent UI can show "this flow was decided X without
	// inspection". Fields are limited to what Swift's flow_closed
	// surfaces (no method/path/hookDecision — Swift can't see
	// decrypted HTTP for non-bumped flows).
	if auditor, ok := p.handler.(api.FlowAuditor); ok {
		var breakdown map[string]int
		if msg.InterceptMs != nil && *msg.InterceptMs > 0 {
			breakdown = map[string]int{"intercept_ms": *msg.InterceptMs}
		}
		pm := api.ProcessMeta{
			PID:      fs.ProcPID,
			Path:     fs.ProcPath,
			Name:     fs.ProcName,
			BundleID: fs.ProcBundleID,
			User:     fs.ProcUser,
		}
		auditor.OnFlowComplete(api.FlowResult{
			FlowID:           msg.FlowID,
			SrcIP:            fs.SrcIP,
			DstHost:          fs.DstHost(),
			DstIP:            fs.DstIP,
			DstPort:          fs.DstPort,
			Process:          pm,
			Decision:         api.Decision(fs.DecisionInt),
			BytesIn:          msg.BytesIn,
			BytesOut:         msg.BytesOut,
			DurationMs:       msg.DurationMs,
			BumpStatus:       msg.BumpStatus,
			StartedAt:        fs.StartedAt,
			UpstreamTtfbMs:   msg.UpstreamTtfbMs,
			UpstreamTotalMs:  msg.UpstreamTotalMs,
			LatencyBreakdown: breakdown,
		})
	}
}
