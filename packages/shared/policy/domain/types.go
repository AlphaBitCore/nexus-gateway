// Package domain holds the compliance-proxy's runtime view of
// interception_domain + interception_path rows. The Engine owns the
// priority-ordered host matcher and per-domain path action lookup;
// listener / forward handler consult it to decide whether a request is
// processed, passed through, or denied at request time.
package domain

import "time"

// PathAction is the runtime decision for a request after host + path
// match. Mirrors the InterceptionPath.action enum in the DB schema
// plus the host-level default carried in
// InterceptionDomain.default_path_action.
type PathAction string

const (
	PathActionProcess     PathAction = "PROCESS"
	PathActionPassthrough PathAction = "PASSTHROUGH"
	// PathActionBlock causes the forward handler to reject the request with
	// a 4xx before any compliance hook runs. Mirrors the DB enum value.
	PathActionBlock PathAction = "BLOCK"
)

// HostMatchType mirrors the InterceptionDomain.host_match_type enum.
type HostMatchType string

const (
	HostMatchExact  HostMatchType = "EXACT"
	HostMatchGlob   HostMatchType = "GLOB"
	HostMatchPrefix HostMatchType = "PREFIX"
	HostMatchRegex  HostMatchType = "REGEX"
)

// PathMatchType mirrors the InterceptionPath.match_type enum.
type PathMatchType string

const (
	PathMatchPrefix PathMatchType = "PREFIX"
	PathMatchExact  PathMatchType = "EXACT"
	PathMatchRegex  PathMatchType = "REGEX"
)

// NetworkZone classifies an interception target. Carried through to
// audit events so compliance reporting can filter by zone.
type NetworkZone string

const (
	ZonePublic   NetworkZone = "PUBLIC"
	ZoneInternal NetworkZone = "INTERNAL"
)

// AdapterErrorBehavior mirrors InterceptionDomain.on_adapter_error.
type AdapterErrorBehavior string

const (
	AdapterErrorFailOpen   AdapterErrorBehavior = "FAIL_OPEN"
	AdapterErrorFailClosed AdapterErrorBehavior = "FAIL_CLOSED"
)

// InterceptionDomain is the compliance-proxy in-memory view of an
// interception_domain row plus its joined interception_path rows.
//
// The Streaming* / Capture* / RawBodySpillEnabled fields are per-host
// StreamingPolicy overrides. A nil value means "inherit from the global
// default in system_metadata['streaming_compliance.config']" — see
// shared/transport/streaming/policy.Resolve.
type InterceptionDomain struct {
	ID                      string               `json:"id"`
	Name                    string               `json:"name"`
	HostPattern             string               `json:"hostPattern"`
	HostMatchType           HostMatchType        `json:"hostMatchType"`
	AdapterID               string               `json:"adapterId"`
	NetworkZone             NetworkZone          `json:"networkZone"`
	DefaultPathAction       PathAction           `json:"defaultPathAction"`
	OnAdapterError          AdapterErrorBehavior `json:"onAdapterError"`
	Enabled                 bool                 `json:"enabled"`
	Priority                int                  `json:"priority"`
	Paths                   []InterceptionPath   `json:"paths"`
	UpdatedAt               time.Time            `json:"updatedAt"`
	StreamingMode           *string              `json:"streamingMode,omitempty"`
	StreamingChunkBytes     *int                 `json:"streamingChunkBytes,omitempty"`
	StreamingHookTimeoutMs  *int                 `json:"streamingHookTimeoutMs,omitempty"`
	StreamingMaxBufferBytes *int                 `json:"streamingMaxBufferBytes,omitempty"`
	StreamingFailBehavior   *string              `json:"streamingFailBehavior,omitempty"`
	CaptureRequestBody      *bool                `json:"captureRequestBody,omitempty"`
	CaptureResponseBody     *bool                `json:"captureResponseBody,omitempty"`
	RawBodySpillEnabled     *bool                `json:"rawBodySpillEnabled,omitempty"`
}

// InterceptionPath represents a single `interception_path` row.
// PathPattern is a slice because the DB column is a text[] of
// alternative patterns that all map to the same action; the engine
// matches against any of them.
type InterceptionPath struct {
	ID          string        `json:"id"`
	PathPattern []string      `json:"pathPattern"`
	MatchType   PathMatchType `json:"matchType"`
	Action      PathAction    `json:"action"`
}
