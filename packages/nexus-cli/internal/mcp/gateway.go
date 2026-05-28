// Package mcp exposes the toolkit's capabilities as Model Context Protocol
// tools over stdio, so agents and partner platforms drive the gateway through
// the same admin API + IAM as every other caller. The server has no auth of its
// own: it is configured with an admin credential (a scoped service user's API
// key), and every tool invocation executes as that principal — IAM denials come
// back from the admin API as tool errors, never bypassed here.
package mcp

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

// Gateway is the capability surface the MCP tools call. *core.Client satisfies
// it; tests inject a fake so tool wiring + IAM gating run without a network.
type Gateway interface {
	Sparkline(ctx context.Context, q url.Values) (*core.SparklineResult, error)
	Instances(ctx context.Context) (*core.InstancesResult, error)
	TrafficList(ctx context.Context, f core.TrafficFilter) (*core.TrafficList, error)
	TrafficEvent(ctx context.Context, id string) (*core.TrafficEvent, error)
	AdminModels(ctx context.Context) (*core.ModelCatalog, error)
	Cost(ctx context.Context, q url.Values) (*core.CostReport, error)
	LatencyPhases(ctx context.Context, groupBy string, q url.Values) (*core.LatencyPhasesResult, error)
	RoutingFallbacks(ctx context.Context, q url.Values) (*core.FallbacksResult, error)
	SimulatorForward(ctx context.Context, req core.SimulatorForwardRequest) (json.RawMessage, error)

	// Read tiers added for the observe/analyze tools.
	Alerts(ctx context.Context) (*core.AlertsResult, error)
	Nodes(ctx context.Context) (*core.NodesResult, error)
	ComplianceOverview(ctx context.Context, q url.Values) (*core.ComplianceOverview, error)
	RoutingSimulate(ctx context.Context, req core.RoutingSimulateRequest) (*core.RoutingSimulateResult, error)
	KillSwitchStatus(ctx context.Context) (*core.KillSwitchState, error)
	PassthroughSnapshot(ctx context.Context) (*core.PassthroughSnapshot, error)

	// Catalogs the mitigate tools resolve a human-friendly name/prefix against
	// before issuing a write (no bare id ever crosses the tool boundary).
	Providers(ctx context.Context) (*core.ProvidersResult, error)
	RoutingRules(ctx context.Context) ([]core.RoutingRule, error)
	VirtualKeys(ctx context.Context) ([]core.VirtualKey, error)

	// Mitigate tier (write; opt-in, IAM-gated, audited server-side).
	SetKillSwitch(ctx context.Context, engaged bool) (*core.KillSwitchResult, error)
	SetProviderEnabled(ctx context.Context, providerID string, enabled bool) error
	CacheFlush(ctx context.Context) error
	RevokeVK(ctx context.Context, id string) error
	SetRoutingRuleEnabled(ctx context.Context, id string, enabled bool) error
	SetPassthroughGlobal(ctx context.Context, req core.PassthroughGlobalRequest) error
}

// Compile-time assertion that the real client is a Gateway.
var _ Gateway = (*core.Client)(nil)
