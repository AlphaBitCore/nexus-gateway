// LatePeekRead.swift — handoff primitive for an in-flight SNI-peek read.
//
// Extracted verbatim from TransparentProxyProvider.swift; a self-contained
// synchronization helper with no dependency on NexusProxyProvider state.

import Foundation

/// LatePeekRead hands the outcome of a still-outstanding SNI-peek read to
/// the relay path chosen after the peek timed out. Apple permits one
/// readData in flight per flow, so the relay must adopt that read rather
/// than race a second one — and the bytes the late completion delivers are
/// the app's real first payload, which must reach the upstream, not be
/// dropped. Whichever side arrives second (the read completing, or the
/// relay registering its consumer) performs the handoff; both happen at
/// most once per flow.
final class LatePeekRead {
    private let lock = NSLock()
    private var outcome: (Data?, Error?)?
    private var consumer: ((Data?, Error?) -> Void)?

    /// deliver records the late read completion, or forwards it
    /// immediately when the relay's consumer is already registered.
    func deliver(_ data: Data?, _ error: Error?) {
        lock.lock()
        if let consumer = consumer {
            self.consumer = nil
            lock.unlock()
            consumer(data, error)
            return
        }
        outcome = (data, error)
        lock.unlock()
    }

    /// consume registers the relay's handler for the adopted read,
    /// firing immediately when the read already completed.
    func consume(_ handler: @escaping (Data?, Error?) -> Void) {
        lock.lock()
        if let outcome = outcome {
            self.outcome = nil
            lock.unlock()
            handler(outcome.0, outcome.1)
            return
        }
        consumer = handler
        lock.unlock()
    }
}
