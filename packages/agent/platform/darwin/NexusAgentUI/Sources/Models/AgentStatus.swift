import Foundation

// Slim model surface for the seven-item NSMenu.
//
// The full StatusSnapshot the Go daemon emits is rich (today stats,
// recent events, policy rules, runtime snapshots, config summary,
// shutdown warning, …). The menu bar only reads two fields:
// `state` (for the tray icon tint) and `agent.deviceID` (to decide
// whether the daemon is in pending-enrollment mode). Everything
// else is decoded permissively — extra keys on the wire are
// ignored — so the daemon can continue evolving its snapshot
// shape without breaking the menu.

struct StatusSnapshot: Decodable {
    let state: String           // "active" | "degraded" | "error"
    let stateReason: String
    let agent: AgentInfo
    // paused mirrors the daemon's kill switch. true when either the
    // user (via this menu) or an admin (via Hub shadow) has engaged
    // it. The menu reads this so admin-initiated pauses surface
    // here too instead of the menu showing stale "Active" state.
    let paused: Bool?
    /// RFC3339 timestamp when a finite user-pause will auto-resume.
    /// Empty / nil when paused indefinitely or not paused.
    let pausedUntil: String?
    /// Per-locale shutdown warning text admin configured. The menu's
    /// Quit handler picks the matching locale and shows it as a
    /// confirm dialog before triggering the quit flow. nil / empty
    /// means "no warning" (admin disabled or never configured); the
    /// daemon already gates on shutdownWarningEnabled so the map is
    /// only populated when both enabled AND text is present.
    let shutdownWarning: [String: String]?
}

struct AgentInfo: Decodable {
    let deviceID: String
    /// Email captured during the most recent SSO enrollment. Empty
    /// in mtls-only deployments and pre-enrollment.
    let ssoEmail: String?
    /// Daemon build identity (e.g. "20260513-bootstrap-warm-fix+local-2").
    /// Surfaced in the menu header so the user can verify "did my
    /// update land" at a glance. Optional for back-compat with old
    /// daemons that did not include this field in GET_STATUS.
    let version: String?
    /// Runtime policy on whether the SHUTDOWN IPC will be honoured.
    /// Used by the menu builder to decide whether to surface the
    /// "Restart Agent" and "Quit Nexus Agent" affordances at all —
    /// when false (the prod compliance always-on default) both items
    /// are omitted so users never click through to a "blocked by
    /// policy" error. Optional + defaults to `true` for back-compat
    /// with older daemons that did not emit the field.
    let quitAllowed: Bool?
    /// True when the daemon's updater has seen a newer build
    /// available on Hub. Drives the "Update available — install"
    /// banner in the menu and on the Dashboard. Optional for
    /// back-compat with older daemons that didn't surface this in
    /// GET_STATUS.
    let updateAvailable: Bool?
    /// RFC3339 timestamp of the most recent LLM-provider call this
    /// agent intercepted. Empty string when no provider traffic has
    /// occurred since the daemon started. The menu-bar polls this
    /// and renders a brief tray-icon highlight when the timestamp
    /// is within the last ~3 seconds — gives the user "agent is
    /// doing AI work right now" feedback without being noisy.
    let lastProviderTrafficAt: String?
}

// ─── Command responses ───────────────────────────────────────────────

struct UpdateCheckResponse: Decodable {
    let available: Bool
    let version: String?
    let error: String?
}

struct ShutdownResponse: Decodable {
    let acknowledged: Bool
    /// Populated when the daemon refuses the shutdown (e.g. operator
    /// set quitAllowed=false in the config). The menu surfaces this
    /// via the transient footer instead of failing silently.
    let error: String?
}

/// Shared response shape for PAUSE_PROTECTION and RESUME_PROTECTION.
/// `resumesAt` is populated only by PAUSE with a finite duration.
struct PauseResponse: Decodable {
    let paused: Bool
    let resumesAt: String?
    let error: String?

    enum CodingKeys: String, CodingKey {
        case paused
        case resumesAt = "resumes_at"
        case error
    }
}

/// Result of the VERSION IPC command — exposes the daemon's build
/// identity so the menu-bar surface can prove "this is the build that
/// landed" without resorting to log files.
struct VersionInfoResponse: Decodable {
    let version: String
    let commit: String?
    let builtAt: String?
    let os: String?
    let arch: String?
}

/// Wire payload sent over REPORT_PROXY_INSTALL. Keep field naming
/// aligned with the Go-side statusapi.ProxyInstallReport — JSON tags
/// are the contract; the Swift Encodable lowercases by default which
/// already matches.
struct ProxyInstallReport: Encodable {
    let stage: String
    let outcome: String
    let error: String?
    let appVersion: String?

    init(stage: String, outcome: String, error: String? = nil, appVersion: String? = nil) {
        self.stage = stage
        self.outcome = outcome
        self.error = error
        self.appVersion = appVersion
    }
}

/// Generic acknowledgement payload returned by commands that don't
/// have a richer reply (REPORT_PROXY_INSTALL today).
struct AckResponse: Decodable {
    let acknowledged: Bool
    let error: String?
}

// ─── Tray icon state ─────────────────────────────────────────────────

/// Coarse-grained agent state used to tint the menu-bar icon.
enum AgentState: String {
    case active, degraded, error

    var systemImage: String {
        switch self {
        case .active: return "shield.checkmark.fill"
        case .degraded: return "exclamationmark.shield.fill"
        case .error: return "xmark.shield.fill"
        }
    }
}
