---
name: vk-hygiene
description: Audit Virtual Keys for stale, disabled, or unused keys and propose cleanup
allowed-tools: resource_search, resource_describe, resource_read, analyze_cost, navigate
---

# Virtual Key hygiene

Use this when the operator wants to review, audit, or clean up Virtual Keys.

1. List the keys: `resource_search "virtual keys"` → `resource_read` the list. Show
   each by name + key prefix + status (never the raw id), and note which are disabled
   or in a non-active status.
2. Cross-reference usage: `analyze_cost` grouped by user/key over a wide window — a key
   with zero recent spend is a cleanup candidate; a key with unexpected spend is worth
   flagging.
3. For a key the operator asks about, `resource_describe virtual-keys` to find the read
   operation and `resource_read` its detail.
4. Summarize the hygiene findings (stale, disabled, never-used, over-spending) and
   PROPOSE which to revoke or rotate. Revoking or regenerating a key is an
   irreversible write — the operator confirms each on the gate; only "active" keys are
   revocable.
