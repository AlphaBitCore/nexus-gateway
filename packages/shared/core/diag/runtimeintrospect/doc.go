// Package runtimeintrospect lets a service expose its live in-memory
// configuration and cache state over HTTP for diagnostic purposes.
//
// Construct one Registry per process, register one Source per logical
// state area (config_keys, cache categories, runtime structs), then
// mount Registry.Handler on the service's admin/metrics port.
//
// Every Source implementation MUST redact secret material — API keys,
// provider credentials, OAuth tokens, mTLS private keys, session
// cookies, raw JWT material — before returning its snapshot. PR review
// is the authoritative check; the package only provides the carrier.
package runtimeintrospect
