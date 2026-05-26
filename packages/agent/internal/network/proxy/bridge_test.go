package proxy

// bridge_test.go: the older captureWriter snapshot pattern was deleted
// because it could not represent N HTTP requests on one TCP flow
// (HTTP/2 multiplexed chatgpt.com browsing was producing 1 audit row
// instead of N).
//
// The new contract is: tlsbump.AuditEmitter writes per-HTTP-request
// audit rows directly to the agent's SQLite Queue via
// agentaudit.NewQueueWriter. There is no in-process snapshot to
// pin in a unit test. End-to-end coverage lives in:
//   - packages/shared/transport/tlsbump/forward_handler_test.go
//     (verifies emitter is called per request)
//   - packages/shared/policy/pipeline/audit_emitter_builder_test.go
//   - packages/shared/policy/pipeline/audit_emitter_helpers_test.go
//     (verify AuditEvent → Writer.Enqueue mapping)
//   - packages/agent/internal/observability/audit/queue/queue_test.go
//     (verifies Queue.Record persistence + Classify routing)
//
// A future agent-bridge integration test should fake NEAppProxyFlow +
// a real agent.Queue and assert N rows after a multiplexed request
// stream — but that needs a richer test harness than this file
// currently provides.
