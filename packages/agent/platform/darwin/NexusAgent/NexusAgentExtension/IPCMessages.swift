// IPCMessages.swift — wire types for the NE ↔ Go agent IPC protocol.
//
// Newline-delimited JSON frames sent over the Unix domain socket. Extracted
// from IPCProtocol.swift (which owns the AgentIPCClient transport) so each
// file stays focused — these are pure Codable value types with no transport
// dependency.

import Foundation

/// Message sent from the NE to the Go agent when a new flow is intercepted.
struct NEFlowNewMessage: Encodable {
    let flowId: String
    let remoteHost: String
    let remoteIp: String
    let remotePort: Int
    let localPort: Int
    let pid: Int32
    let transportProtocol: String
    // Kernel-attested signing identifier of the flow's source app
    // (NEFlowMetaData.sourceAppSigningIdentifier), e.g. "codex",
    // "com.github.Electron.helper". The daemon prefers this over its
    // own PID→process lookup, which is racy (PID reuse) and empty for
    // sandboxed / CLI helper processes. Empty when macOS reports no
    // signing identifier (unsigned binaries).
    let bundleId: String

    enum CodingKeys: String, CodingKey {
        case type_ = "type"
        case flowId, remoteHost, remoteIp, remotePort, localPort, pid, bundleId
        case transportProtocol = "protocol"
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode("flow_new", forKey: .type_)
        try container.encode(flowId, forKey: .flowId)
        try container.encode(remoteHost, forKey: .remoteHost)
        try container.encode(remoteIp, forKey: .remoteIp)
        try container.encode(remotePort, forKey: .remotePort)
        try container.encode(localPort, forKey: .localPort)
        try container.encode(pid, forKey: .pid)
        try container.encode(bundleId, forKey: .bundleId)
        try container.encode(transportProtocol, forKey: .transportProtocol)
    }
}

/// Message sent from the NE when a flow closes.
///
/// E50 latency phase fields (`upstreamTtfbMs`, `upstreamTotalMs`,
/// `interceptMs`) are optional; the Swift NE proxy populates them when
/// the upstream call surfaces `URLSessionTaskMetrics` data (most flows
/// do — Apple's NEAppProxyTCPFlow + URLSession path emits the metric
/// pairs around request send / first response byte / last byte). When
/// absent the field is omitted from the JSON envelope and the Go side
/// leaves the corresponding `traffic_event` column NULL. See
/// `docs/developers/architecture/services/agent/agent-macos-platform-architecture.md` for the cross-language contract.
struct NEFlowClosedMessage: Encodable {
    let flowId: String
    let bytesIn: Int64
    let bytesOut: Int64
    let durationMs: Int
    let bumpStatus: String

    // E50 phase fields — nil means "not measured this flow". URLSession
    // metrics are best-effort: passthrough flows that go directly via
    // raw TCP relay (no URLSessionTask wrapping) report nil for all 3.
    let upstreamTtfbMs: Int?
    let upstreamTotalMs: Int?
    let interceptMs: Int?

    enum CodingKeys: String, CodingKey {
        case type_ = "type"
        case flowId, bytesIn, bytesOut, durationMs, bumpStatus
        case upstreamTtfbMs, upstreamTotalMs, interceptMs
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode("flow_closed", forKey: .type_)
        try container.encode(flowId, forKey: .flowId)
        try container.encode(bytesIn, forKey: .bytesIn)
        try container.encode(bytesOut, forKey: .bytesOut)
        try container.encode(durationMs, forKey: .durationMs)
        try container.encode(bumpStatus, forKey: .bumpStatus)
        // Omit nil phase fields — Go side treats absent + null identically.
        if let v = upstreamTtfbMs { try container.encode(v, forKey: .upstreamTtfbMs) }
        if let v = upstreamTotalMs { try container.encode(v, forKey: .upstreamTotalMs) }
        if let v = interceptMs { try container.encode(v, forKey: .interceptMs) }
    }
}

/// Decision response from the Go agent.
struct NEDecisionResponse: Codable {
    let flowId: String
    let decision: String // "inspect" | "passthrough" | "deny"
}

/// One-shot upgrade message sent after we extract the SNI hostname
/// from the flow's first TLS ClientHello frame. The daemon updates
/// the in-flight flowState's destination host so the audit row
/// (written on flow_closed) carries the real hostname instead of
/// the IP literal that NEAppProxyFlow.remoteHostname surfaced
/// (typically nil for browsers / Electron / anything that pre-
/// resolves DNS itself).
struct NEFlowHostUpdateMessage: Encodable {
    let flowId: String
    let hostname: String

    enum CodingKeys: String, CodingKey {
        case type_ = "type"
        case flowId, hostname
    }

    func encode(to encoder: Encoder) throws {
        var container = encoder.container(keyedBy: CodingKeys.self)
        try container.encode("flow_update_host", forKey: .type_)
        try container.encode(flowId, forKey: .flowId)
        try container.encode(hostname, forKey: .hostname)
    }
}
