// Package configsync polls the Dashboard Backend for configuration updates
// and delivers typed config snapshots to the agent's hook pipeline and
// traffic interception layer.
package shadow

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// ConfigSnapshot holds an aggregated cache of Cat B config the agent
// has applied. It is used as the offline-fallback persistence shape
// (saved to local SQLCipher via OfflineFallback.SaveSnapshot /
// LoadCached, which round-trip through audit/queue.Queue.SaveConfigSnapshot
// / LoadLatestConfigSnapshot). It is NOT the live-pull wire format —
// the agent fetches Cat B keys per-key via GET
// /api/internal/things/config/<configKey>?type=agent (see
// cmd/agent/configdispatch.go agentPullConfig); this struct is the
// post-merge in-memory shape the offline path serializes.
type ConfigSnapshot struct {
	Version             int                     `json:"configVersion"`
	HookConfigs         []core.HookConfig       `json:"hookConfigs"`
	InterceptionDomains []InterceptionDomainDTO `json:"interceptionDomains"`
	FetchedAt           time.Time               `json:"fetchedAt"`
}

// InterceptionDomainDTO is the wire format for interception domains from the
// Dashboard Backend. Includes nested paths. Converted to configtypes at the
// consumer level.
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
	// (shadow.ConfigSnapshot.ToDomainPolicy) maps them onto
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

// ParseSnapshot parses the raw JSON config response from the Dashboard Backend
// into a typed ConfigSnapshot.
func ParseSnapshot(data map[string]any) (*ConfigSnapshot, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("configsync: marshal config: %w", err)
	}
	var snap ConfigSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("configsync: unmarshal config: %w", err)
	}
	snap.FetchedAt = time.Now()
	return &snap, nil
}
