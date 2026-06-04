---
name: cache-effectiveness
description: Assess prompt/response cache effectiveness — hit rate, savings, and config
allowed-tools: analyze_cost, observe_traffic_list, resource_search, resource_describe, resource_read, navigate
---

# Cache effectiveness

Use this when the operator asks whether caching is working or how much it saves.

1. Read the savings + hit picture: `analyze_cost` over the window (it carries cache
   hits + net cache savings alongside spend). State the cache hit count and the dollar
   savings for the window covered.
2. Sanity-check against traffic: `observe_traffic_list` — recent rows show per-request
   cache status, so you can confirm hits are actually landing, not just configured.
3. Inspect the cache config: `resource_search "semantic cache"` → `resource_describe`
   the kind → `resource_read` its config to report the TTL / thresholds in effect.
4. Summarize: hit rate, savings, and whether the config explains the result (e.g. a
   low hit rate with a tight threshold). If a config change would help, PROPOSE it —
   writing the cache config is confirm-gated.
