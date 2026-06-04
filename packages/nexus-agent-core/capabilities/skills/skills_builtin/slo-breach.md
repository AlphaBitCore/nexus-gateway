---
name: slo-breach
description: Triage a latency or availability breach to the provider and route behind it
allowed-tools: analyze_slo, observe_traffic_list, observe_traffic_event, route_explain, navigate, show_event
---

# SLO breach triage

Use this when latency is high, availability is down, or an SLO/percentile looks wrong.

1. Read provider SLO over the relevant window (`analyze_slo`) — honor the time range
   in the question; note overall availability, the worst per-provider latency
   percentiles (p50/p95/TTFB), and any routing-fallback activity.
2. Identify the provider(s) driving the breach (highest p95 / most errors / most
   fallbacks). Name the provider, cite its numbers.
3. Pull the slow/failing traffic (`observe_traffic_list`, filter `status=5xx` or
   `error`) and drill the worst recent event (`observe_traffic_event` → `show_event`)
   to see where the latency is spent (upstream TTFB vs hooks vs total).
4. Check what a request to that model resolves to (`route_explain`) — a bad primary
   with no healthy fallback explains an availability dip.
5. State the breached SLO, the responsible provider, and the evidence. Propose a
   mitigation (disable the bad provider, or rely on the fallback) but PROPOSE only —
   the operator confirms every change on the gate.
