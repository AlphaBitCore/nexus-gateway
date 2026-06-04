---
name: cost-investigation
description: Find what is driving gateway cost and where it concentrates
allowed-tools: analyze_cost, observe_traffic_list, observe_models, navigate
---

# Cost investigation

Follow this playbook when the operator asks why cost is high or where spend concentrates.

1. Pull cost grouped by provider (`analyze_cost` with `groupBy=provider`) and read the
   top spenders; `navigate` to `cost` so the operator sees the same view.
2. Re-group by `model` and by `user` to see whether the spend is one expensive model,
   one heavy user, or broad.
3. Cross-check the model catalog (`observe_models`) for the per-million pricing of the
   top model so the driver is concrete.
4. Sample the recent traffic for the top driver (`observe_traffic_list` filtered by that
   model) to confirm the volume is real and not a single outlier.
5. Summarize the single biggest cost driver with the numbers behind it; suggest a routing
   or rate-limit change only as a proposal.
