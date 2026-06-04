---
name: compliance-audit
description: Check whether traffic is being governed and whether anything is slipping past compliance
allowed-tools: analyze_compliance, observe_passthrough, observe_traffic_list, navigate
---

# Compliance audit

Follow this playbook when the operator asks about governance coverage or whether traffic
is bypassing the compliance hooks.

1. Pull the compliance overview (`analyze_compliance`): total requests, blocked count,
   and the overall block rate over the recent window; `navigate` to `compliance`.
2. Check the emergency-passthrough snapshot (`observe_passthrough`) — if any tier is
   engaged, traffic is bypassing hooks/cache/normalize right now; call that out first.
3. Sample blocked traffic (`observe_traffic_list` with `status=error` or a block filter)
   to confirm the policy is firing on the right requests.
4. Summarize whether governance is effective: is the block rate plausible, is any
   passthrough tier engaged, and is anything ungoverned. Propose disengaging passthrough
   only as a confirmed mitigation.
