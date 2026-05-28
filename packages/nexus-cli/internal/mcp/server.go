package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

// Options configures which tool tiers are exposed and the VK the simulate tool
// forwards under.
type Options struct {
	// EnableMitigate registers the write (mitigate) tools. Off by default —
	// destructive actions are opt-in per deployment.
	EnableMitigate bool
	// VKSecret is the Virtual Key the simulate tool forwards under. When empty,
	// the simulate tool reports it is unconfigured rather than failing opaquely.
	VKSecret string
}

// NewServer builds the MCP server over gw. The observe / analyze / simulate
// tiers are always registered; the mitigate tier is registered only when
// opts.EnableMitigate is set.
func NewServer(gw Gateway, opts Options) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "nexus", Title: "Nexus Operator Toolkit", Version: "v1"}, nil)
	addObserveTools(s, gw)
	addAnalyzeTools(s, gw)
	addSimulateTools(s, gw, opts.VKSecret)
	if opts.EnableMitigate {
		addMitigateTools(s, gw)
	}
	return s
}

// --- tool argument shapes (schema inferred from these structs) ---

type noArgs struct{}

type trafficListArgs struct {
	Status string `json:"status,omitempty" jsonschema:"filter by status range: 4xx, 5xx, or error"`
	Model  string `json:"model,omitempty" jsonschema:"filter by model slug"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max rows to return (default 20)"`
}

type trafficGetArgs struct {
	ID string `json:"id" jsonschema:"the traffic event id"`
}

type costArgs struct {
	GroupBy string `json:"groupBy,omitempty" jsonschema:"group by provider, user, model, or device (default provider)"`
}

type simulateArgs struct {
	Model  string `json:"model" jsonschema:"model slug to send the request to"`
	Prompt string `json:"prompt" jsonschema:"the user prompt to run through the pipeline"`
}

type killSwitchArgs struct {
	Engage bool `json:"engage" jsonschema:"true engages the global kill switch, false disengages it"`
}

type routeExplainArgs struct {
	Model        string `json:"model" jsonschema:"model slug to dry-run routing for"`
	EndpointType string `json:"endpointType,omitempty" jsonschema:"endpoint type: chat, embedding, etc. (default chat)"`
}

type providerMitigateArgs struct {
	Provider string `json:"provider" jsonschema:"the provider name or display name (resolved to its id)"`
	Enabled  bool   `json:"enabled" jsonschema:"true enables the provider, false disables it"`
}

type ruleMitigateArgs struct {
	Rule    string `json:"rule" jsonschema:"the routing-rule name (resolved to its id)"`
	Enabled bool   `json:"enabled" jsonschema:"true enables the rule, false disables it"`
}

type vkRevokeArgs struct {
	VK string `json:"vk" jsonschema:"the virtual key name, key prefix, or id to revoke (must be active)"`
}

type passthroughGlobalArgs struct {
	Enabled         bool   `json:"enabled" jsonschema:"true engages global emergency passthrough, false disengages it"`
	BypassCache     bool   `json:"bypassCache,omitempty" jsonschema:"also bypass the response cache (engaging bypasses the compliance hooks by default)"`
	BypassNormalize bool   `json:"bypassNormalize,omitempty" jsonschema:"also bypass normalization"`
	Reason          string `json:"reason,omitempty" jsonschema:"reason recorded with the change"`
}

// --- observe tier (read) ---

func addObserveTools(s *mcp.Server, gw Gateway) {
	mcp.AddTool(s, &mcp.Tool{Name: "observe_health", Description: "Gateway health: window totals (requests, cost, tokens, cache hits, errors) plus service/node counts."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
			sp, err := gw.Sparkline(ctx, nil)
			if err != nil {
				return nil, nil, err
			}
			inst, err := gw.Instances(ctx)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(map[string]any{"totals": sp.Totals(), "nodes": inst.Count, "services": inst.Services})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "observe_traffic_list", Description: "List recent traffic events, optionally filtered by status range or model."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a trafficListArgs) (*mcp.CallToolResult, any, error) {
			limit := a.Limit
			if limit <= 0 {
				limit = 20
			}
			list, err := gw.TrafficList(ctx, core.TrafficFilter{StatusRange: a.Status, ModelUsed: a.Model, Limit: limit})
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(list)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "observe_traffic_event", Description: "Fetch one traffic event by id (status, model, tokens, cost, trace id, latency phases)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a trafficGetArgs) (*mcp.CallToolResult, any, error) {
			if a.ID == "" {
				return nil, nil, fmt.Errorf("id is required")
			}
			ev, err := gw.TrafficEvent(ctx, a.ID)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(ev)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "observe_models", Description: "The configured model catalog grouped by provider."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
			cat, err := gw.AdminModels(ctx)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(cat)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "observe_alerts", Description: "The alerts firing right now (what's burning) — name, severity, and when each started firing."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
			al, err := gw.Alerts(ctx)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(al)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "observe_nodes", Description: "Every registered node: heartbeat, version, online state, and whether its applied config has drifted from target."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
			n, err := gw.Nodes(ctx)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(n)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "observe_killswitch", Description: "Current global kill-switch state (engaged or not, version, who last toggled it). The kill switch halts TLS bumping on every node."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
			st, err := gw.KillSwitchStatus(ctx)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(st)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "observe_passthrough", Description: "Emergency-passthrough snapshot: the three tiers (global / per-adapter / per-provider) and what each is bypassing (hooks/cache/normalize) — the 'is anything slipping past compliance right now' signal."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
			snap, err := gw.PassthroughSnapshot(ctx)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(snap)
		})
}

// --- analyze tier (read summaries) ---

func addAnalyzeTools(s *mcp.Server, gw Gateway) {
	mcp.AddTool(s, &mcp.Tool{Name: "analyze_cost", Description: "Cost grouped by provider/user/model/device over the recent window."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a costArgs) (*mcp.CallToolResult, any, error) {
			groupBy := a.GroupBy
			if groupBy == "" {
				groupBy = "provider"
			}
			rep, err := gw.Cost(ctx, url.Values{"groupBy": {groupBy}})
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(rep)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "analyze_slo", Description: "Provider SLO: overall availability, per-provider latency percentiles, and routing-fallback activity."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
			win := weekWindow()
			lp, err := gw.LatencyPhases(ctx, "provider", win)
			if err != nil {
				return nil, nil, err
			}
			fb, err := gw.RoutingFallbacks(ctx, win)
			if err != nil {
				return nil, nil, err
			}
			sp, err := gw.Sparkline(ctx, nil)
			if err != nil {
				return nil, nil, err
			}
			tot := sp.Totals()
			reqs := tot[core.MetricRequestCount]
			errs := tot[core.MetricStatus4xxCount] + tot[core.MetricStatus5xxCount]
			avail := 100.0
			if reqs > 0 {
				avail = 100 - errs/reqs*100
			}
			return jsonResult(map[string]any{"availabilityPct": avail, "requests": reqs, "errors": errs, "providers": lp.Rows, "fallbacks": fb.Data})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "analyze_compliance", Description: "Compliance overview: total requests, blocked count, overall block rate, and the governance KPIs over the recent window."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
			ov, err := gw.ComplianceOverview(ctx, weekWindow())
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(ov)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "route_explain", Description: "Routing dry-run (why this route): which provider/model a request for the given model would resolve to, plus any warnings. Fires no real request and changes no state."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a routeExplainArgs) (*mcp.CallToolResult, any, error) {
			if a.Model == "" {
				return nil, nil, fmt.Errorf("model is required")
			}
			ep := a.EndpointType
			if ep == "" {
				ep = "chat"
			}
			res, err := gw.RoutingSimulate(ctx, core.RoutingSimulateRequest{ModelID: a.Model, EndpointType: ep})
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(res)
		})
}

// --- simulate tier (no production mutation) ---

func addSimulateTools(s *mcp.Server, gw Gateway, vkSecret string) {
	mcp.AddTool(s, &mcp.Tool{Name: "simulate_request", Description: "Run a crafted chat request through the real gateway pipeline (request lab) and return the full response. No production state changes."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a simulateArgs) (*mcp.CallToolResult, any, error) {
			if vkSecret == "" {
				return nil, nil, fmt.Errorf("the simulate tool has no Virtual Key configured for this deployment")
			}
			if a.Model == "" {
				return nil, nil, fmt.Errorf("model is required")
			}
			body, err := json.Marshal(map[string]any{
				"model":      a.Model,
				"messages":   []map[string]string{{"role": "user", "content": a.Prompt}},
				"max_tokens": 256,
			})
			if err != nil {
				return nil, nil, err
			}
			raw, err := gw.SimulatorForward(ctx, core.SimulatorForwardRequest{Path: "/v1/chat/completions", Method: "POST", VK: vkSecret, Body: body})
			if err != nil {
				return nil, nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, nil, nil
		})
}

// --- mitigate tier (write; opt-in, off by default) ---

func addMitigateTools(s *mcp.Server, gw Gateway) {
	mcp.AddTool(s, &mcp.Tool{Name: "mitigate_kill_switch", Description: "Engage or disengage the global kill switch (halts TLS bumping fleet-wide). IAM-gated and audited server-side. Off by default — only present when the deployment enables mitigate tools."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a killSwitchArgs) (*mcp.CallToolResult, any, error) {
			res, err := gw.SetKillSwitch(ctx, a.Engage)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(res)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "mitigate_cache_flush", Description: "Flush the gateway's cached config so the next request re-reads fresh state. IAM-gated and audited server-side."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
			if err := gw.CacheFlush(ctx); err != nil {
				return nil, nil, err
			}
			return jsonResult(map[string]any{"flushed": true})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "mitigate_provider_enabled", Description: "Enable or disable a provider by name (resolved to its id). Disabling takes a misbehaving provider out of routing. IAM-gated and audited server-side."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a providerMitigateArgs) (*mcp.CallToolResult, any, error) {
			id, label, err := resolveProviderID(ctx, gw, a.Provider)
			if err != nil {
				return nil, nil, err
			}
			if err := gw.SetProviderEnabled(ctx, id, a.Enabled); err != nil {
				return nil, nil, err
			}
			return jsonResult(map[string]any{"provider": label, "enabled": a.Enabled})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "mitigate_routing_rule_enabled", Description: "Enable or disable a routing rule by name (resolved to its id) — a way to take a misbehaving rule out of the path. IAM-gated and audited server-side."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a ruleMitigateArgs) (*mcp.CallToolResult, any, error) {
			id, label, err := resolveRuleID(ctx, gw, a.Rule)
			if err != nil {
				return nil, nil, err
			}
			if err := gw.SetRoutingRuleEnabled(ctx, id, a.Enabled); err != nil {
				return nil, nil, err
			}
			return jsonResult(map[string]any{"rule": label, "enabled": a.Enabled})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "mitigate_vk_revoke", Description: "Revoke an active Virtual Key by name, key prefix, or id (resolved to its id). Irreversible. IAM-gated and audited server-side."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a vkRevokeArgs) (*mcp.CallToolResult, any, error) {
			id, label, err := resolveRevocableVK(ctx, gw, a.VK)
			if err != nil {
				return nil, nil, err
			}
			if err := gw.RevokeVK(ctx, id); err != nil {
				return nil, nil, err
			}
			return jsonResult(map[string]any{"revoked": label})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "mitigate_passthrough_global", Description: "Engage or disengage the global emergency-passthrough tier. Engaging bypasses the compliance hooks fleet-wide (the canonical 'let traffic through'); add bypassCache/bypassNormalize for the other tiers. IAM-gated and audited server-side."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a passthroughGlobalArgs) (*mcp.CallToolResult, any, error) {
			req := core.PassthroughGlobalRequest{Enabled: a.Enabled, Reason: a.Reason}
			if a.Enabled {
				// Bypassing the hooks is the canonical emergency; the server rejects an
				// enabled tier that bypasses nothing, so default hooks on.
				req.BypassHooks = true
				req.BypassCache = a.BypassCache
				req.BypassNormalize = a.BypassNormalize
			}
			if err := gw.SetPassthroughGlobal(ctx, req); err != nil {
				return nil, nil, err
			}
			return jsonResult(map[string]any{"enabled": a.Enabled})
		})
}

// jsonResult renders v as indented JSON text content and returns it as the
// structured output too.
func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, v, nil
}

// weekWindow is the [7d ago, now] window the analytics endpoints require.
func weekWindow() url.Values {
	now := time.Now().UTC()
	return url.Values{
		"start": {now.AddDate(0, 0, -7).Format(time.RFC3339)},
		"end":   {now.Format(time.RFC3339)},
	}
}

// Serve runs the server over stdio until the context is cancelled or the client
// disconnects.
func Serve(ctx context.Context, gw Gateway, opts Options) error {
	return NewServer(gw, opts).Run(ctx, &mcp.StdioTransport{})
}
