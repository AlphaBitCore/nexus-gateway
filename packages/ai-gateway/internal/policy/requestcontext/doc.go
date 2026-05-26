// Package requestcontext defines the immutable per-request artefact that
// the ai-gateway request pipeline builds once and shares across the L4
// policy plane consumers (routing, hooks, audit).
//
// # Position in the layering
//
// The ai-gateway request path is structured into six layers:
//
//	L1  Transport             HTTP server, raw read/write
//	L2  Wire-format ingress   body bytes -> *normalize.NormalizedPayload
//	L3  Request context       this package
//	L4  Policy plane          routing, hooks, audit
//	L5  Wire-format egress    NormalizedPayload -> wire format
//	L6  Response normalize    wire format -> NormalizedPayload
//
// L3 is where the gateway crystallises everything an L4 consumer needs to
// make its decision: who the caller is, what canonical payload they sent,
// what endpoint family they hit, what inbound HTTP headers came with the
// request, and a handle on the original raw bytes for audit / spill. Once
// L3 hands a *RequestContext to a consumer, the consumer's input is fully
// determined; it does not parse the body, does not re-read the HTTP
// request, and does not share mutable state with peers.
//
// # Immutability promise
//
// RequestContext fields are unexported; consumers read them via
// pointer-receiver getters. By convention the struct is treated as
// read-only once Builder.Build() has returned it. Getters do not return
// defensive copies of slice / map fields; the underlying memory is owned
// by the original handler call site and must not be mutated by any
// downstream consumer. Mutation is a defect.
//
// # Build order
//
// The handler constructs a RequestContext after VK auth and rate limit have
// decided the request is admitted, before routing runs. The contract is
// "exactly one normalize call per request": the handler invokes
// normalize.Registry.Normalize once, hands the *NormalizedPayload to
// Builder.WithNormalized, and every L4 consumer reads from the same
// artefact. There is no second parse.
//
// # Dependency boundary
//
// This package depends only on net/http, the shared normalize package,
// and the vkauth identity type. It does NOT depend on internal/router/,
// internal/handler/, or any policy-plane consumer. The dependency
// direction is one-way: consumers import requestcontext to read; this
// package imports nothing back.
package requestcontext
