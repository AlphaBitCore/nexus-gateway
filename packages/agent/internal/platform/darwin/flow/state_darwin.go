//go:build darwin

// Package flow contains the flow state machine types used by the macOS
// DarwinPlatform to track in-flight NE flows from flow_new through flow_closed.
package flow

import "time"

// State holds per-flow tracking state from flow_new through flow_closed.
// Field names are exported so platform.darwin can access them directly.
type State struct {
	// Connection identity — populated by flow_new.
	FlowID  string
	DstHost string
	DstIP   string
	DstPort int
	SrcIP   string
	SrcPort int

	// Process metadata — populated by flow_new via proc.ProcessInfo.
	ProcPID      int
	ProcPath     string
	ProcName     string
	ProcBundleID string
	ProcUser     string

	// Decision returned by the connection handler at flow_new time.
	DecisionInt int // 0=inspect, 1=passthrough, 2=deny (mirrors platform.Decision)

	// Flow lifecycle.
	StartedAt time.Time

	// Request-side signals populated by handleFlowInspect when the
	// compliance hook pipeline runs against the MITM'd request.
	Provider          string
	Model             string
	ApiKeyClass       string
	ApiKeyFingerprint string

	// HTTP request method + path captured at flow_inspect time. Without
	// these the audit_event method/path columns would always be empty
	// because the Swift NEFlowClosedMessage doesn't carry them.
	Method string
	Path   string

	// Response-side usage populated by handleFlowInspectResponse.
	PromptTokens          *int
	CompletionTokens      *int
	UsageExtractionStatus string

	// Payload capture bytes stashed by handleFlowInspect /
	// handleFlowInspectResponse when the admin has enabled the
	// corresponding capture flag. Nil when disabled.
	PayloadRequest  []byte
	PayloadResponse []byte
}
