package runtime

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// gatewayTools builds the gateway capability tools: the observe + analyze + simulate
// reads, then the generic resource tools (and, when includeMitigate, the confirm-tier
// writes). vkSecret is the VK the simulate tool forwards under (empty => simulate
// reports unconfigured). The shared helpers live in result.go (jsonResult/errResult)
// and window.go (the time-range argument).
func gatewayTools(gw Gateway, vkSecret string, includeMitigate bool) []agent.Tool {
	tools := observeTools(gw)
	tools = append(tools, analyzeTools(gw)...)
	tools = append(tools, simulateTools(gw, vkSecret)...)
	// Generic resource tools: the long-tail layer covering every CP admin kind via
	// the embedded OpenAPI catalog. Reads (discovery/list/get) always; writes
	// (create/update/delete/action, confirm-tier) alongside the curated mitigates.
	tools = append(tools, resourceReadTools(gw)...)
	if includeMitigate {
		tools = append(tools, mitigateTools(gw)...)
		tools = append(tools, resourceWriteTools(gw)...)
	}
	return tools
}

// observeTools are the auto-tier reads of live gateway state — health totals, the
// individual recent requests, a single event, the model catalog, firing alerts,
// nodes, and the two emergency-control snapshots.
func observeTools(gw Gateway) []agent.Tool {
	return []agent.Tool{
		&funcTool{name: "observe_health", tier: agent.TierAuto,
			schema: json.RawMessage(`{"type":"object","properties":{` + windowSchemaProp + `}}`),
			desc:   "At-a-glance gateway health totals over a time window — requests, cost, tokens, cache hits, errors — plus live node/service counts. Window defaults to the last 7 days; pass window=today|24h|1h for a recent total. Use for a health/volume summary; NOT for listing or inspecting individual requests (use observe_traffic_list).",
			run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				sp, err := gw.Sparkline(ctx, windowValues(windowArg(in)))
				if err != nil {
					return errResult("health unavailable: %s", err), nil
				}
				inst, err := gw.Instances(ctx)
				if err != nil {
					return errResult("instances unavailable: %s", err), nil
				}
				return jsonResult(map[string]any{"totals": sp.Totals(), "nodes": inst.Count, "services": inst.Services})
			}},

		&funcTool{name: "observe_traffic_list", tier: agent.TierAuto,
			schema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","description":"filter: 4xx, 5xx, or error"},"model":{"type":"string"},"limit":{"type":"integer"},` + windowSchemaProp + `}}`),
			desc:   "The individual recent requests (one row per request, newest first) — filter by status range, model, and time window. Use to inspect OR COUNT recent requests: the result's total is the request count over the window. Window defaults to the last 7 days; pass window=today for today's requests. THIS is the tool for 'how many requests today', not the analytics aggregates.",
			run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Status string `json:"status"`
					Model  string `json:"model"`
					Limit  int    `json:"limit"`
				}
				_ = json.Unmarshal(in, &a)
				if a.Limit <= 0 {
					a.Limit = 20
				}
				start, end := windowRange(windowArg(in))
				list, err := gw.TrafficList(ctx, core.TrafficFilter{StatusRange: a.Status, ModelUsed: a.Model, Limit: a.Limit, StartTime: start, EndTime: end})
				if err != nil {
					return errResult("traffic list failed: %s", err), nil
				}
				return jsonResult(list)
			}},

		&funcTool{name: "observe_traffic_event", tier: agent.TierAuto,
			schema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`),
			desc:   "Fetch one traffic event by id (status, model, tokens, cost, trace id, latency phases).",
			run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(in, &a)
				if a.ID == "" {
					return errResult("id is required"), nil
				}
				ev, err := gw.TrafficEvent(ctx, a.ID)
				if err != nil {
					return errResult("event %s not found: %s", a.ID, err), nil
				}
				return jsonResult(ev)
			}},

		&funcTool{name: "observe_models", tier: agent.TierAuto,
			desc: "The configured model catalog grouped by provider.",
			run: func(ctx context.Context, _ json.RawMessage) (agent.Result, error) {
				cat, err := gw.AdminModels(ctx)
				if err != nil {
					return errResult("models unavailable: %s", err), nil
				}
				return jsonResult(cat)
			}},

		&funcTool{name: "observe_alerts", tier: agent.TierAuto,
			desc: "The alerts firing right now — name, severity, and when each started firing.",
			run: func(ctx context.Context, _ json.RawMessage) (agent.Result, error) {
				al, err := gw.Alerts(ctx)
				if err != nil {
					return errResult("alerts unavailable: %s", err), nil
				}
				return jsonResult(al)
			}},

		&funcTool{name: "observe_nodes", tier: agent.TierAuto,
			desc: "Every registered node: heartbeat, version, online state, and whether its applied config has drifted from target.",
			run: func(ctx context.Context, _ json.RawMessage) (agent.Result, error) {
				n, err := gw.Nodes(ctx)
				if err != nil {
					return errResult("nodes unavailable: %s", err), nil
				}
				return jsonResult(n)
			}},

		&funcTool{name: "observe_killswitch", tier: agent.TierAuto,
			desc: "Current global kill-switch state. The kill switch halts TLS bumping on every node.",
			run: func(ctx context.Context, _ json.RawMessage) (agent.Result, error) {
				st, err := gw.KillSwitchStatus(ctx)
				if err != nil {
					return errResult("kill-switch state unavailable: %s", err), nil
				}
				return jsonResult(st)
			}},

		&funcTool{name: "observe_passthrough", tier: agent.TierAuto,
			desc: "Emergency-passthrough snapshot: the three tiers (global / per-adapter / per-provider) and what each bypasses (hooks/cache/normalize).",
			run: func(ctx context.Context, _ json.RawMessage) (agent.Result, error) {
				snap, err := gw.PassthroughSnapshot(ctx)
				if err != nil {
					return errResult("passthrough state unavailable: %s", err), nil
				}
				return jsonResult(snap)
			}},
	}
}

// analyzeTools are the auto-tier analytics aggregates (cost, SLO, compliance) plus
// the routing dry-run that explains where a model resolves.
func analyzeTools(gw Gateway) []agent.Tool {
	return []agent.Tool{
		&funcTool{name: "analyze_cost", tier: agent.TierAuto,
			schema: json.RawMessage(`{"type":"object","properties":{"groupBy":{"type":"string","description":"provider, user, model, or device"},` + windowSchemaProp + `}}`),
			desc:   "Spend (USD) grouped by provider/user/model/device over a time window. Window defaults to the last 7 days; pass window=today|24h|30d to scope it. Use for money/cost questions; NOT for counting requests (observe_traffic_list) or latency (analyze_slo).",
			run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					GroupBy string `json:"groupBy"`
				}
				_ = json.Unmarshal(in, &a)
				if a.GroupBy == "" {
					a.GroupBy = "provider"
				}
				q := windowValues(windowArg(in))
				q.Set("groupBy", a.GroupBy)
				rep, err := gw.Cost(ctx, q)
				if err != nil {
					return errResult("cost report failed: %s", err), nil
				}
				return jsonResult(rep)
			}},

		&funcTool{name: "analyze_slo", tier: agent.TierAuto,
			schema: json.RawMessage(`{"type":"object","properties":{` + windowSchemaProp + `}}`),
			desc:   "Provider SLO over a time window: overall availability, per-provider latency percentiles, and routing-fallback activity. Window defaults to the last 7 days; pass window=today|24h to scope it. Use for latency/availability/reliability questions; NOT for cost or request counts.",
			run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				win := windowValues(windowArg(in))
				lp, err := gw.LatencyPhases(ctx, "provider", win)
				if err != nil {
					return errResult("latency phases failed: %s", err), nil
				}
				fb, err := gw.RoutingFallbacks(ctx, win)
				if err != nil {
					return errResult("fallbacks failed: %s", err), nil
				}
				sp, err := gw.Sparkline(ctx, win)
				if err != nil {
					return errResult("sparkline failed: %s", err), nil
				}
				tot := sp.Totals()
				reqs := tot[core.MetricRequestCount]
				errs := tot[core.MetricStatus4xxCount] + tot[core.MetricStatus5xxCount]
				avail := 100.0
				if reqs > 0 {
					avail = 100 - errs/reqs*100
				}
				return jsonResult(map[string]any{"availabilityPct": avail, "requests": reqs, "errors": errs, "providers": lp.Rows, "fallbacks": fb.Data})
			}},

		&funcTool{name: "analyze_compliance", tier: agent.TierAuto,
			schema: json.RawMessage(`{"type":"object","properties":{` + windowSchemaProp + `}}`),
			desc:   "Compliance overview over a time window: total requests, blocked count, overall block rate, and governance KPIs. Window defaults to the last 7 days; pass window=today|24h for a recent count. Use for block-rate/governance questions; this total IS the request count for the window.",
			run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				ov, err := gw.ComplianceOverview(ctx, windowValues(windowArg(in)))
				if err != nil {
					return errResult("compliance overview failed: %s", err), nil
				}
				return jsonResult(ov)
			}},

		&funcTool{name: "route_explain", tier: agent.TierAuto,
			schema: json.RawMessage(`{"type":"object","properties":{"model":{"type":"string"},"endpointType":{"type":"string"}},"required":["model"]}`),
			desc:   "Routing dry-run (why this route): which provider/model a request would resolve to, plus warnings. Fires no real request.",
			run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Model        string `json:"model"`
					EndpointType string `json:"endpointType"`
				}
				_ = json.Unmarshal(in, &a)
				if a.Model == "" {
					return errResult("model is required"), nil
				}
				if a.EndpointType == "" {
					a.EndpointType = "chat"
				}
				res, err := gw.RoutingSimulate(ctx, core.RoutingSimulateRequest{ModelID: a.Model, EndpointType: a.EndpointType})
				if err != nil {
					return errResult("route explain failed: %s", err), nil
				}
				return jsonResult(res)
			}},
	}
}

// simulateTools run a crafted chat request through the real gateway pipeline (the
// request lab); vkSecret is the key it forwards under (empty => unconfigured).
func simulateTools(gw Gateway, vkSecret string) []agent.Tool {
	return []agent.Tool{
		&funcTool{name: "simulate_request", tier: agent.TierAuto,
			schema: json.RawMessage(`{"type":"object","properties":{"model":{"type":"string"},"prompt":{"type":"string"}},"required":["model","prompt"]}`),
			desc:   "Run a crafted chat request through the real gateway pipeline (request lab) and return the full response. No production state changes.",
			run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				if vkSecret == "" {
					return errResult("the simulate tool has no Virtual Key configured for this deployment"), nil
				}
				var a struct {
					Model  string `json:"model"`
					Prompt string `json:"prompt"`
				}
				_ = json.Unmarshal(in, &a)
				if a.Model == "" {
					return errResult("model is required"), nil
				}
				body, _ := json.Marshal(map[string]any{
					"model":      a.Model,
					"messages":   []map[string]string{{"role": "user", "content": a.Prompt}},
					"max_tokens": 256,
				})
				raw, err := gw.SimulatorForward(ctx, core.SimulatorForwardRequest{Path: "/v1/chat/completions", Method: "POST", VK: vkSecret, Body: body})
				if err != nil {
					return errResult("simulate failed: %s", err), nil
				}
				return agent.Result{Content: string(raw)}, nil
			}},
	}
}

// enableVerb renders the operator-facing verb for an enable/disable toggle, used
// by the mitigate tools' confirmDetail so the gate names the exact action.
func enableVerb(enabled bool) string {
	if enabled {
		return "enable"
	}
	return "disable"
}

// mitigateTools are the confirm-tier write tools (the agent loop gates them; MCP
// includes them only behind --enable-mitigate).
func mitigateTools(gw Gateway) []agent.Tool {
	return []agent.Tool{
		&funcTool{name: "mitigate_kill_switch", tier: agent.TierConfirm,
			schema: json.RawMessage(`{"type":"object","properties":{"engage":{"type":"boolean"}},"required":["engage"]}`),
			desc:   "Engage or disengage the global kill switch (halts TLS bumping fleet-wide).",
			confirmDetail: func(in json.RawMessage) string {
				var a struct {
					Engage bool `json:"engage"`
				}
				_ = json.Unmarshal(in, &a)
				if a.Engage {
					return "engage the global kill switch (halts TLS bumping fleet-wide)"
				}
				return "disengage the global kill switch"
			},
			impact: func(ctx context.Context, in json.RawMessage) (any, error) {
				var a struct {
					Engage bool `json:"engage"`
				}
				_ = json.Unmarshal(in, &a)
				st, err := gw.KillSwitchStatus(ctx)
				if err != nil {
					return nil, err
				}
				action, summary := "disengage", "Resumes TLS bumping fleet-wide."
				if a.Engage {
					action, summary = "engage", "Halts TLS bumping on every node, fleet-wide, until disengaged."
				}
				return map[string]any{
					"action":  action,
					"summary": summary,
					"current": map[string]any{
						"engaged":       st.Engaged,
						"version":       st.Version,
						"lastChangedAt": st.At,
						"lastChangedBy": st.By,
					},
				}, nil
			},
			run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Engage bool `json:"engage"`
				}
				_ = json.Unmarshal(in, &a)
				res, err := gw.SetKillSwitch(ctx, a.Engage)
				if err != nil {
					return errResult("kill switch write failed: %s", err), nil
				}
				return jsonResult(res)
			}},

		&funcTool{name: "mitigate_cache_flush", tier: agent.TierConfirm,
			desc:          "Flush the gateway's cached config so the next request re-reads fresh state.",
			confirmDetail: func(json.RawMessage) string { return "flush the gateway config cache" },
			run: func(ctx context.Context, _ json.RawMessage) (agent.Result, error) {
				if err := gw.CacheFlush(ctx); err != nil {
					return errResult("cache flush failed: %s", err), nil
				}
				return jsonResult(map[string]any{"flushed": true})
			}},

		&funcTool{name: "mitigate_provider_enabled", tier: agent.TierConfirm,
			schema: json.RawMessage(`{"type":"object","properties":{"provider":{"type":"string"},"enabled":{"type":"boolean"}},"required":["provider","enabled"]}`),
			desc:   "Enable or disable a provider by name (resolved to its id). Disabling takes a misbehaving provider out of routing.",
			confirmDetail: func(in json.RawMessage) string {
				var a struct {
					Provider string `json:"provider"`
					Enabled  bool   `json:"enabled"`
				}
				_ = json.Unmarshal(in, &a)
				return fmt.Sprintf("%s provider %q", enableVerb(a.Enabled), a.Provider)
			},
			run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Provider string `json:"provider"`
					Enabled  bool   `json:"enabled"`
				}
				_ = json.Unmarshal(in, &a)
				id, label, err := resolveProviderID(ctx, gw, a.Provider)
				if err != nil {
					return errResult("%s", err), nil
				}
				if err := gw.SetProviderEnabled(ctx, id, a.Enabled); err != nil {
					return errResult("provider write failed: %s", err), nil
				}
				return jsonResult(map[string]any{"provider": label, "enabled": a.Enabled})
			}},

		&funcTool{name: "mitigate_routing_rule_enabled", tier: agent.TierConfirm,
			schema: json.RawMessage(`{"type":"object","properties":{"rule":{"type":"string"},"enabled":{"type":"boolean"}},"required":["rule","enabled"]}`),
			desc:   "Enable or disable a routing rule by name (resolved to its id).",
			confirmDetail: func(in json.RawMessage) string {
				var a struct {
					Rule    string `json:"rule"`
					Enabled bool   `json:"enabled"`
				}
				_ = json.Unmarshal(in, &a)
				return fmt.Sprintf("%s routing rule %q", enableVerb(a.Enabled), a.Rule)
			},
			run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Rule    string `json:"rule"`
					Enabled bool   `json:"enabled"`
				}
				_ = json.Unmarshal(in, &a)
				id, label, err := resolveRuleID(ctx, gw, a.Rule)
				if err != nil {
					return errResult("%s", err), nil
				}
				if err := gw.SetRoutingRuleEnabled(ctx, id, a.Enabled); err != nil {
					return errResult("routing rule write failed: %s", err), nil
				}
				return jsonResult(map[string]any{"rule": label, "enabled": a.Enabled})
			}},

		&funcTool{name: "mitigate_vk_revoke", tier: agent.TierConfirm,
			schema: json.RawMessage(`{"type":"object","properties":{"vk":{"type":"string"}},"required":["vk"]}`),
			desc:   "Revoke an active Virtual Key by name, key prefix, or id (resolved to its id). Irreversible.",
			confirmDetail: func(in json.RawMessage) string {
				var a struct {
					VK string `json:"vk"`
				}
				_ = json.Unmarshal(in, &a)
				return fmt.Sprintf("revoke virtual key %q (irreversible)", a.VK)
			},
			impact: func(ctx context.Context, in json.RawMessage) (any, error) {
				var a struct {
					VK string `json:"vk"`
				}
				_ = json.Unmarshal(in, &a)
				_, label, err := resolveRevocableVK(ctx, gw, a.VK)
				if err != nil {
					return nil, err
				}
				return map[string]any{
					"action":  "revoke",
					"summary": "Permanently revokes this Virtual Key. Any app still using it will immediately start getting 401 Unauthorized. This cannot be undone.",
					"current": map[string]any{
						"virtualKey": label,
					},
					"irreversible": true,
				}, nil
			},
			run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					VK string `json:"vk"`
				}
				_ = json.Unmarshal(in, &a)
				id, label, err := resolveRevocableVK(ctx, gw, a.VK)
				if err != nil {
					return errResult("%s", err), nil
				}
				if err := gw.RevokeVK(ctx, id); err != nil {
					return errResult("revoke failed: %s", err), nil
				}
				return jsonResult(map[string]any{"revoked": label})
			}},

		&funcTool{name: "mitigate_passthrough_global", tier: agent.TierConfirm,
			schema: json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"},"bypassCache":{"type":"boolean"},"bypassNormalize":{"type":"boolean"},"reason":{"type":"string"}},"required":["enabled"]}`),
			desc:   "Engage or disengage the global emergency-passthrough tier. Engaging bypasses the compliance hooks fleet-wide.",
			confirmDetail: func(in json.RawMessage) string {
				var a struct {
					Enabled bool `json:"enabled"`
				}
				_ = json.Unmarshal(in, &a)
				if a.Enabled {
					return "engage global emergency passthrough (bypasses compliance hooks fleet-wide)"
				}
				return "disengage global emergency passthrough"
			},
			impact: func(ctx context.Context, in json.RawMessage) (any, error) {
				var a struct {
					Enabled bool `json:"enabled"`
				}
				_ = json.Unmarshal(in, &a)
				snap, err := gw.PassthroughSnapshot(ctx)
				if err != nil {
					return nil, err
				}
				adapters, providers := snap.ActiveOverrides()
				action, summary := "disengage", "Restores fleet-wide compliance interception (hooks/cache/normalize) on the global tier."
				if a.Enabled {
					action, summary = "engage", "Bypasses the compliance hooks on EVERY request, fleet-wide, until disengaged — traffic flows uninspected."
				}
				return map[string]any{
					"action":  action,
					"summary": summary,
					"current": map[string]any{
						"globalEnabled":           snap.Global.Enabled,
						"globalBypassHooks":       snap.Global.BypassHooks,
						"activeAdapterOverrides":  adapters,
						"activeProviderOverrides": providers,
					},
				}, nil
			},
			run: func(ctx context.Context, in json.RawMessage) (agent.Result, error) {
				var a struct {
					Enabled         bool   `json:"enabled"`
					BypassCache     bool   `json:"bypassCache"`
					BypassNormalize bool   `json:"bypassNormalize"`
					Reason          string `json:"reason"`
				}
				_ = json.Unmarshal(in, &a)
				req := core.PassthroughGlobalRequest{Enabled: a.Enabled, Reason: a.Reason}
				if a.Enabled {
					req.BypassHooks = true // server rejects an enabled tier that bypasses nothing
					req.BypassCache = a.BypassCache
					req.BypassNormalize = a.BypassNormalize
				}
				if err := gw.SetPassthroughGlobal(ctx, req); err != nil {
					return errResult("passthrough write failed: %s", err), nil
				}
				return jsonResult(map[string]any{"enabled": a.Enabled})
			}},
	}
}
