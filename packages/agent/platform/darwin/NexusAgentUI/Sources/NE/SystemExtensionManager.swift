import SystemExtensions
import os.log

final class SystemExtensionManager: NSObject {

    static let shared = SystemExtensionManager()

    private let extensionID = "com.nexus-gateway.agent.extension"
    private let logger = Logger(subsystem: "com.nexus-gateway.agent", category: "SysExt")
    private var completion: ((Result<Void, Error>) -> Void)?
    private var requestStartedAt: Date?

    func installIfNeeded(completion: @escaping (Result<Void, Error>) -> Void) {
        logger.info("installIfNeeded: start (extensionID=\(self.extensionID))")
        self.completion = completion
        self.requestStartedAt = Date()
        let request = OSSystemExtensionRequest.activationRequest(
            forExtensionWithIdentifier: extensionID,
            queue: .main
        )
        request.delegate = self
        logger.info("installIfNeeded: submitting OSSystemExtensionRequest.activationRequest — macOS will dispatch delegate callbacks: actionForReplacing/needsUserApproval/didFinish/didFail. Watch System Settings → General → Login Items & Extensions → Network Extensions if needsUserApproval fires.")
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
        logger.info("delegate.actionForReplacingExtension: existing.bundleVersion=\(existing.bundleVersion) existing.bundleShortVersion=\(existing.bundleShortVersion) → new.bundleVersion=\(ext.bundleVersion) new.bundleShortVersion=\(ext.bundleShortVersion); returning .replace (we always upgrade)")
        return .replace
    }

    func requestNeedsUserApproval(_ request: OSSystemExtensionRequest) {
        logger.info("delegate.requestNeedsUserApproval: macOS is awaiting user approval for extensionID=\(self.extensionID). User must visit System Settings → General → Login Items & Extensions → Network Extensions and approve 'NexusAgent (HORIZON PURPOSE SDN BHD)'. didFinishWithResult will fire only after the user clicks Approve.")
    }

    func request(_ request: OSSystemExtensionRequest,
                 didFinishWithResult result: OSSystemExtensionRequest.Result) {
        let elapsedMs = requestStartedAt.map { Int(Date().timeIntervalSince($0) * 1000) } ?? -1
        logger.info("delegate.didFinishWithResult: \(self.resultName(result)) after \(elapsedMs)ms. Extension is now activated; next step is TransparentProxyManager.enableIfNeeded → saveToPreferences → startVPNTunnel.")
        completion?(.success(()))
        completion = nil
        requestStartedAt = nil
    }

    func request(_ request: OSSystemExtensionRequest,
                 didFailWithError error: Error) {
        let nsErr = error as NSError
        let elapsedMs = requestStartedAt.map { Int(Date().timeIntervalSince($0) * 1000) } ?? -1
        logger.error("delegate.didFailWithError: extensionID=\(self.extensionID) after \(elapsedMs)ms — domain=\(nsErr.domain) code=\(nsErr.code) localized=\(error.localizedDescription). Common error codes (OSSystemExtensionErrorDomain): 1=unknown, 2=missingEntitlement, 3=unsupportedParentBundleLocation (.app must be in /Applications), 4=extensionNotFound, 5=extensionMissingIdentifier, 6=duplicateExtensionIdentifier, 7=unknownExtensionCategory, 8=codeSignatureInvalid, 9=validationFailed, 10=forbiddenBySystemPolicy, 11=requestCanceled, 12=requestSuperseded.")
        completion?(.failure(error))
        completion = nil
        requestStartedAt = nil
    }
}
