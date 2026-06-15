import Foundation
import ServiceManagement
import os.log

/// LaunchServiceManager registers the two launchd jobs that make up the agent
/// using `SMAppService` (macOS 13+), the modern replacement for the deprecated
/// `SMJobBless` / `SMLoginItemSetEnabled`:
///
///   1. **The root boot daemon** — `SMAppService.daemon(plistName:)` registers a
///      *system-domain* LaunchDaemon (root, RunAtLoad) from a plist embedded in
///      the app bundle at `Contents/Library/LaunchDaemons/`. This is the whole
///      reason for the migration: registration is now tied to the `.app` bundle,
///      so deleting the app deregisters the daemon (no orphaned
///      `/Library/LaunchDaemons` plist, no launchd spawn-error spam, no
///      `disabled.plist`-override EIO at install time). The daemon stays a
///      **boot** daemon — pre-login enforcement is preserved; we do not downgrade
///      to a per-user login agent.
///
///   2. **The menu-bar app as a Login Item** — `SMAppService.mainApp` registers
///      the app itself to auto-launch at login, replacing the old per-user
///      `LaunchAgent.plist` `open -g` shim with a native System Settings → Login
///      Item the user can see and toggle.
///
/// SMAppService is exactly the mechanism that lets this *unprivileged* menu-bar
/// app (it runs as the console user, not root) register a *system* daemon: the
/// privileged registration is brokered by `smd`/launchd with a one-time user (or
/// MDM) approval — no `sudo`, no `launchctl bootstrap`. On an MDM-managed device
/// a `com.apple.servicemanagement` profile pre-approves both, so `register()`
/// lands directly in `.enabled` with no prompt.
///
/// The NE system extension is **not** managed here — it stays on
/// `OSSystemExtensionRequest` (`SystemExtensionManager`), which already ties the
/// extension to the bundle and schedules it for removal on app-delete.
@MainActor
enum LaunchServiceManager {

    /// Plist file name as it ships inside the app bundle at
    /// `Contents/Library/LaunchDaemons/`. `SMAppService.daemon(plistName:)`
    /// resolves it relative to that directory; the string must match the file
    /// name exactly.
    static let daemonPlistName = "com.nexus-gateway.agent.plist"

    private static let logger = Logger(subsystem: "com.nexus-gateway.agent", category: "LaunchService")

    private static var daemonService: SMAppService {
        SMAppService.daemon(plistName: daemonPlistName)
    }

    /// True when the daemon registration still needs the user to approve it in
    /// System Settings. The menu uses this to decide whether to surface the
    /// one-line "finish setup" affordance. On a managed (pre-approved) device the
    /// status is `.enabled`, so this is false and no row is shown — the approval
    /// UI is unmanaged-only by construction, with no MDM-detection code needed.
    static var daemonNeedsApproval: Bool {
        daemonService.status == .requiresApproval
    }

    /// Register both launchd jobs. Idempotent and safe to call on every launch:
    /// re-registering an already-enabled service is a no-op that also refreshes
    /// the bundle path after an update (P3). Best-effort — failures are logged,
    /// never fatal; `status`/`daemonNeedsApproval` drive any user-visible follow-up.
    static func register() {
        registerDaemon()
        registerLoginItem()
    }

    private static func registerDaemon() {
        let service = daemonService
        // `.enabled` already → nothing to do. `.requiresApproval` → register()
        // would throw `.operationNotPermitted`; the user must approve in Settings,
        // so we log and let the menu surface the affordance instead of spamming.
        switch service.status {
        case .enabled:
            logger.info("daemon already registered + enabled")
            return
        case .requiresApproval:
            logger.notice("daemon registration requires user approval in System Settings → Login Items & Extensions")
            return
        default:
            break
        }
        do {
            try service.register()
            logger.info("daemon registered via SMAppService (status now \(statusName(service.status), privacy: .public))")
        } catch {
            // Distinguish the benign "needs approval" case from a genuine
            // failure (signing/team mismatch, plist rejected, launch-constraint
            // error 163). On the benign case the post-call status is
            // .requiresApproval and the menu's finishSetup row guides the user;
            // anything else is a real fault that must NOT masquerade as
            // approval-pending — escalate it at fault level so it shows up in
            // the logs as a problem rather than a routine first-run prompt.
            let post = service.status
            if post == .requiresApproval || post == .enabled {
                logger.notice("daemon register pending approval: \(error.localizedDescription, privacy: .public) (status \(statusName(post), privacy: .public))")
            } else {
                logger.fault("daemon SMAppService.register FAILED (not an approval prompt): \(error.localizedDescription, privacy: .public) (status \(statusName(post), privacy: .public))")
            }
        }
    }

    private static func registerLoginItem() {
        let app = SMAppService.mainApp
        switch app.status {
        case .enabled:
            logger.info("login item already registered + enabled")
            return
        case .requiresApproval:
            // register() would throw every launch while approval is pending;
            // don't spam — the System Settings toggle is the user's to flip.
            logger.notice("login item registration requires user approval")
            return
        default:
            break
        }
        do {
            try app.register()
            logger.info("login item registered via SMAppService (status now \(statusName(app.status), privacy: .public))")
        } catch {
            logger.error("login item SMAppService.register failed: \(error.localizedDescription, privacy: .public)")
        }
    }

    /// Deep-link the user to the System Settings pane where both the Login Item
    /// (daemon) approval and the Network Extension approval live on macOS 13+.
    static func openLoginItemsSettings() {
        SMAppService.openSystemSettingsLoginItems()
    }

    /// Unregister both launchd jobs — the explicit uninstall path. Deleting the
    /// .app already deregisters them automatically (the SMAppService model), so
    /// this is for an in-place "remove the agent now" that does not delete the
    /// bundle. Best-effort: log failures, don't throw — uninstall must proceed.
    static func unregister() {
        do {
            try daemonService.unregister()
            logger.info("daemon unregistered via SMAppService")
        } catch {
            logger.error("daemon SMAppService.unregister failed: \(error.localizedDescription, privacy: .public)")
        }
        do {
            try SMAppService.mainApp.unregister()
            logger.info("login item unregistered via SMAppService")
        } catch {
            logger.error("login item SMAppService.unregister failed: \(error.localizedDescription, privacy: .public)")
        }
    }

    private static func statusName(_ s: SMAppService.Status) -> String {
        switch s {
        case .notRegistered: return "notRegistered"
        case .enabled: return "enabled"
        case .requiresApproval: return "requiresApproval"
        case .notFound: return "notFound"
        @unknown default: return "unknown(\(s.rawValue))"
        }
    }
}
