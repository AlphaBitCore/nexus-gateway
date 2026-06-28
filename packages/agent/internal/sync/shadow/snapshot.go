package shadow

import (
	"github.com/goccy/go-json"
)

// InterceptionDomainDTO is the wire format for interception domains from the
// Dashboard Backend. Includes nested paths. Converted to configtypes at the
// consumer level via ToDomainPolicy.
type InterceptionDomainDTO struct {
	ID                string                `json:"id"`
	Name              string                `json:"name"`
	HostPattern       string                `json:"hostPattern"`
	HostMatchType     string                `json:"hostMatchType"`
	AdapterID         string                `json:"adapterId"`
	AdapterConfig     json.RawMessage       `json:"adapterConfig,omitempty"`
	Enabled           bool                  `json:"enabled"`
	Priority          int                   `json:"priority"`
	DefaultPathAction string                `json:"defaultPathAction"`
	OnAdapterError    string                `json:"onAdapterError"`
	NetworkZone       string                `json:"networkZone"`
	Paths             []InterceptionPathDTO `json:"paths"`

	// Per-host StreamingPolicy + payload-capture overrides. NULL on any
	// field means "inherit from the global default" — see
	// shared/streaming/policy.Resolve and shared/payloadcapture.Store.
	// Hub's catb_agent_interception_domains loader populates these from
	// the snake_case DB columns; the agent's converter
	// (shadow.ToDomainPolicy) maps them onto
	// shared/domainpolicy.InterceptionDomain so shared/tlsbump's
	// forward_handler reads the same per-host overrides cp does.
	StreamingMode           *string `json:"streamingMode,omitempty"`
	StreamingChunkBytes     *int    `json:"streamingChunkBytes,omitempty"`
	StreamingHookTimeoutMs  *int    `json:"streamingHookTimeoutMs,omitempty"`
	StreamingMaxBufferBytes *int    `json:"streamingMaxBufferBytes,omitempty"`
	StreamingFailBehavior   *string `json:"streamingFailBehavior,omitempty"`
	CaptureRequestBody      *bool   `json:"captureRequestBody,omitempty"`
	CaptureResponseBody     *bool   `json:"captureResponseBody,omitempty"`
	RawBodySpillEnabled     *bool   `json:"rawBodySpillEnabled,omitempty"`
}

// InterceptionPathDTO is the wire format for interception paths.
type InterceptionPathDTO struct {
	ID          string   `json:"id"`
	PathPattern []string `json:"pathPattern"`
	MatchType   string   `json:"matchType"`
	Action      string   `json:"action"`
	Priority    int      `json:"priority"`
	Enabled     bool     `json:"enabled"`
}
