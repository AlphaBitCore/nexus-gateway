// gateway.go declares the Gateway capability surface the views render over.
package kit

import (
	"context"
	"github.com/goccy/go-json"
	"net/url"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// Gateway is the capability surface the v1 views need. *core.Client satisfies
// it; tests inject a fake so Update/View logic runs without a network.
type Gateway interface {
	// Overview + radar + event (read).
	Sparkline(ctx context.Context, q url.Values) (*core.SparklineResult, error)
	Instances(ctx context.Context) (*core.InstancesResult, error)
	DLQ(ctx context.Context) (*core.DLQResult, error)
	TrafficList(ctx context.Context, f core.TrafficFilter) (*core.TrafficList, error)
	TrafficEvent(ctx context.Context, id string) (*core.TrafficEvent, error)
	TrafficEventNormalized(ctx context.Context, id string) (json.RawMessage, error)

	// Performance/SLO + Cost (read).
	LatencyPhases(ctx context.Context, groupBy string, q url.Values) (*core.LatencyPhasesResult, error)
	RoutingFallbacks(ctx context.Context, q url.Values) (*core.FallbacksResult, error)
	ProviderDetail(ctx context.Context, providerID string, q url.Values) (*core.ProviderDetail, error)
	Cost(ctx context.Context, q url.Values) (*core.CostReport, error)
	CacheROI(ctx context.Context, q url.Values) (*core.CacheROIResult, error)

	// Wizard catalogs (read).
	AdminModels(ctx context.Context) (*core.ModelCatalog, error)
	VirtualKeys(ctx context.Context) ([]core.VirtualKey, error)
	Providers(ctx context.Context) (*core.ProvidersResult, error)

	// Fleet + alerting (read).
	Alerts(ctx context.Context) (*core.AlertsResult, error)
	Nodes(ctx context.Context) (*core.NodesResult, error)

	// Wave 2 depth (read).
	ByProvider(ctx context.Context, q url.Values) (*core.ByProviderResult, error)
	ComplianceOverview(ctx context.Context, q url.Values) (*core.ComplianceOverview, error)
	Jobs(ctx context.Context) (*core.JobsResult, error)
	ConfigSyncOutOfSync(ctx context.Context) (*core.ConfigSyncResult, error)

	// Routing rules (read; toggle is a write below).
	RoutingRules(ctx context.Context) ([]core.RoutingRule, error)

	// Generic admin request — backs the /resource cascade's direct, catalog-driven
	// calls to any admin kind (read path here; writes go through the confirm gate).
	AdminRequest(ctx context.Context, method, path string, query url.Values, body any) (json.RawMessage, int, error)

	// Chat Playground + Request Lab.
	ChatStream(ctx context.Context, vkSecret string, req core.ChatRequest, onDelta func(string)) (*core.ChatUsage, error)
	SimulatorForward(ctx context.Context, req core.SimulatorForwardRequest) (json.RawMessage, error)
	RoutingSimulate(ctx context.Context, req core.RoutingSimulateRequest) (*core.RoutingSimulateResult, error)

	// Emergency controls (read): current kill-switch state + passthrough snapshot.
	KillSwitchStatus(ctx context.Context) (*core.KillSwitchState, error)
	PassthroughSnapshot(ctx context.Context) (*core.PassthroughSnapshot, error)

	// Mitigation (write; gated by prod confirmation in the view).
	SetKillSwitch(ctx context.Context, engaged bool) (*core.KillSwitchResult, error)
	SetPassthroughGlobal(ctx context.Context, req core.PassthroughGlobalRequest) error
	SetProviderEnabled(ctx context.Context, providerID string, enabled bool) error
	CacheFlush(ctx context.Context) error
	RevokeVK(ctx context.Context, id string) error
	RegenerateVK(ctx context.Context, id string) (*core.RegeneratedVK, error)
	SetRoutingRuleEnabled(ctx context.Context, id string, enabled bool) error
}

// Compile-time assertion that the real client is a Gateway.
var _ Gateway = (*core.Client)(nil)
