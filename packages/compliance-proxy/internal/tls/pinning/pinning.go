// Package pinning is reserved for compliance-proxy-local TLS pinning
// helpers. The active pinning tracker lives in
// packages/shared/transport/tlsbump (tlsbump.PinningTracker) and is wired
// via cmd/compliance-proxy/wiring/pinning.go. This package is the home for
// any future compliance-proxy-specific pinning overrides or decorators that
// cannot live in the shared layer.
package pinning
