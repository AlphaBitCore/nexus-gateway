import SystemExtensions
import os.log

final class SystemExtensionManager: NSObject {

    static let shared = SystemExtensionManager()

    /// How an activation request actually left the extension. Callers
    /// must branch on this: after .pendingReboot the NEW extension is
    /// only staged — the previously-active one (if any) keeps serving
    /// until the Mac reboots, so "proceed and enable the proxy" runs
    /// against the OLD code and the user must be told a reboot is due.
    enum InstallOutcome {
        case active
        case pendingReboot
    }

    private let extensionID = "com.nexus-gateway.agent.extension"
    private let logger = Logger(subsystem: "com.nexus-gateway.agent", category: "SysExt")
    private var completion: ((Result<InstallOutcome, Error>) -> Void)?
    /// Separate completion for a deactivation request (uninstall path). Install
    /// and deactivation never run concurrently (deactivation is terminal), so a
    /// dedicated slot keeps the delegate routing unambiguous.
    private var deactivateCompletion: ((Result<Void, Error>) -> Void)?
    private var requestStartedAt: Date?
    /// Set when actionForReplacingExtension cancels because the staged
    /// extension is byte-for-byte the same version as the running one;
    /// the resulting requestCanceled "failure" then reports success.
    private var cancelledAsAlreadyCurrent = false

    func installIfNeeded(completion: @escaping (Result<InstallOutcome, Error>) -> Void) {
        logger.info("installIfNeeded: start (extensionID=\(self.extensionID))")
        self.completion = completion
        self.requestStartedAt = Date()
        self.cancelledAsAlreadyCurrent = false
        let request = OSSystemExtensionRequest.activationRequest(
            forExtensionWithIdentifier: extensionID,
            queue: .main
        )
        request.delegate = self
        logger.info("installIfNeeded: submitting OSSystemExtensionRequest.activationRequest — macOS will dispatch delegate callbacks: actionForReplacing/needsUserApproval/didFinish/didFail. Watch System Settings → General → Login Items & Extensions → Network Extensions if needsUserApproval fires.")
        OSSystemExtensionManager.shared.submitRequest(request)
    }

    /// Deactivate (uninstall) the system extension. This is the ONLY way to
    /// remove a system extension — the shell cannot under SIP. Used by the
    /// explicit uninstall flow (the bundle-delete path already schedules removal
    /// automatically post-P2, so this is for an in-place "remove the agent now"
    /// without deleting the .app). Like activation, macOS may defer completion to
    /// reboot; the caller is told via the result either way.
    func deactivate(completion: @escaping (Result<Void, Error>) -> Void) {
        logger.info("deactivate: start (extensionID=\(self.extensionID))")
        self.deactivateCompletion = completion
        self.requestStartedAt = Date()
        let request = OSSystemExtensionRequest.deactivationRequest(
            forExtensionWithIdentifier: extensionID,
            queue: .main
        )
        request.delegate = self
        OSSystemExtensionManager.shared.submitRequest(request)
    }

    /// Map OSSystemExtensionRequest.Result to a human name for logs.
    /// Apple's enum has .completed = 0 and .willCompleteAfterReboot = 1.
    private func resultName(_ r: OSSystemExtensionRequest.Result) -> String {
        switch r {
        case .completed: return "completed(0)"
        case .willCompleteAfterReboot: return "willCompleteAfterReboot(1)"
        @unknown default: return "unknown(\(r.rawValue))"
        }
    }
}

extension SystemExtensionManager: OSSystemExtensionRequestDelegate {

    func request(
        _ request: OSSystemExtensionRequest,
        actionForReplacingExtension existing: OSSystemExtensionProperties,
        withExtension ext: OSSystemExtensionProperties
    ) -> OSSystemExtensionRequest.ReplacementAction {
        // Same version on both sides: replacing would tear down and
        // re-approve an identical extension (worst case re-prompting the
        // user) for zero gain — cancel and report already-active. Any
        // version DIFFERENCE replaces, deliberately including downgrades:
        // reinstalling an older build is a legitimate recovery move.
        if existing.bundleVersion == ext.bundleVersion,
           existing.bundleShortVersion == ext.bundleShortVersion {
            logger.info("delegate.actionForReplacingExtension: existing \(existing.bundleShortVersion)(\(existing.bundleVersion)) == staged — cancelling, already current")
            cancelledAsAlreadyCurrent = true
            return .cancel
        }
        logger.info("delegate.actionForReplacingExtension: existing.bundleVersion=\(existing.bundleVersion) existing.bundleShortVersion=\(existing.bundleShortVersion) → new.bundleVersion=\(ext.bundleVersion) new.bundleShortVersion=\(ext.bundleShortVersion); returning .replace")
        return .replace
    }

    func requestNeedsUserApproval(_ request: OSSystemExtensionRequest) {
        logger.info("delegate.requestNeedsUserApproval: macOS is awaiting user approval for extensionID=\(self.extensionID). User must visit System Settings → General → Login Items & Extensions → Network Extensions and approve 'NexusAgent (YOUR ORG NAME)'. didFinishWithResult will fire only after the user clicks Approve.")
    }

    func request(_ request: OSSystemExtensionRequest,
                 didFinishWithResult result: OSSystemExtensionRequest.Result) {
        let elapsedMs = requestStartedAt.map { Int(Date().timeIntervalSince($0) * 1000) } ?? -1
        // Deactivation (uninstall) path: a willCompleteAfterReboot here means the
        // extension is removed but the running instance keeps serving until
        // reboot — report success either way (the caller surfaces the reboot
        // hint); there is no proxy to enable afterwards.
        if let deactivate = deactivateCompletion {
            logger.info("delegate.didFinishWithResult (deactivate): \(self.resultName(result)) after \(elapsedMs)ms")
            deactivate(.success(()))
            deactivateCompletion = nil
            requestStartedAt = nil
            return
        }
        switch result {
        case .willCompleteAfterReboot:
            // The new extension is only STAGED — the old one keeps
            // serving until reboot. Reporting this as plain success
            // would let the caller enable the proxy against the old
            // code and silently hide that a reboot is required.
            logger.error("delegate.didFinishWithResult: \(self.resultName(result)) after \(elapsedMs)ms — new extension staged but NOT active until the Mac reboots")
            completion?(.success(.pendingReboot))
        default:
            logger.info("delegate.didFinishWithResult: \(self.resultName(result)) after \(elapsedMs)ms. Extension is now activated; next step is TransparentProxyManager.enableIfNeeded → saveToPreferences → startVPNTunnel.")
            completion?(.success(.active))
        }
        completion = nil
        requestStartedAt = nil
    }

    func request(_ request: OSSystemExtensionRequest,
                 didFailWithError error: Error) {
        let nsErr = error as NSError
        let elapsedMs = requestStartedAt.map { Int(Date().timeIntervalSince($0) * 1000) } ?? -1
        // Deactivation (uninstall) path: route the failure to its own completion.
        if let deactivate = deactivateCompletion {
            logger.error("delegate.didFailWithError (deactivate): domain=\(nsErr.domain) code=\(nsErr.code) after \(elapsedMs)ms — \(error.localizedDescription)")
            deactivate(.failure(error))
            deactivateCompletion = nil
            requestStartedAt = nil
            return
        }
        // The already-current cancel is OUR OWN decision surfacing as a
        // requestCanceled error — the running extension is exactly the
        // requested version, so this is success, not failure.
        if cancelledAsAlreadyCurrent,
           nsErr.domain == OSSystemExtensionErrorDomain,
           nsErr.code == OSSystemExtensionError.requestCanceled.rawValue {
            logger.info("delegate.didFailWithError: requestCanceled after \(elapsedMs)ms — extension already at the staged version; reporting active")
            completion?(.success(.active))
            completion = nil
            requestStartedAt = nil
            return
        }
        logger.error("delegate.didFailWithError: extensionID=\(self.extensionID) after \(elapsedMs)ms — domain=\(nsErr.domain) code=\(nsErr.code) localized=\(error.localizedDescription). Common error codes (OSSystemExtensionErrorDomain): 1=unknown, 2=missingEntitlement, 3=unsupportedParentBundleLocation (.app must be in /Applications), 4=extensionNotFound, 5=extensionMissingIdentifier, 6=duplicateExtensionIdentifier, 7=unknownExtensionCategory, 8=codeSignatureInvalid, 9=validationFailed, 10=forbiddenBySystemPolicy, 11=requestCanceled, 12=requestSuperseded.")
        completion?(.failure(error))
        completion = nil
        requestStartedAt = nil
    }
}
