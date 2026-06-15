// TransparentProxyProvider.swift — NETransparentProxyProvider implementation
// for macOS traffic interception (E17 Phase 3A).
//
// Intercepts TCP flows from managed applications, queries the Go agent for
// interception decisions via Unix socket IPC, and routes flows accordingly.

import NetworkExtension
import os.log

class NexusProxyProvider: NETransparentProxyProvider {

    private let logger = Logger(subsystem: "com.nexus-gateway.agent", category: "ProxyProvider")
    private let ipcClient = AgentIPCClient()
    private let quicBundles = QUICFallbackBundles()
    private let bypassBundles = BypassBundles()
    private let daemonPIDFilter = DaemonPIDFilter()

    // The macOS system-network-service protection floor (the set of daemons
    // whose UDP must NEVER be claimed by our proxy, regardless of destination
    // port) now lives in SystemBundles.generated.swift — generated from the Go
    // SSOT packages/shared/policy/systembundles so the two copies can never
    // drift. handleNewFlow fast-declines via SystemBundles.covers(_:), which
    // reproduces the Go equal/ancestor/descendant matching exactly. Belt-and-
    // suspenders with the rule-level excludedNetworkRules in startProxy() — the
    // rule layer catches well-known UDP ports, the bundle floor catches the
    // long tail (mDNSResponder DNS-over-HTTPS on UDP 443, apsd's varying Apple
    // Push ports, configd's misc discovery probes).

    /// Serial dispatch queue used for time-bounded callbacks
    /// (peekSNIThenDecide SNI-peek timeout, future watchdogs). Separate
    /// from AgentIPCClient's queue so an IPC hang can't starve our timeouts.
    private let queue = DispatchQueue(label: "com.nexus-gateway.agent.proxy", qos: .userInitiated)

    /// Active flow tracking for byte counting and duration.
    private var activeFlows: [String: FlowState] = [:]
    private let flowLock = NSLock()

    struct FlowState {
        let flow: NEAppProxyFlow
        let startTime: Date
        var bytesIn: Int64 = 0
        var bytesOut: Int64 = 0
        var bumpStatus: String = ""
        // E50 phase fields populated by URLSessionTaskMetricsHandler when
        // an upstream URLSession task completes. Optional because not
        // every flow goes through URLSession (raw TCP relay does not).
        var upstreamTtfbMs: Int? = nil
        var upstreamTotalMs: Int? = nil
        // interceptMs = elapsed from kernel NEAppProxyFlow.open() to the
        // start of our handler. Captured at flow registration time.
        var interceptMs: Int? = nil
    }

    // MARK: - Lifecycle

    override func startProxy(options: [String: Any]?, completionHandler: @escaping (Error?) -> Void) {
        let t0 = Date()
        let optionsDesc = options.map { String(describing: $0) } ?? "<nil>"
        logger.info("startProxy: ENTERED — pid=\(ProcessInfo.processInfo.processIdentifier) bundleId=\(Bundle.main.bundleIdentifier ?? "<nil>") options=\(optionsDesc)")

        // Connect to the Go agent's IPC socket
        logger.info("startProxy: connecting to daemon IPC socket via AgentIPCClient")
        guard ipcClient.connect() else {
            logger.error("startProxy: AgentIPCClient.connect() returned false — daemon socket /var/run/nexus-agent/ne.sock unreachable. Check: (1) is the LaunchDaemon running? `launchctl print system/com.nexus-gateway.agent`. (2) does the socket exist? `sudo lsof /var/run/nexus-agent/ne.sock`. (3) sandbox/entitlement: extension's app-sandbox is false and network-server is true.")
            completionHandler(NSError(domain: "NexusAgent", code: 1,
                userInfo: [NSLocalizedDescriptionKey: "Cannot connect to agent IPC socket"]))
            return
        }
        logger.info("startProxy: AgentIPCClient.connect() ok in \(Int(Date().timeIntervalSince(t0) * 1000))ms")

        // Configure network settings: intercept ALL outbound flows
        // (TCP + UDP). UDP intercept exists for one purpose: kill QUIC
        // connections (h3 over UDP 443) early so Chrome / Edge fall
        // back to HTTP/2 over TCP, which we can SNI-parse + run through
        // hook pipeline. This mirrors mitmproxy's `.any` rule (their
        // macos-redirector at network-extension/TransparentProxyProvider.swift)
        // — they intercept UDP precisely so their TCP path actually
        // sees real browser AI traffic. Without this, Chrome → ChatGPT
        // is invisible because Cloudflare-fronted services prefer h3.
        let settings = NETransparentProxyNetworkSettings(tunnelRemoteAddress: "127.0.0.1")

        let anyRule = NENetworkRule(
            remoteNetwork: nil,
            remotePrefix: 0,
            localNetwork: nil,
            localPrefix: 0,
            protocol: .any,
            direction: .outbound
        )
        settings.includedNetworkRules = [anyRule]

        // Layer 1 architecture: catch-all .any (above) captures all TCP +
        // all UDP. Layer 2 (below): excludedNetworkRules tells macOS NECP
        // to NEVER route the following critical-system UDP ports through
        // our proxy, even when the catch-all includedRule would otherwise
        // match. Without this exclusion list, mDNSResponder /
        // configd / dhcpcd / apsd / ntpd outbound UDP packets all enter
        // handleNewFlow; our code does `return false` for non-browser
        // bundles, but the CLAUDE.md binding warns this return-false
        // behaviour under `.any` includedRule is NOT a guaranteed
        // fall-back-to-native — macOS may drop the flow instead of
        // routing natively, breaking DNS / DHCP / mDNS / NTP / Apple
        // Push. Live RCA 2026-05-24: user reported "page froze" + curl
        // getaddrinfo timeout because mDNSResponder UDP/53 queries
        // were being silently dropped. excludedNetworkRules is the only
        // OS-level mechanism to guarantee these critical packets never
        // reach our process. Browser h3 (UDP/443) is NOT excluded — it
        // still enters handleNewFlow and gets QUIC-killed there so
        // Chrome / Edge fall back to HTTP/2 over TCP / 443 (captured by
        // the TCP catch-all path). See memory:
        // feedback_macos_mdns_flush_after_ne_state_change.
        let criticalUDPPorts = [
            "53",    // DNS
            "5353",  // mDNS
            "67",    // DHCP server (boot)
            "68",    // DHCP client (boot)
            "123",   // NTP
            "500",   // IKE
            "4500",  // IKE NAT-T
            "1900",  // SSDP / UPnP discovery
            "5355",  // LLMNR (Windows compat, harmless on macOS)
        ]
        var excludedRules: [NENetworkRule] = []
        for port in criticalUDPPorts {
            // Two rules per port covering IPv4 + IPv6 catch-all hosts.
            // Prefix=0 + hostname="0.0.0.0"/"::"  = match any host on
            // that protocol+port. macOS NENetworkRule's port matching
            // for UDP is documented in NetworkExtension headers; it
            // should respect the port field on the remoteNetwork
            // endpoint. If empirical testing shows NECP ignores the
            // port for UDP rules, the handleNewFlow process-bundle
            // check below acts as a Layer-2 belt-and-suspenders.
            excludedRules.append(NENetworkRule(
                remoteNetwork: NWHostEndpoint(hostname: "0.0.0.0", port: port),
                remotePrefix: 0,
                localNetwork: nil,
                localPrefix: 0,
                protocol: .UDP,
                direction: .outbound
            ))
            excludedRules.append(NENetworkRule(
                remoteNetwork: NWHostEndpoint(hostname: "::", port: port),
                remotePrefix: 0,
                localNetwork: nil,
                localPrefix: 0,
                protocol: .UDP,
                direction: .outbound
            ))
        }
        settings.excludedNetworkRules = excludedRules
        logger.info("startProxy: built NETransparentProxyNetworkSettings (tunnelRemoteAddress=127.0.0.1, includedRules=1 .any catch-all, excludedRules=\(excludedRules.count) critical-UDP-port rules: \(criticalUDPPorts.joined(separator: ","))); calling setTunnelNetworkSettings")

        setTunnelNetworkSettings(settings) { [weak self] error in
            let totalMs = Int(Date().timeIntervalSince(t0) * 1000)
            if let error = error {
                let nsErr = error as NSError
                self?.logger.error("startProxy: setTunnelNetworkSettings FAILED in \(totalMs)ms: domain=\(nsErr.domain) code=\(nsErr.code) localized=\(error.localizedDescription)")
                completionHandler(error)
                return
            }
            self?.logger.info("startProxy: setTunnelNetworkSettings ok in \(totalMs)ms — proxy is now ACTIVE; handleNewFlow will fire for every outbound TCP flow from monitored apps")
            completionHandler(nil)
        }
    }

    override func stopProxy(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        // Map reason rawValue to a name. Apple's enum:
        // .none=0, .userInitiated=1, .providerFailed=2, .noNetworkAvailable=3,
        // .unrecoverableNetworkChange=4, .providerDisabled=5, .authenticationCanceled=6,
        // .configurationFailed=7, .idleTimeout=8, .configurationDisabled=9,
        // .configurationRemoved=10, .superseded=11, .userLogout=12, .userSwitch=13,
        // .connectionFailed=14, .sleep=15, .appUpdate=16.
        // .internalError=17 is macOS 14.2+ only; Package.swift pins .v13
        // deployment target, so raw 17 lands in `@unknown default`.
        let reasonName: String
        switch reason {
        case .none: reasonName = "none(0)"
        case .userInitiated: reasonName = "userInitiated(1)"
        case .providerFailed: reasonName = "providerFailed(2)"
        case .noNetworkAvailable: reasonName = "noNetworkAvailable(3)"
        case .unrecoverableNetworkChange: reasonName = "unrecoverableNetworkChange(4)"
        case .providerDisabled: reasonName = "providerDisabled(5)"
        case .authenticationCanceled: reasonName = "authenticationCanceled(6)"
        case .configurationFailed: reasonName = "configurationFailed(7)"
        case .idleTimeout: reasonName = "idleTimeout(8)"
        case .configurationDisabled: reasonName = "configurationDisabled(9)"
        case .configurationRemoved: reasonName = "configurationRemoved(10)"
        case .superceded: reasonName = "superceded(11)" // Apple's typo, preserved in framework
        case .userLogout: reasonName = "userLogout(12)"
        case .userSwitch: reasonName = "userSwitch(13)"
        case .connectionFailed: reasonName = "connectionFailed(14)"
        case .sleep: reasonName = "sleep(15)"
        case .appUpdate: reasonName = "appUpdate(16)"
        // `default` (not `@unknown default`) so the switch tolerates
        // SDK-visible cases that are availability-gated above the
        // deployment target — e.g. `.internalError` (raw 17) is macOS
        // 14.2+ only and Package.swift pins .v13.
        default: reasonName = "unknown(\(reason.rawValue))"
        }
        flowLock.lock()
        let activeCount = activeFlows.count
        let flows = activeFlows
        activeFlows.removeAll()
        flowLock.unlock()

        logger.info("stopProxy: ENTERED — reason=\(reasonName) activeFlows=\(activeCount) (will report flow_closed for each, then disconnect IPC)")

        for (flowId, state) in flows {
            reportFlowClosed(flowId: flowId, state: state)
        }

        // shutdown() (not disconnect()) suppresses auto-reconnect — stopProxy is a deliberate teardown.
        ipcClient.shutdown()
        logger.info("stopProxy: complete (ipcClient shut down)")
        completionHandler()
    }

    // MARK: - Flow Handling

    override func handleNewFlow(_ flow: NEAppProxyFlow) -> Bool {
        // SAFETY: this method MUST return true/false synchronously and
        // MUST NOT throw — any uncaught Swift error here causes macOS
        // to drop the flow without routing natively, which appears as
        // a network outage to the user. Swift cannot catch runtime
        // traps, so the safety contract is structural, not a wrapper:
        // every lookup below is a `guard`/`as?` that declines the flow
        // on failure, and the body contains NO force-unwraps. Keep it
        // that way — a single `!` added here can crash the provider on
        // a malformed flow and take the host's networking with it.
        let bundleId = flow.metaData.sourceAppSigningIdentifier

        // NE self-heal: daemon absent past grace → decline all (native routing), AgentIPCClient auto-reconnects. See agent-ne-fail-open-architecture.md.
        if ipcClient.daemonAbsentBeyondGrace(6.0) {
            logger.info("handleNewFlow: daemon IPC absent beyond grace — declining flow for native routing (self-heal, no zombie interception) bundleId=\(bundleId, privacy: .public)")
            return false
        }

        // E55 self-intercept guard: NE intercepts ALL outbound traffic
        // including the agent daemon's own connections.
        //
        // A claim + direct-relay approach for daemon self-traffic was
        // tried and abandoned — NE's createTCPConnection in self-intercept context
        // returns "read: errno 403" during TLS handshake, breaking
        // both daemon's bootstrap to Hub AND tlsbump's upstream
        // forward (browser → chatgpt.com → bridge → daemon HTTP
        // client → claim+relay → upstream fail → 502 to browser).
        // Reverted to `return false` as the least-bad option.
        //
        // Known consequence: with includedNetworkRules.protocol=.any,
        // NECP routes daemon's outbound to the proxy; `return false`
        // drops the flow. Daemon's HTTP client will retry, and an
        // early-boot race window sometimes lets a request through
        // before NE fully claims the daemon PID (this is how the
        // existing WebSocket gets established + reconnects).
        //
        // Known limitation / future option (tracked: task #56): the
        // clean fix would use NETransparentProxyNetworkSettings.
        // excludedNetworkRules to tell NECP not to route daemon-bound
        // traffic to the proxy at the IP level. That needs the daemon
        // to DNS-resolve Hub + key upstreams at startup and push the IP
        // list to the extension via ne.sock. Until that lands, the
        // synchronous `return false` above remains the correct fail-open
        // behavior — it is not a placeholder and must not be flipped to
        // a claim+relay path (which returns "read: errno 403" here).
        let sourcePid = extractPID(from: flow)
        if daemonPIDFilter.isDaemon(pid: sourcePid) {
            logger.debug("handleNewFlow: skipping self-intercept from daemon PID \(sourcePid)")
            return false
        }

        // Self-exemption: source bundle on bypassBundles → decline (native routing, no bump). Empty = inspect all. See agent-ne-fail-open-architecture.md Rule 3.
        if bypassBundles.shouldBypass(bundleId: bundleId) {
            logger.info("handleNewFlow: bypass — source bundle \(bundleId, privacy: .public) on exemption list; declining for native routing (no bump)")
            return false
        }

        // UDP path: only kill UDP flows from known QUIC-capable clients
        // (Chrome / Edge / Safari / Firefox / Arc / Vivaldi / Brave +
        // Electron AI desktop apps). For anything else — DNS, DHCP,
        // mDNS, NTP, WireGuard, FaceTime, games, custom UDP services —
        // we DECLINE the flow so macOS routes it natively and our
        // proxy never touches it. Without this guard, a process hang
        // anywhere in our code path (IPC stuck, peek timeout, panic)
        // takes down the entire host's UDP stack including DNS, which
        // looks like a complete network outage to the user.
        // See incident 2026-05-15: shipping `.any` rule + blanket
        // close-all-UDP killed DNS/DHCP/mDNS and required manual
        // launchctl unload + plist deletion to recover.
        if let udpFlow = flow as? NEAppProxyUDPFlow {
            // Layer 2 defense: explicit fast-decline for known macOS
            // system network services. Even though startProxy() already
            // registered excludedNetworkRules for the standard
            // critical-UDP ports (53/5353/67/68/123/500/4500/1900/5355),
            // a system process can send UDP to non-standard ports too —
            // e.g. mDNSResponder for DNS-over-HTTPS (UDP/443 in some
            // configs), apsd for Apple Push (multiple varying ports
            // across macOS versions). Bundle-id check here ensures the
            // flow is never claimed regardless of port. Belt + suspenders
            // with the rule-level exclusion above.
            if SystemBundles.covers(bundleId) {
                logger.debug("handleNewFlow: decline UDP from system service \(bundleId, privacy: .public) — bundle in SystemBundles protection floor (generated from Go SSOT)")
                return false
            }
            if quicBundles.shouldForceFallback(bundleId: bundleId) {
                logger.info("handleNewFlow: kill UDP from \(bundleId, privacy: .public) → force QUIC→TCP fallback")
                udpFlow.closeReadWithError(NSError(domain: "NexusAgent", code: 1,
                    userInfo: [NSLocalizedDescriptionKey: "QUIC blocked; fall back to TCP"]))
                udpFlow.closeWriteWithError(nil)
                return true
            }
            // Not on the allowlist — bundleId may be empty (unsigned
            // binary), a system daemon, a game, or a custom UDP app.
            // Decline; macOS handles it natively. This is the
            // critical safety branch: never claim UDP we can't relay.
            //
            // Diagnostic (log only — does NOT change the decline decision, so
            // fail-open is unaffected): a UDP flow from a *signed, non-system*
            // app that is NOT on the QUIC-fallback list escapes here over UDP
            // (likely QUIC/h3) and is never seen on the TCP path. Logging the
            // bundle id reveals exactly which app/helper to add to the
            // QUIC-fallback list (e.g. an AI client whose chat we're missing).
            // Empty bundleId (unsigned) is skipped to avoid system noise.
            if !bundleId.isEmpty {
                logger.info("handleNewFlow: UDP flow declined — escapes uncaptured (likely QUIC). bundle=\(bundleId, privacy: .public). Add to QUIC-fallback list to capture this app.")
            }
            return false
        }

        guard let tcpFlow = flow as? NEAppProxyTCPFlow else {
            // Neither TCP nor UDP — unknown flow class. Decline so
            // macOS routes natively.
            logger.debug("handleNewFlow: rejected — neither TCP nor UDP; flow class=\(String(describing: type(of: flow)))")
            return false
        }

        guard let endpoint = tcpFlow.remoteEndpoint as? NWHostEndpoint else {
            logger.debug("handleNewFlow: rejected — remoteEndpoint not NWHostEndpoint; type=\(String(describing: type(of: tcpFlow.remoteEndpoint)))")
            return false
        }

        let flowId = UUID().uuidString
        // endpoint.hostname is the literal connect target — for any caller
        // that pre-resolves DNS (curl, browsers, native getaddrinfo) it is
        // an IP STRING, not the original hostname. Apple exposes the
        // pre-resolved hostname via NEAppProxyFlow.remoteHostname. Prefer
        // that so our audit row stores `chatgpt.com` instead of
        // `203.0.113.10`. Fall back to the endpoint when remoteHostname
        // is nil (rare; happens when the caller skipped name resolution).
        let endpointAddr = endpoint.hostname
        let resolvedHost = (tcpFlow.remoteHostname?.isEmpty == false ? tcpFlow.remoteHostname : nil) ?? endpointAddr
        let host = resolvedHost
        let port = Int(endpoint.port) ?? 443

        // Resolve the source process PID from the flow's audit token
        let hasAuditToken = flow.metaData.sourceAppAuditToken != nil
        let pid = extractPID(from: flow)

        // IP captured separately. When remoteHostname differs from
        // endpointAddr (the common case), endpointAddr already holds the
        // dotted-quad / colon-hex IP string. When BOTH fields are
        // hostnames (rare: the caller skipped name resolution), the IP
        // stays empty: it feeds only a best-effort audit column, and
        // resolving DNS here would put a blocking call on the
        // handleNewFlow decision path — while it blocks, no other
        // intercepted flow on the host gets a decision. Never resolve
        // DNS synchronously in this method.
        let ip = isLikelyIPLiteral(endpointAddr) ? endpointAddr : ""

        let localPort = extractLocalPort(from: flow)

        // INFO so every flow_new lands in default-level agent.log. The
        // bundleId / host / port triple is the fastest answer to "did
        // Chrome's traffic to chatgpt.com even reach the NE?". Without
        // this line at INFO, browsers' passthrough flows are completely
        // silent and the diagnostic dies at "I see nothing in agent.log".
        logger.info("handleNewFlow: ENTER flow=\(flowId, privacy: .public) bundleId=\(bundleId, privacy: .public) host=\(host, privacy: .public) ip=\(ip, privacy: .public) port=\(port) pid=\(pid) hasAuditToken=\(hasAuditToken) — opening flow + peeking SNI before requesting decision")

        // Track the flow
        flowLock.lock()
        activeFlows[flowId] = FlowState(flow: tcpFlow, startTime: Date())
        flowLock.unlock()

        // E55 / #79: peek the TLS ClientHello BEFORE asking the daemon
        // for a decision. Pre-E55 we sent `host` (which is the IP
        // literal for callers like Cursor/curl/Claude Desktop that
        // pre-resolve DNS) directly — daemon Engine.Evaluate would
        // never match an interception_domain pattern → passthrough →
        // user sees their inspect rules silently bypassed. Now we
        // peek for SNI first; the daemon receives the real hostname
        // and Engine sees the right target. The 500ms timeout means
        // server-speaks-first protocols (SSH, SMTP, plain HTTP after
        // redirect) fall through with the original (IP / remote-
        // hostname) host, which is the correct behaviour for those.
        peekSNIThenDecide(
            flowId: flowId,
            flow: tcpFlow,
            initialHost: host,
            ip: ip,
            port: port,
            localPort: localPort,
            pid: pid,
            bundleId: bundleId
        )

        return true // we are handling this flow
    }

    /// E55 / #79: peeks the TLS ClientHello of an inbound flow,
    /// extracts the SNI hostname, and only THEN asks the daemon for
    /// the inspect/passthrough/deny decision. Without this two-step
    /// pre-decision peek, callers that pre-resolve DNS (Cursor /
    /// Claude Desktop / curl / browsers) hand the daemon an IP
    /// literal — Engine.Evaluate matches no domain rule → passthrough
    /// → user's inspect rules silently bypassed.
    ///
    /// Lifecycle: open the NE flow → readData first chunk → on TLS
    /// ClientHello extract SNI → requestDecision with the resolved
    /// host → applyDecisionAfterPeek drives the bridge / passthrough /
    /// deny path; the peeked bytes are forwarded onwards by the chosen
    /// path so we don't lose any handshake data.
    ///
    /// Fail-safe: 500ms timeout (server-speaks-first protocols never
    /// emit a ClientHello, e.g. SSH/SMTP/IMAP) — on timeout we just
    /// requestDecision with the original host (IP or remoteHostname).
    /// Same outcome as the pre-E55 path for non-TLS flows.
    private func peekSNIThenDecide(flowId: String,
                                   flow: NEAppProxyTCPFlow,
                                   initialHost: String,
                                   ip: String,
                                   port: Int,
                                   localPort: Int,
                                   pid: Int32,
                                   bundleId: String) {
        // Watchdog on flow.open itself: the 500ms peek timer below is
        // armed INSIDE the open callback, so an open that never calls
        // back would leave this claimed flow hanging with no timeout at
        // all. A claimed-but-unusable flow gets the standard fail-open
        // reset; a late open completion after the watchdog fired is
        // dropped (the guard is one-shot).
        let openGuard = TimeoutGuard()
        queue.asyncAfter(deadline: .now() + relayEstablishTimeout) { [weak self] in
            guard let self, openGuard.tryFire() else { return }
            self.failOpenResetFlow(flowId: flowId, flow: flow,
                                   reason: "flow.open never completed for \(initialHost):\(port)")
        }
        flow.open(withLocalEndpoint: nil) { [weak self] err in
            guard let self else { return }
            guard openGuard.tryFire() else {
                // Watchdog already reset this flow; nothing to relay.
                return
            }
            if let err = err {
                let nsErr = err as NSError
                self.logger.error("peekSNIThenDecide: flow.open FAILED for flow=\(flowId, privacy: .public) host=\(initialHost, privacy: .public):\(port) — domain=\(nsErr.domain) code=\(nsErr.code)")
                // handleNewFlow already returned true; the OS has claimed this
                // flow on our behalf. Without an explicit close the browser
                // waits ~75 s for its SYN to time out — visible as a frozen
                // tab. Close the flow so the browser sees an immediate reset
                // and retries (where macOS routes it natively or the new
                // handleNewFlow call takes the same flow through a healthy
                // path). Reset is the fail-open shape for a claimed-then-
                // unusable flow; the only worse option is leaving it hung.
                flow.closeReadWithError(nsErr)
                flow.closeWriteWithError(nil)
                self.completeFlow(flowId: flowId)
                return
            }
            // Bound the SNI peek by 500ms so non-TLS protocols don't
            // block decision indefinitely. When the timer wins, the
            // readData below is STILL OUTSTANDING — Apple permits one
            // read in flight per flow, and whatever that read finally
            // delivers is the app's real first bytes (a server-speaks-
            // first protocol's first post-greeting write). lateRead
            // hands that late completion to the relay instead of
            // discarding it, and keeps the relay from racing a second
            // readData against the outstanding one.
            let timeoutFired = TimeoutGuard()
            let lateRead = LatePeekRead()
            self.queue.asyncAfter(deadline: .now() + .milliseconds(500)) { [weak self] in
                guard let self else { return }
                if timeoutFired.tryFire() {
                    self.logger.debug("peekSNIThenDecide: 500ms timeout for flow=\(flowId, privacy: .public) — falling back to initial host \(initialHost, privacy: .public)")
                    self.dispatchDecision(flowId: flowId, flow: flow, host: initialHost, peeked: nil, lateRead: lateRead, ip: ip, port: port, localPort: localPort, pid: pid, bundleId: bundleId)
                }
            }
            flow.readData { [weak self] data, readErr in
                guard let self else { return }
                guard timeoutFired.tryFire() else {
                    // Timeout already dispatched the decision with no
                    // peek bytes. This completion carries the app's
                    // real first payload (or its close/error) — hand it
                    // to the relay adopter; the chosen relay path
                    // forwards it as the first app→remote write.
                    lateRead.deliver(data, readErr)
                    return
                }
                if let readErr = readErr {
                    self.logger.debug("peekSNIThenDecide: first read error for flow=\(flowId, privacy: .public): \(readErr.localizedDescription) — dispatching with initial host")
                    self.dispatchDecision(flowId: flowId, flow: flow, host: initialHost, peeked: nil, lateRead: nil, ip: ip, port: port, localPort: localPort, pid: pid, bundleId: bundleId)
                    return
                }
                guard let data = data, !data.isEmpty else {
                    self.logger.debug("peekSNIThenDecide: empty first chunk for flow=\(flowId, privacy: .public); app closed before sending — completing")
                    self.completeFlow(flowId: flowId)
                    return
                }
                // SNI is best-effort. Use it when extracted; otherwise
                // keep initialHost.
                var resolvedHost = initialHost
                if let sni = SNIParser.extractSNI(from: data) {
                    self.logger.info("peekSNIThenDecide: SNI peeked sni=\(sni, privacy: .public) initial_host=\(initialHost, privacy: .public) for flow=\(flowId, privacy: .public) — using SNI as decision host")
                    resolvedHost = sni
                    // Also notify the daemon so the audit row's
                    // dst_host column carries the SNI immediately
                    // (decision and audit are independent paths).
                    self.ipcClient.notifyFlowHost(flowId: flowId, hostname: sni)
                }
                self.dispatchDecision(flowId: flowId, flow: flow, host: resolvedHost, peeked: data, lateRead: nil, ip: ip, port: port, localPort: localPort, pid: pid, bundleId: bundleId)
            }
        }
    }

    /// dispatchDecision fires the daemon decision request and routes
    /// the resulting decision into applyDecisionAfterPeek. Called by
    /// peekSNIThenDecide after either SNI extraction or 500ms
    /// timeout. The optional peeked bytes are the TLS ClientHello we
    /// already pulled off the flow — applyDecisionAfterPeek passes them
    /// to the bridge / passthrough handler so they get forwarded to the
    /// remote without being lost.
    private func dispatchDecision(flowId: String,
                                  flow: NEAppProxyTCPFlow,
                                  host: String,
                                  peeked: Data?,
                                  lateRead: LatePeekRead?,
                                  ip: String,
                                  port: Int,
                                  localPort: Int,
                                  pid: Int32,
                                  bundleId: String) {
        let t0 = Date()
        ipcClient.requestDecision(flowId: flowId, host: host, ip: ip, port: port, localPort: localPort, pid: pid, bundleId: bundleId) { [weak self] response in
            let latencyMs = Int(Date().timeIntervalSince(t0) * 1000)
            self?.logger.info("dispatchDecision: response decision=\(response.decision, privacy: .public) latency_ms=\(latencyMs) flow=\(flowId, privacy: .public) host=\(host, privacy: .public):\(port) — handing off to applyDecisionAfterPeek")
            self?.applyDecisionAfterPeek(flowId: flowId, flow: flow, host: host, port: port, peeked: peeked, lateRead: lateRead, decision: response.decision)
        }
    }

    /// applyDecisionAfterPeek routes a daemon decision onto the deny /
    /// inspect / passthrough relay path. By the time it runs: (a) the NE
    /// flow is already open (peekSNIThenDecide opened it), and (b) the
    /// first chunk has already been read off the flow and is passed in
    /// via peeked — the chosen relay path must forward it to the remote
    /// first before resuming the normal read loop, or the upstream sees
    /// a TCP connection that never sent a ClientHello.
    private func applyDecisionAfterPeek(flowId: String,
                                        flow: NEAppProxyTCPFlow,
                                        host: String,
                                        port: Int,
                                        peeked: Data?,
                                        lateRead: LatePeekRead?,
                                        decision: String) {
        switch decision {
        case "deny":
            // The adopted late read (if any) is deliberately never
            // consumed here: the flow is being closed, so its pending
            // completion fires against a closed flow and its bytes are
            // policy-blocked anyway.
            logger.info("applyDecisionAfterPeek: DENY flow=\(flowId, privacy: .public) → \(host, privacy: .public):\(port)")
            updateBumpStatus(flowId: flowId, status: "")
            flow.closeReadWithError(NSError(domain: "NexusAgent", code: 403, userInfo: [NSLocalizedDescriptionKey: "Blocked by policy"]))
            flow.closeWriteWithError(nil)
            completeFlow(flowId: flowId)

        case "inspect":
            // Inspect path → bridge. The peeked TLS ClientHello bytes
            // need to reach the bridge (so the Go bump pipeline's TLS
            // terminator sees them). Open bridge connection, write BRIDGE header,
            // immediately replay peeked bytes, then bidir relay.
            // INFO level so operators can audit "what's being intercepted"
            // without enabling debug logs.
            logger.info("applyDecisionAfterPeek: INSPECT flow=\(flowId, privacy: .public) → \(host, privacy: .public):\(port) → bridge (peeked=\(peeked?.count ?? 0)B)")
            updateBumpStatus(flowId: flowId, status: "BUMP_SUCCESS")
            relayInspectViaBridgePostPeek(flowId: flowId, flow: flow, host: host, port: port, peeked: peeked, lateRead: lateRead)

        default: // "passthrough"
            // INFO level so the "Chrome went to chatgpt.com but wasn't
            // captured" diagnostic is answerable from agent.log alone —
            // a passthrough decision means the daemon's policy engine
            // returned passthrough (host not on inspect list / pause /
            // exemption). Without this line at INFO, every Chrome flow
            // is invisible at default log level.
            logger.info("applyDecisionAfterPeek: PASSTHROUGH flow=\(flowId, privacy: .public) → \(host, privacy: .public):\(port) (peeked=\(peeked?.count ?? 0)B)")
            updateBumpStatus(flowId: flowId, status: "")
            relayPassthroughPostPeek(flowId: flowId, flow: flow, host: host, port: port, peeked: peeked, lateRead: lateRead)
        }
    }

    /// relayEstablishTimeout bounds the claim-to-first-relay window
    /// (SEC-M8-02). createTCPConnection to a black-holed upstream/bridge
    /// (SYN routed but never answered — no RST, no SYN-ACK) leaves the
    /// write completion (and the relay loops) waiting on the OS-default
    /// TCP connect timeout (~75s), freezing the one claimed flow for the
    /// user. The spec calls for a 2–5s watchdog; we use the upper end (5s)
    /// so a legitimately slow but working WAN handshake is not reset
    /// spuriously — and even a spurious reset is fail-open (the app gets
    /// ECONNRESET and macOS retries the flow natively).
    private let relayEstablishTimeout: DispatchTimeInterval = .seconds(5)

    /// failOpenResetFlow applies the established fail-open reset shape
    /// (the #82 / commit 61b597d93 contract) to a claimed-but-unusable
    /// flow: the app sees an immediate ECONNRESET and macOS routes the
    /// retry natively, rather than the flow hanging until the ~75s OS TCP
    /// timeout. Idempotent via completeFlow (a second call no-ops once the
    /// flow row is removed). code 60 = ETIMEDOUT (Darwin).
    private func failOpenResetFlow(flowId: String, flow: NEAppProxyTCPFlow, reason: String) {
        logger.info("failOpenResetFlow: flow=\(flowId, privacy: .public) reset — \(reason, privacy: .public)")
        let err = NSError(domain: "NexusAgent", code: 60,
                          userInfo: [NSLocalizedDescriptionKey: "upstream connect timeout (fail-open reset)"])
        flow.closeReadWithError(err)
        flow.closeWriteWithError(nil)
        completeFlow(flowId: flowId)
    }

    /// relayInspectViaBridgePostPeek connects the inspect path to the Go
    /// bump bridge. The NE flow is already open (peekSNIThenDecide opened
    /// it), so it writes the BRIDGE header, replays the peeked ClientHello
    /// bytes immediately after, then relays bidirectionally — so the
    /// Go-side bump pipeline's TLS terminator sees the full handshake.
    private func relayInspectViaBridgePostPeek(flowId: String, flow: NEAppProxyTCPFlow, host: String, port: Int, peeked: Data?, lateRead: LatePeekRead? = nil) {
        let isIPv6 = host.contains(":")
        let hostPart = isIPv6 ? "[\(host)]" : host
        let header = "BRIDGE \(hostPart):\(port) \(flowId)\n"
        guard let headerData = header.data(using: .utf8) else {
            logger.error("relayInspectViaBridgePostPeek: header encode failed for flow=\(flowId, privacy: .public)")
            relayPassthroughPostPeek(flowId: flowId, flow: flow, host: host, port: port, peeked: peeked, lateRead: lateRead)
            return
        }
        let bridgeEndpoint = NWHostEndpoint(hostname: "127.0.0.1", port: "9443")
        let bridgeConn = createTCPConnection(to: bridgeEndpoint, enableTLS: false, tlsParameters: nil, delegate: nil)

        // SEC-M8-02: bound bridge establishment. If the bridge SYN is
        // black-holed (Go listener hung), the write completion below never
        // fires; without this watchdog the flow freezes ~75s. On timeout
        // we cancel and fall back to direct passthrough — the same fail-
        // open degradation the writeErr branch already takes — and the
        // passthrough path arms its OWN watchdog, so the claim-to-relay
        // window stays bounded end-to-end. tryFire() races the write
        // completion: whoever fires first wins (TimeoutGuard is one-shot).
        let establishGuard = TimeoutGuard()
        queue.asyncAfter(deadline: .now() + relayEstablishTimeout) { [weak self] in
            guard let self else { return }
            guard establishGuard.tryFire() else { return }
            self.logger.info("relayInspectViaBridgePostPeek: bridge establish timeout for flow=\(flowId, privacy: .public) — cancel + fail-open to direct passthrough")
            bridgeConn.cancel()
            // lateRead is forwarded un-consumed: the adopted read is only
            // consumed AFTER a successful header write, which by this
            // guard's one-shot semantics can no longer happen.
            self.relayPassthroughPostPeek(flowId: flowId, flow: flow, host: host, port: port, peeked: peeked, lateRead: lateRead)
        }

        // Compose header + peeked bytes into a single write so the
        // bridge sees them as a contiguous stream (parseHeader
        // reads up to the newline, leaves the remainder buffered).
        var combined = headerData
        if let peeked = peeked { combined.append(peeked) }
        bridgeConn.write(combined) { [weak self] writeErr in
            guard let self else { return }
            // The establishment watchdog may already have fired (timed out
            // and fell back to passthrough); a late write completion must
            // not double-relay onto the same flow. tryFire() losing here
            // means the watchdog won — drop quietly.
            guard establishGuard.tryFire() else {
                self.logger.debug("relayInspectViaBridgePostPeek: late write completion after establish timeout for flow=\(flowId, privacy: .public) — dropping")
                return
            }
            if let writeErr = writeErr {
                self.logger.error("relayInspectViaBridgePostPeek: header+peek write FAILED for flow=\(flowId, privacy: .public): \(writeErr.localizedDescription) — falling back to direct")
                bridgeConn.cancel()
                self.relayPassthroughPostPeek(flowId: flowId, flow: flow, host: host, port: port, peeked: peeked, lateRead: lateRead)
                return
            }
            if let peeked = peeked {
                self.addBytesOut(flowId: flowId, count: Int64(peeked.count))
            }
            // Now run the standard bidirectional relay loops with the
            // bridge as remote. The app→bridge direction must adopt the
            // still-outstanding peek read when one exists (timed-out
            // peek) instead of issuing a competing readData; otherwise
            // it starts reading FRESH bytes (the first chunk was already
            // sent via combined above). The watchdog is already disarmed
            // (tryFire above), so no establishGuard is threaded onward.
            self.startAppToRemoteRelay(flowId: flowId, flow: flow, remote: bridgeConn, lateRead: lateRead)
            self.relayRemoteToFlow(flowId: flowId, flow: flow, remote: bridgeConn)
        }
    }

    /// relayPassthroughPostPeek is the post-peek counterpart of the
    /// passthrough path. Connects directly to remote, replays peeked
    /// bytes, then bidir relay.
    private func relayPassthroughPostPeek(flowId: String, flow: NEAppProxyTCPFlow, host: String, port: Int, peeked: Data?, lateRead: LatePeekRead? = nil) {
        let remoteHost = NWHostEndpoint(hostname: host, port: String(port))
        let remoteConn = createTCPConnection(to: remoteHost, enableTLS: false, tlsParameters: nil, delegate: nil)
        logger.info("relayPassthroughPostPeek: opened direct upstream flow=\(flowId, privacy: .public) → \(host, privacy: .public):\(port) peek_bytes=\(peeked?.count ?? 0)")

        // SEC-M8-02: bound direct-upstream establishment. A black-holed
        // upstream SYN otherwise hangs the flow ~75s (the peek write below,
        // or — when there is no peek to write — the first relay-loop write,
        // never completes). This is the terminal fail-open path (no further
        // fallback exists), so on timeout we cancel the dead connection and
        // reset the flow so the app gets an immediate ECONNRESET and macOS
        // retries natively. The guard is disarmed by the first evidence the
        // connection is usable: the peek-write completion when a peek
        // exists, otherwise the first data/error in EITHER relay direction
        // (threaded into both relay loops below) — so a server-speaks-first
        // protocol that legitimately sends nothing app-side is not falsely
        // reset. (A genuinely established server that stays silent for >5s
        // gets a fail-open reset+native-retry; acceptable and never worse
        // than the ~75s freeze this replaces.)
        let establishGuard = TimeoutGuard()
        queue.asyncAfter(deadline: .now() + relayEstablishTimeout) { [weak self] in
            guard let self else { return }
            guard establishGuard.tryFire() else { return }
            self.logger.info("relayPassthroughPostPeek: upstream establish timeout for flow=\(flowId, privacy: .public) → \(host, privacy: .public):\(port) — cancel + fail-open reset")
            remoteConn.cancel()
            self.failOpenResetFlow(flowId: flowId, flow: flow, reason: "upstream \(host):\(port) connect timeout")
        }

        if let peeked = peeked, !peeked.isEmpty {
            addBytesOut(flowId: flowId, count: Int64(peeked.count))
            remoteConn.write(peeked) { [weak self] err in
                guard let self else { return }
                // Disarm the establishment watchdog; if it already fired
                // (connection black-holed), drop this late completion.
                guard establishGuard.tryFire() else {
                    self.logger.debug("relayPassthroughPostPeek: late peek-write completion after establish timeout for flow=\(flowId, privacy: .public) — dropping")
                    return
                }
                if let err = err {
                    self.logger.debug("relayPassthroughPostPeek: peek write error for flow=\(flowId, privacy: .public): \(err.localizedDescription)")
                    return
                }
                self.relayFlowToRemote(flowId: flowId, flow: flow, remote: remoteConn)
            }
            relayRemoteToFlow(flowId: flowId, flow: flow, remote: remoteConn)
        } else {
            // No peek bytes (timed-out/non-TLS peek): there is no upstream
            // write to anchor establishment on, so thread the guard into
            // BOTH relay loops — the first app→remote write success OR the
            // first remote→app read completion disarms the watchdog,
            // covering both client- and server-speaks-first protocols.
            // The app→remote direction adopts the still-outstanding peek
            // read when one exists, instead of racing a second readData.
            startAppToRemoteRelay(flowId: flowId, flow: flow, remote: remoteConn, lateRead: lateRead, establishGuard: establishGuard)
            relayRemoteToFlow(flowId: flowId, flow: flow, remote: remoteConn, establishGuard: establishGuard)
        }
    }

    /// startAppToRemoteRelay begins the app→remote relay direction. When
    /// the flow went through a timed-out SNI peek, its first readData is
    /// STILL OUTSTANDING (Apple permits one read in flight per flow) and
    /// the bytes it eventually delivers are the app's real first payload —
    /// a server-speaks-first protocol's (SMTP/FTP/MySQL) first
    /// post-greeting write. This helper consumes that adopted completion
    /// as the relay's first chunk, then continues the normal read loop;
    /// without it the relay would race a second readData and the adopted
    /// completion's bytes would be silently dropped (app→upstream dead).
    private func startAppToRemoteRelay(flowId: String, flow: NEAppProxyTCPFlow, remote: NWTCPConnection, lateRead: LatePeekRead?, establishGuard: TimeoutGuard? = nil) {
        guard let lateRead = lateRead else {
            relayFlowToRemote(flowId: flowId, flow: flow, remote: remote, establishGuard: establishGuard)
            return
        }
        lateRead.consume { [weak self] data, error in
            guard let self else { return }
            if let error = error {
                self.logger.debug("startAppToRemoteRelay: adopted peek read ended for \(flowId): \(error.localizedDescription)")
                remote.cancel()
                return
            }
            guard let data = data, !data.isEmpty else {
                // App closed its write side before sending — propagate FIN.
                remote.writeClose()
                return
            }
            self.addBytesOut(flowId: flowId, count: Int64(data.count))
            remote.write(data) { writeError in
                if let writeError = writeError {
                    self.logger.info("startAppToRemoteRelay: WRITE upstream FAILED flow=\(flowId, privacy: .public) bytes=\(data.count) err=\(writeError.localizedDescription, privacy: .public) — remote likely closed; relay tearing down")
                    return
                }
                _ = establishGuard?.tryFire()
                self.relayFlowToRemote(flowId: flowId, flow: flow, remote: remote)
            }
        }
    }

    /// Read data from the app (via flow.readData) and forward to the remote server.
    ///
    /// establishGuard (SEC-M8-02) is non-nil only on the first call of a
    /// passthrough flow that had no peek bytes to anchor establishment on;
    /// the first successful upstream write disarms the establishment
    /// watchdog. It is nil on every other call (recursion and paths whose
    /// watchdog is already disarmed by a peek/header write).
    private func relayFlowToRemote(flowId: String, flow: NEAppProxyTCPFlow, remote: NWTCPConnection, establishGuard: TimeoutGuard? = nil) {
        flow.readData(completionHandler: { [weak self] data, error in
            if let error = error {
                self?.logger.debug("App→Remote read done for \(flowId): \(error.localizedDescription)")
                remote.cancel()
                return
            }
            guard let data = data, !data.isEmpty else {
                // App finished sending — send FIN to remote.
                remote.writeClose()
                return
            }

            self?.addBytesOut(flowId: flowId, count: Int64(data.count))

            remote.write(data) { writeError in
                if let writeError = writeError {
                    // INFO: a write error to the upstream socket means the
                    // remote dropped while the app was still sending —
                    // visible to the user as "page upload froze" / failed
                    // POST / dropped websocket. Always actionable.
                    self?.logger.info("relayFlowToRemote: WRITE upstream FAILED flow=\(flowId, privacy: .public) bytes=\(data.count) err=\(writeError.localizedDescription, privacy: .public) — remote likely closed; relay tearing down")
                    return
                }
                // First successful upstream write proves the connection is
                // usable → disarm the establishment watchdog (SEC-M8-02).
                // One-shot, so this no-ops on recursion; recurse with no
                // guard.
                _ = establishGuard?.tryFire()
                self?.relayFlowToRemote(flowId: flowId, flow: flow, remote: remote)
            }
        })
    }

    /// Read data from the remote server and forward to the app (via flow.write).
    ///
    /// establishGuard (SEC-M8-02) is non-nil only on the first call of a
    /// no-peek passthrough flow. ANY completion of the first remote read —
    /// data delivered OR an error surfaced (e.g. RST / cancel) — proves the
    /// connection is not black-holed and disarms the establishment
    /// watchdog; a black-holed connection never fires this completion, so
    /// the watchdog stays armed and resets the flow. This is the remote-
    /// side establishment signal that complements relayFlowToRemote's
    /// app→remote write-success signal, so a server-speaks-first protocol
    /// (SSH/SMTP/FTP/IMAP) routed through no-peek passthrough is not reset
    /// while it legitimately waits for the server's first bytes.
    private func relayRemoteToFlow(flowId: String, flow: NEAppProxyTCPFlow, remote: NWTCPConnection, establishGuard: TimeoutGuard? = nil) {
        remote.readMinimumLength(1, maximumLength: 65536) { [weak self] data, error in
            // First remote-read completion (data or error) disarms the
            // establishment watchdog. One-shot; recursion passes no guard.
            _ = establishGuard?.tryFire()
            if let error = error {
                self?.logger.debug("Remote→App read done for \(flowId): \(error.localizedDescription)")
                flow.closeReadWithError(nil)
                self?.completeFlow(flowId: flowId)
                return
            }
            guard let data = data, !data.isEmpty else {
                flow.closeReadWithError(nil)
                self?.completeFlow(flowId: flowId)
                return
            }

            self?.addBytesIn(flowId: flowId, count: Int64(data.count))

            flow.write(data) { writeError in
                if let writeError = writeError {
                    // INFO: a write error to the app side means the user's
                    // app dropped the connection (closed tab, killed app,
                    // socket reset) before all upstream bytes arrived.
                    // Visible to the user as "download truncated" /
                    // "page partially loaded then froze". Always actionable.
                    self?.logger.info("relayRemoteToFlow: WRITE app FAILED flow=\(flowId, privacy: .public) bytes=\(data.count) err=\(writeError.localizedDescription, privacy: .public) — app likely closed; relay tearing down")
                    remote.cancel()
                    self?.completeFlow(flowId: flowId)
                    return
                }
                self?.relayRemoteToFlow(flowId: flowId, flow: flow, remote: remote)
            }
        }
    }

    // MARK: - Flow State Management

    private func addBytesIn(flowId: String, count: Int64) {
        flowLock.lock()
        activeFlows[flowId]?.bytesIn += count
        flowLock.unlock()
    }

    private func addBytesOut(flowId: String, count: Int64) {
        flowLock.lock()
        activeFlows[flowId]?.bytesOut += count
        flowLock.unlock()
    }

    private func updateBumpStatus(flowId: String, status: String) {
        flowLock.lock()
        activeFlows[flowId]?.bumpStatus = status
        flowLock.unlock()
    }

    private func completeFlow(flowId: String) {
        flowLock.lock()
        guard let state = activeFlows.removeValue(forKey: flowId) else {
            flowLock.unlock()
            return
        }
        flowLock.unlock()

        // Per-flow terminal summary at INFO so any "this flow froze for
        // the user" investigation can pivot directly from flow_id to the
        // exit reason (bytes seen / duration / bumpStatus). The bumpStatus
        // tells you which path the flow took: empty=passthrough,
        // BUMP_SUCCESS=inspect, BUMP_FAILED_PASSTHROUGH=inspect-attempted-
        // then-fallback. duration_ms near 0 with 0 bytes = flow was
        // reset before any relay (typically flow.open failed and we
        // closeRead'd per the #82 fail-open contract).
        let durationMs = Int(Date().timeIntervalSince(state.startTime) * 1000)
        logger.info("completeFlow: flow=\(flowId, privacy: .public) bytes_in=\(state.bytesIn) bytes_out=\(state.bytesOut) duration_ms=\(durationMs) bump_status=\(state.bumpStatus, privacy: .public)")

        reportFlowClosed(flowId: flowId, state: state)
    }

    private func reportFlowClosed(flowId: String, state: FlowState) {
        let durationMs = Int(Date().timeIntervalSince(state.startTime) * 1000)
        // E50: forward optional phase fields. nil when URLSession metrics
        // weren't captured this flow (passthrough / raw TCP relay path).
        ipcClient.notifyFlowClosed(
            flowId: flowId,
            bytesIn: state.bytesIn,
            bytesOut: state.bytesOut,
            durationMs: durationMs,
            bumpStatus: state.bumpStatus,
            upstreamTtfbMs: state.upstreamTtfbMs,
            upstreamTotalMs: state.upstreamTotalMs,
            interceptMs: state.interceptMs
        )
    }

    /// E50 phase setters — called by the URLSession metrics delegate when
    /// upstream task metrics surface (typically `urlSession(_:task:didFinishCollecting:)`).
    /// `metrics.taskInterval.duration` ≈ upstreamTotalMs and
    /// `responseStartDate - requestStartDate` ≈ upstreamTtfbMs.
    func stampPhaseMetrics(flowId: String,
                           upstreamTtfbMs: Int?,
                           upstreamTotalMs: Int?) {
        flowLock.lock()
        defer { flowLock.unlock() }
        guard var state = activeFlows[flowId] else { return }
        if let v = upstreamTtfbMs { state.upstreamTtfbMs = v }
        if let v = upstreamTotalMs { state.upstreamTotalMs = v }
        activeFlows[flowId] = state
    }

    // MARK: - Process Resolution

    /// Extract PID from the flow's metadata audit token.
    private func extractPID(from flow: NEAppProxyFlow) -> Int32 {
        // audit_token_t is a struct of 8 uint32 values. Index 5 holds the PID
        // per the XNU kernel convention (same field used by SecCodeCopyGuestWithAttributes).
        // This avoids the private audit_token_to_pid() symbol that is not exported
        // from the public SDK and causes linker errors.
        guard let tokenData = flow.metaData.sourceAppAuditToken else { return 0 }
        return tokenData.withUnsafeBytes { buf -> Int32 in
            guard buf.count >= MemoryLayout<audit_token_t>.size else { return 0 }
            // PID is at index 5 of the val[8] array in audit_token_t.
            // loadUnaligned, because Data's backing buffer carries no
            // alignment guarantee and an aligned load(as:) traps on a
            // misaligned pointer — a crash in the packet path takes the
            // host's networking down with the provider.
            return Int32(bitPattern: buf.loadUnaligned(
                fromByteOffset: 5 * MemoryLayout<UInt32>.stride, as: UInt32.self))
        }
    }

    /// Cheap test: dotted-quad (a.b.c.d) or colon-hex (xxx:yyy:…) ⇒ IP literal.
    /// Avoids DNS round-trip when endpointAddr is already an IP, which is
    /// the normal case for callers that pre-resolved DNS (curl, browsers).
    private func isLikelyIPLiteral(_ s: String) -> Bool {
        if s.isEmpty { return false }
        return s.contains(":") || (s.unicodeScalars.allSatisfy { c in
            (c >= "0" && c <= "9") || c == "."
        } && s.contains("."))
    }

    private func extractLocalPort(from flow: NEAppProxyFlow) -> Int {
        // Intentional: NEAppProxyFlow doesn't reliably expose the local
        // port across macOS versions, and the Go agent never reads this
        // value for any routing/decision. Returning 0 is the deliberate,
        // permanent fallback — not a stub. Do not "fix" this to chase a
        // port the decision path doesn't consume.
        return 0
    }

}
