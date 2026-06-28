// Package platform is the OS-abstraction shim for the Nexus Agent.
// It re-exports the types and interfaces from platform/api and convenience
// functions from platform/paths and platform/catrust so callers can continue
// to import a single "platform" package while the implementation has been
// reorganised into OS-specific sub-packages.
//
// Canonical sub-package layout (post P9.8/P9.15):
//
//	platform/api/     — Platform interface, types, decision constants
//	platform/paths/   — OS-idiomatic path helpers
//	platform/catrust/ — OS-specific CA trust-store helpers
//	platform/darwin/  — DarwinPlatform + darwin sub-packages
//	platform/linux/   — LinuxPlatform
//	platform/windows/ — WindowsPlatform
package platform

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/api"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/catrust"
	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/platform/paths"
)

// Re-export all types from platform/api so callers that import
// "platform" continue to compile without modification.

// Platform is the OS-abstraction interface.
type Platform = api.Platform

// ProcessMeta contains metadata about the process that initiated a connection.
type ProcessMeta = api.ProcessMeta

// InterceptedConn represents a network connection captured by the platform shim.
type InterceptedConn = api.InterceptedConn

// Decision tells the platform shim how to handle an intercepted connection.
type Decision = api.Decision

const (
	DecisionInspect     = api.DecisionInspect
	DecisionPassthrough = api.DecisionPassthrough
	DecisionDeny        = api.DecisionDeny
)

// ConnectionHandler is called by the platform shim for each intercepted flow.
type ConnectionHandler = api.ConnectionHandler

// InterceptionMode identifies which kernel/userspace mechanism is active.
type InterceptionMode = api.InterceptionMode

const (
	ModeNETransparentProxy  = api.ModeNETransparentProxy
	ModeIPTables            = api.ModeIPTables
	ModeNexusWFP            = api.ModeNexusWFP
	ModeSystemProxyFallback = api.ModeSystemProxyFallback
)

// InterceptionModeReporter is the optional interface for surfacing the active mode.
type InterceptionModeReporter = api.InterceptionModeReporter

// ForceQUICFallbackController is the optional interface for forcing QUIC apps onto TCP.
type ForceQUICFallbackController = api.ForceQUICFallbackController

// InterceptionHealth captures whether the OS-level capture layer is attached.
type InterceptionHealth = api.InterceptionHealth

// InterceptionGracePeriod is how long the status collector waits after daemon startup.
const InterceptionGracePeriod = api.InterceptionGracePeriod

// InterceptionHealthReporter is the optional interface platform shims implement.
type InterceptionHealthReporter = api.InterceptionHealthReporter

// FlowResult contains the full outcome of an intercepted flow.
type FlowResult = api.FlowResult

// FlowAuditor is an optional interface that ConnectionHandler implementations may satisfy.
type FlowAuditor = api.FlowAuditor

// Paths holds the OS-idiomatic agent filesystem locations.
type Paths = paths.Paths

// DefaultPaths returns the canonical paths for the current OS.
var DefaultPaths = paths.DefaultPaths

// InstallCACert installs the given PEM-encoded CA certificate into the OS trust store.
var InstallCACert = catrust.InstallCACert
