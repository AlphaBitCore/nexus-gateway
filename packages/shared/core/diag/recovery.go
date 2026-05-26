package diag

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"runtime/debug"
	"time"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// RecoveryConfig parameterizes the deferred panic handler. A single value is
// shared across a service's main + every long-lived goroutine so all crash
// events land in the same buffer; per-goroutine nuance is captured by
// overriding Source with a label like "audit-drain" or "intercept" in a
// copy of the config.
type RecoveryConfig struct {
	// ThingID is the service's registered thing ID. Stamped onto every crash
	// event so the Hub-side handler can fall back to it when the auth context
	// is unavailable.
	ThingID string

	// Buffer is an optional durable crash buffer (e.g. the agent's SQLCipher
	// pending_diag_event store). Best-effort: a nil or failing buffer must not
	// block the re-panic. Non-agent services typically pass nil.
	Buffer LocalBufferInserter

	// AgentVersion is the build tag stamped into DiagEvent.AgentVersion so
	// the Control Plane UI can surface "crash on v1.4.2 only" patterns.
	// For non-agent services, populate this with the service's build version.
	AgentVersion string

	// OSInfo is optional static metadata (os, osVersion, kernelVersion).
	// Pass by reference so every recovery site shares the same map.
	OSInfo map[string]any

	// Source labels the goroutine that crashed. Defaults to "main" when
	// empty — call sites set it explicitly per goroutine ("audit-drain",
	// "intercept", …) so dashboards can group by site.
	Source string
}

// Recover is the deferred panic handler. It captures the panic value, builds
// a FATAL crash DiagEvent, persists it via the Buffer (when non-nil),
// optionally calls finalize (e.g. flushing log file handles), then RE-panics
// so the OS crash reporter still fires and the supervisor's restart logic
// engages.
//
// Usage:
//
//	defer diag.Recover(cfg, nil)
//
// At every long-lived goroutine entry.
func Recover(cfg RecoveryConfig, finalize func()) {
	r := recover()
	if r == nil {
		return
	}

	source := cfg.Source
	if source == "" {
		source = "main"
	}

	stack := debug.Stack()
	msg := fmt.Sprintf("%v", r)

	hash := md5.Sum([]byte(opsmetrics.LevelFatal + "|" + source + "|" + msg))
	evt := opsmetrics.DiagEvent{
		ThingID:      cfg.ThingID,
		OccurredAt:   time.Now().UTC(),
		Level:        opsmetrics.LevelFatal,
		EventType:    opsmetrics.EventTypeCrash,
		Source:       source,
		Message:      msg,
		MessageHash:  hex.EncodeToString(hash[:]),
		StackTrace:   string(stack),
		RepeatCount:  1,
		AgentVersion: cfg.AgentVersion,
		OSInfo:       cfg.OSInfo,
	}

	if cfg.Buffer != nil {
		// Best-effort: a failing buffer must not block the re-panic.
		_ = cfg.Buffer.Insert(evt)
	}

	if finalize != nil {
		finalize()
	}

	// Re-panic so the OS crash reporter still fires and any outer recover
	// (e.g. a goroutine wrapper that wants to log + continue) gets to act.
	panic(r)
}
