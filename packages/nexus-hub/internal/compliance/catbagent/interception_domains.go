package catbagent

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"time"
)

// AgentInterceptionDomainsLoader aggregates enabled InterceptionDomain
// rows plus their enabled paths. Shape returned by Load matches
// AgentPipeline.ApplyDomainsShadowState:
//
//	{"interceptionDomains": [ {...domain fields..., "paths": [...] }, ... ]}
//
// Wire JSON tag names mirror
// packages/agent/internal/sync/shadow/snapshot.go::InterceptionDomainDTO.
// No per-agent scoping yet.
type AgentInterceptionDomainsLoader struct {
	db     pgxQuerier
	logger *slog.Logger
}

// NewAgentInterceptionDomainsLoader constructs a loader bound to the
// given pool.
func NewAgentInterceptionDomainsLoader(db pgxQuerier, logger *slog.Logger) *AgentInterceptionDomainsLoader {
	return &AgentInterceptionDomainsLoader{db: db, logger: logger}
}

// Field tags below duplicate InterceptionDomainDTO / InterceptionPathDTO
// to keep the wire contract at the SQL scan site. The SELECT columns
// are copied (not imported) from
// packages/control-plane/internal/store/interception_domain.go's
// ListEnabledInterceptionDomains to avoid a cp -> hub dependency cycle.
// Any schema change to interception_domain / interception_path must
// update both sites.
type agentInterceptionDomainRow struct {
	ID                string                     `json:"id"`
	Name              string                     `json:"name"`
	HostPattern       string                     `json:"hostPattern"`
	HostMatchType     string                     `json:"hostMatchType"`
	AdapterID         string                     `json:"adapterId"`
	AdapterConfig     json.RawMessage            `json:"adapterConfig,omitempty"`
	Enabled           bool                       `json:"enabled"`
	Priority          int                        `json:"priority"`
	DefaultPathAction string                     `json:"defaultPathAction"`
	OnAdapterError    string                     `json:"onAdapterError"`
	NetworkZone       string                     `json:"networkZone"`
	Paths             []agentInterceptionPathRow `json:"paths"`

	// Per-host StreamingPolicy + payload-capture overrides. NULL columns
	// serialize to omitted JSON keys; the agent's domainpolicy engine +
	// tlsbump resolver fall back to the global defaults when these aren't
	// present. Nullable shape mirrors cp's domainpolicy.InterceptionDomain
	// so both ingresses see the same per-host knobs identically.
	StreamingMode           *string `json:"streamingMode,omitempty"`
	StreamingChunkBytes     *int    `json:"streamingChunkBytes,omitempty"`
	StreamingHookTimeoutMs  *int    `json:"streamingHookTimeoutMs,omitempty"`
	StreamingMaxBufferBytes *int    `json:"streamingMaxBufferBytes,omitempty"`
	StreamingFailBehavior   *string `json:"streamingFailBehavior,omitempty"`
	CaptureRequestBody      *bool   `json:"captureRequestBody,omitempty"`
	CaptureResponseBody     *bool   `json:"captureResponseBody,omitempty"`
	RawBodySpillEnabled     *bool   `json:"rawBodySpillEnabled,omitempty"`
}

type agentInterceptionPathRow struct {
	ID          string   `json:"id"`
	PathPattern []string `json:"pathPattern"`
	MatchType   string   `json:"matchType"`
	Action      string   `json:"action"`
	Priority    int      `json:"priority"`
	Enabled     bool     `json:"enabled"`
}

// Load reads enabled domains then enabled paths and assembles the nested
// envelope. Version is max(updated_at) across both tables so a path
// change bumps the version even when the domain row itself didn't move.
func (l *AgentInterceptionDomainsLoader) Load(ctx context.Context, _ string) (any, int64, error) {
	domainRows, err := l.db.Query(ctx, `
		SELECT id, name, host_pattern, host_match_type, adapter_id,
		       adapter_config, enabled, priority, default_path_action,
		       on_adapter_error, network_zone, updated_at,
		       streaming_mode, streaming_chunk_bytes, streaming_hook_timeout_ms,
		       streaming_max_buffer_bytes, streaming_fail_behavior,
		       capture_request_body, capture_response_body,
		       raw_body_spill_enabled
		FROM interception_domain
		WHERE enabled = true
		ORDER BY priority DESC, name ASC
	`)
	if err != nil {
		return nil, 0, fmt.Errorf("catb: query interception_domain: %w", err)
	}
	defer domainRows.Close()

	var (
		domains    []agentInterceptionDomainRow
		domainIdx  = make(map[string]int)
		maxUpdated time.Time
	)
	for domainRows.Next() {
		var (
			d          agentInterceptionDomainRow
			adapterCfg []byte
			updatedAt  time.Time
		)
		if err := domainRows.Scan(
			&d.ID, &d.Name, &d.HostPattern, &d.HostMatchType, &d.AdapterID,
			&adapterCfg, &d.Enabled, &d.Priority, &d.DefaultPathAction,
			&d.OnAdapterError, &d.NetworkZone, &updatedAt,
			&d.StreamingMode, &d.StreamingChunkBytes, &d.StreamingHookTimeoutMs,
			&d.StreamingMaxBufferBytes, &d.StreamingFailBehavior,
			&d.CaptureRequestBody, &d.CaptureResponseBody,
			&d.RawBodySpillEnabled,
		); err != nil {
			return nil, 0, fmt.Errorf("catb: scan interception_domain: %w", err)
		}
		if len(adapterCfg) > 0 {
			d.AdapterConfig = json.RawMessage(adapterCfg)
		}
		d.Paths = []agentInterceptionPathRow{}

		domainIdx[d.ID] = len(domains)
		domains = append(domains, d)
		if updatedAt.After(maxUpdated) {
			maxUpdated = updatedAt
		}
	}
	if err := domainRows.Err(); err != nil {
		return nil, 0, fmt.Errorf("catb: iterate interception_domain: %w", err)
	}

	if len(domains) == 0 {
		return map[string]any{"interceptionDomains": []agentInterceptionDomainRow{}}, 0, nil
	}

	pathRows, err := l.db.Query(ctx, `
		SELECT id, domain_id, path_pattern, match_type, action,
		       priority, enabled, updated_at
		FROM interception_path
		WHERE enabled = true
		ORDER BY priority DESC, created_at ASC
	`)
	if err != nil {
		return nil, 0, fmt.Errorf("catb: query interception_path: %w", err)
	}
	defer pathRows.Close()

	for pathRows.Next() {
		var (
			p         agentInterceptionPathRow
			domainID  string
			updatedAt time.Time
		)
		if err := pathRows.Scan(
			&p.ID, &domainID, &p.PathPattern, &p.MatchType, &p.Action,
			&p.Priority, &p.Enabled, &updatedAt,
		); err != nil {
			return nil, 0, fmt.Errorf("catb: scan interception_path: %w", err)
		}
		if idx, ok := domainIdx[domainID]; ok {
			domains[idx].Paths = append(domains[idx].Paths, p)
		}
		if updatedAt.After(maxUpdated) {
			maxUpdated = updatedAt
		}
	}
	if err := pathRows.Err(); err != nil {
		return nil, 0, fmt.Errorf("catb: iterate interception_path: %w", err)
	}

	state := map[string]any{"interceptionDomains": domains}
	return state, timestampVersion(maxUpdated), nil
}
