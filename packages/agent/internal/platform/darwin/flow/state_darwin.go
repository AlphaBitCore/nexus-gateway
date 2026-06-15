//go:build darwin

// Package flow contains the flow state machine types used by the macOS
// DarwinPlatform to track in-flight NE flows from flow_new through flow_closed.
package flow

import (
	"sync/atomic"
	"time"
)

// State holds per-flow tracking state from flow_new through flow_closed.
//
// Concurrency contract (the decision is computed on a worker goroutine
// off the IPC reader): the reader registers the State synchronously with
// the identity fields before any later frame for the same flow is read,
// so flow_update_host and flow_closed always find it. The worker is the
// SOLE writer of the process-metadata and Decision fields, and publishes
// them by storing Ready=true LAST. Any reader of those fields
// (flow_closed, LookupFlowDestination) must observe Ready.Load()==true
// first — that load/store pair carries the happens-before, so no per-field
// lock is needed.
//
// DstHost is the one mutable identity field read CROSS-goroutine: the IPC
// reader writes it (registration, then flow_update_host on SNI), while a
// per-connection BRIDGE goroutine reads it via LookupFlowDestination for
// leaf-cert SAN matching — unsynchronized that is a data race (a torn
// string read). It is therefore an atomic.Pointer[string] accessed only
// through DstHost()/SetDstHost(), never as a plain field.
type State struct {
	// Connection identity — populated by flow_new on the reader goroutine.
	FlowID  string
	dstHost atomic.Pointer[string]
	DstIP   string
	DstPort int
	SrcIP   string
	SrcPort int

	// Process metadata — written by the decision worker; read only after
	// Ready is observed true.
	ProcPID      int
	ProcPath     string
	ProcName     string
	ProcBundleID string
	ProcUser     string

	// Decision returned by the connection handler. Written by the worker;
	// read only after Ready is observed true.
	DecisionInt int // 0=inspect, 1=passthrough, 2=deny (mirrors platform.Decision)

	// Ready is stored true by the worker AFTER it has written every
	// process-metadata + Decision field, publishing them to any reader.
	Ready atomic.Bool

	// Flow lifecycle.
	StartedAt time.Time
}

// DstHost returns the destination host, safe to call from any goroutine
// (the bridge goroutine reads it concurrently with the IPC reader's
// SetDstHost). Empty until the reader registers the flow.
func (s *State) DstHost() string {
	if p := s.dstHost.Load(); p != nil {
		return *p
	}
	return ""
}

// SetDstHost atomically updates the destination host. Called only on the
// IPC reader goroutine (registration + flow_update_host), but published
// atomically so the bridge goroutine never observes a torn read.
func (s *State) SetDstHost(host string) {
	s.dstHost.Store(&host)
}
