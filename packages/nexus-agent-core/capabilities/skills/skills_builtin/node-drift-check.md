---
name: node-drift-check
description: Check fleet health and find nodes whose applied config has drifted from target
allowed-tools: observe_nodes, observe_health, resource_search, resource_read, navigate
---

# Node drift check

Use this when the operator asks about node health, the fleet, config sync, or drift.

1. Read the fleet (`observe_nodes`) and the at-a-glance totals (`observe_health`) —
   note how many nodes are online vs expected.
2. Flag every node whose applied config lags its target (the drifted ones) — name
   them by node name, never a bare id.
3. For drift detail, `resource_search "config sync out of sync"` then `resource_read`
   the matching operation to list the nodes still behind, and `navigate` to `sync`
   so the operator sees the same view.
4. Summarize: total nodes, how many online, how many drifted (and which), and the
   likely cause (a recent config push not yet applied, or an offline node). If a node
   is offline, say so — a config push can't apply to a node that isn't connected.
