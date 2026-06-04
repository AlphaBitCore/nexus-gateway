---
name: provider-outage
description: Respond to a provider degrading or failing — confirm it, then propose a route around it
allowed-tools: observe_alerts, analyze_slo, observe_traffic_list, route_explain, resource_search, resource_read, navigate
---

# Provider outage response

Use this when a provider looks down or is erroring (an alert names it, or errors spike).

1. Confirm the blast radius: `observe_alerts` (is it firing?), `analyze_slo` (its
   availability + error rate vs the others), and `observe_traffic_list` filtered to
   `status=5xx`/`error` to see the failing requests for that provider.
2. Check routing: `route_explain` for an affected model — does it resolve to the bad
   provider with a healthy fallback behind it, or is there no recovery target?
3. Inspect the routing rules that govern it (`resource_search "routing rules"` →
   `resource_read` the rules) to see what would absorb the traffic if you disabled it.
4. PROPOSE the mitigation with the evidence: either disable the provider (so routing
   skips it) or confirm the fallback already covers it. Disabling a provider and
   toggling a routing rule are writes — the operator authorizes each on the gate.
   Never disable a provider without a fallback that keeps requests flowing.
