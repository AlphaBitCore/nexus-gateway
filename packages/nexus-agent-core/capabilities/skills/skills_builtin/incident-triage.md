---
name: incident-triage
description: Walk a firing alert to its root cause across alerts, nodes, and recent errors
allowed-tools: observe_alerts, observe_nodes, observe_traffic_list, observe_traffic_event, navigate, show_event
---

# Incident triage

Follow this playbook when the operator reports an incident or an alert is firing.

1. Open the alerts view and read what is firing (`observe_alerts`, then `navigate` to `alerts`).
2. For each firing alert, identify the affected service or provider.
3. Check node health and config drift (`observe_nodes`) — an out-of-sync node is a
   common root cause.
4. Pull the recent error traffic (`observe_traffic_list` with `status=error`) and drill
   the most recent failing event (`observe_traffic_event`, then `show_event`).
5. State the most likely root cause, cite the data you used, and propose a mitigation.
   Do NOT mitigate without explicit operator confirmation — propose, then wait.
