import Foundation

/// IPC client used by the slim menu-bar app (E40 Phase 1).
///
/// Talks to the Go daemon's statusapi over a Unix socket. Only the
/// commands the seven-item NSMenu actually needs are exposed:
/// GET_STATUS, CHECK_UPDATE, PAUSE_PROTECTION, RESUME_PROTECTION,
/// SHUTDOWN. Every other command (QUERY_EVENTS, AUTHENTICATE*,
/// ENROLL_TOKEN, SYNC_CONFIG, GET_RUNTIME) belongs to the Wails
/// Dashboard once Phase 2 lands, and is invoked from the Go side
/// over its own statusapi client.
actor StatusClient {
    private let socketPath: String = {
        // Match the Go agent's guiSocketPath() logic:
        //   macOS (this code only ever runs here): /var/run/nexus-agent-status.sock
        //     — daemon is a root LaunchDaemon, this menu-bar app runs as
        //     the user; system-wide /var/run/ is the only path both can
        //     reach. The daemon chmods the socket 0666 on macOS for
        //     cross-UID connect (single-user desktop scope, see
        //     statusapi/listen_other.go).
        //   Linux fallback (in case someone ports this Swift to Linux):
        //     XDG_RUNTIME_DIR → ~/.nexus/.
        #if os(macOS)
        return "/var/run/nexus-agent-status.sock"
        #else
        if let xdg = ProcessInfo.processInfo.environment["XDG_RUNTIME_DIR"], !xdg.isEmpty {
            return (xdg as NSString).appendingPathComponent("nexus-agent-status.sock")
        }
        let home = NSHomeDirectory()
        return (home as NSString).appendingPathComponent(".nexus/agent-status.sock")
        #endif
    }()

    func getStatus() async throws -> StatusSnapshot {
        try await sendCommand("GET_STATUS")
    }

    func checkUpdate() async throws -> UpdateCheckResponse {
        try await sendCommand("CHECK_UPDATE")
    }

    func shutdown() async throws -> ShutdownResponse {
        try await sendCommand("SHUTDOWN")
    }

    /// PAUSE_PROTECTION engages the local kill switch. seconds=0
    /// pauses indefinitely; seconds>0 schedules an auto-resume.
    func pauseProtection(seconds: Int) async throws -> PauseResponse {
        let cmd = seconds > 0 ? "PAUSE_PROTECTION?seconds=\(seconds)" : "PAUSE_PROTECTION"
        return try await sendCommand(cmd)
    }

    /// RESUME_PROTECTION cancels any auto-resume timer and disengages
    /// the kill switch. Safe to call when not paused.
    func resumeProtection() async throws -> PauseResponse {
        try await sendCommand("RESUME_PROTECTION")
    }

    /// VERSION returns the daemon's build identity. The menu-bar app
    /// surfaces this so the user can see "Nexus Agent vXXXXXXXX" at a
    /// glance without opening About or running a CLI.
    func version() async throws -> VersionInfoResponse {
        try await sendCommand("VERSION")
    }

    /// UNENROLL clears the device cert + token from the daemon's
    /// keychain. Used by the menu's Switch identity / Sign Out
    /// submenu. The daemon respawns into pending-enrollment mode;
    /// the Dashboard will show the onboarding screen next time the
    /// user opens it. acknowledged=false carries an error string
    /// when the IPC handler is wired but the unenroll fails (e.g.
    /// keychain locked) — surface that to the menu's transient
    /// footer.
    func signOut() async throws -> ShutdownResponse {
        try await sendCommand("UNENROLL")
    }

    /// REPORT_PROXY_INSTALL forwards the outcome of a system-extension
    /// activation or NETransparentProxyManager.saveToPreferences
    /// attempt to the daemon so the result lands in agent.log
    /// (and any diagnostics bundle). The body is the JSON-encoded
    /// ProxyInstallReport, sent verbatim after the '?' separator.
    func reportProxyInstall(_ report: ProxyInstallReport) async throws -> AckResponse {
        let payload = try JSONEncoder().encode(report)
        guard let json = String(data: payload, encoding: .utf8) else {
            throw IPCError.emptyResponse
        }
        return try await sendCommand("REPORT_PROXY_INSTALL?\(json)")
    }

    private func sendCommand<T: Decodable>(_ command: String) async throws -> T {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else {
            throw IPCError.socketCreationFailed
        }
        defer { close(fd) }

        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let pathBytes = socketPath.utf8CString
        withUnsafeMutablePointer(to: &addr.sun_path) { ptr in
            ptr.withMemoryRebound(to: CChar.self, capacity: Int(104)) { dest in
                for (i, byte) in pathBytes.enumerated() where i < 104 {
                    dest[i] = byte
                }
            }
        }

        let connectResult = withUnsafePointer(to: &addr) { ptr in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { sockPtr in
                Darwin.connect(fd, sockPtr, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard connectResult == 0 else {
            throw IPCError.connectionFailed(errno: errno)
        }

        // Send command
        let message = command + "\n"
        message.withCString { ptr in
            _ = send(fd, ptr, message.utf8.count, 0)
        }

        // Read response
        var data = Data()
        let bufferSize = 65536
        let buffer = UnsafeMutablePointer<UInt8>.allocate(capacity: bufferSize)
        defer { buffer.deallocate() }

        while true {
            let bytesRead = recv(fd, buffer, bufferSize, 0)
            if bytesRead <= 0 { break }
            data.append(buffer, count: bytesRead)
            if data.last == UInt8(ascii: "\n") { break }
        }

        guard !data.isEmpty else {
            throw IPCError.emptyResponse
        }

        return try JSONDecoder().decode(T.self, from: data)
    }
}

enum IPCError: LocalizedError {
    case socketCreationFailed
    case connectionFailed(errno: Int32)
    case emptyResponse

    var errorDescription: String? {
        switch self {
        case .socketCreationFailed:
            return "Failed to create Unix socket"
        case .connectionFailed(let errno):
            return "Connection failed (errno: \(errno)). Is the agent running?"
        case .emptyResponse:
            return "Empty response from agent"
        }
    }
}
