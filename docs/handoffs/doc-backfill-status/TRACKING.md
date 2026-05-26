# Architecture docs backfill — tracking

Single on-disk record of which architecture docs have been written + reviewed via the `feature/docs-backfill` worktree. Survives session restarts. Update by re-running the inventory walk after each commit; do NOT edit by hand mid-session except for the **Audit findings** + **Code-fix prerequisites** sections, which carry per-doc work-state that the git log alone cannot express.

Rule (authoritative): a doc counts as "committed via our pipeline" only when it has at least one commit on `feature/docs-backfill` that is NOT on the integration branch (`origin/develop` on this fork). Uncommitted drafts and pre-existing integration-branch docs do NOT count.

Last inventory: <run-date-recorded-on-update>

## Summary

- Total architecture docs on disk: 91
- Committed via this worktree: 7
- Uncommitted drafts in working tree: 0
- Pre-existing on integration branch (not yet rewritten on this branch): 84

Note: no uncommitted drafts remain. Both safety drafts (kill-switch, pii-redaction) have landed via this worktree's PR-B/PR-C work and now appear in the Committed table.

## Code-fix prerequisites

Some doc rewrites cannot accurately describe current-state semantics until an underlying code bug is fixed. Tracked here because doc state ⨯ code state is the load-bearing constraint, not doc state alone.

| ID | Description | Status | Blocks docs |
|---|---|---|---|
| PR-C | AI-Guard reconcile producer — `webhook-forward.Execute` reconciles webhook decision against `onMatch.InflightAction` ceiling using `core.StrictestDecision`; stamps `ReasonAIGuardSuggestedVsPolicy` on divergence. Pipeline `mergeResults` Modify branch propagates hook ReasonCode + sorts by Order for parallel determinism. Two architecture-review rounds. | LANDED (e8d496253) | pii-redaction-policy-architecture.md |
| PR-B | kill-switch wire-field semantic inversion — wire renamed `enabled`→`engaged`, agent semantic flipped to match canonical, bridge inversion wrapper deleted. Includes B04 doc rewrite + prod-deploy SQL one-liners in `docs/operators/ops/runbooks/prod-deploy-data-changes.md`. Two independent architecture review rounds + Pass-2 audit + 8 BUGs/CONCERNs fixed in same commit. | LANDED (25d60478c) | kill-switch-architecture.md |

Source: `docs/handoffs/quota-killswitch-aiguard/HANDOFF.md`. Both confirmed by per-claim audits run on the B04 + B06 drafts in this session.

## Committed via this worktree

All five have passed `/doc-review` Pass 2 per-claim audit with the latest fixes applied; no known drift remaining.

| Doc path | Last branch-local commit | Subject |
|---|---|---|
| docs/developers/architecture/cross-cutting/foundation/endpoint-typology-architecture.md | a499e7ee5 | refactor(typology): rename legacy.go → path_segment.go |
| docs/developers/architecture/cross-cutting/observability/admin-audit-log-coverage.md | cd27425a7 | docs: admin-audit-log-coverage — doc-review Pass 2 fixes |
| docs/developers/architecture/cross-cutting/observability/alerting-architecture.md | 234030af1 | chore: schema.prisma comment drift fixes + AllSourceTypes lockstep |
| docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md | 3acbfc38d | refactor(audit): single authoritative embedding coerce at Writer.Enqueue |
| docs/developers/architecture/cross-cutting/observability/observability-architecture.md | ac492bd89 | docs: observability-architecture — re-sync counter names after `nexus_` namespace pin |
| docs/developers/architecture/cross-cutting/safety/pii-redaction-policy-architecture.md | 94668990c | docs: pii-redaction-policy-architecture — post-PR-C wired reconcile + drift fixes |
| docs/developers/architecture/cross-cutting/safety/kill-switch-architecture.md | 25d60478c | refactor(killswitch): PR-B rename wire field enabled→engaged + unify semantic |

## Pre-existing on integration branch (not yet rewritten on this branch)

85 docs. The one remaining draft in **Uncommitted drafts** above also appears here per the inventory rule — it has working-tree modifications but no branch-local commit yet, so the binary "committed?" rule places it in this bucket too until it lands.

| Doc path |
|---|
| docs/developers/architecture/agent-windows-wfp-driver.md |
| docs/developers/architecture/cross-cutting/foundation/configuration-architecture.md |
| docs/developers/architecture/cross-cutting/foundation/jobs-architecture.md |
| docs/developers/architecture/cross-cutting/foundation/mq-architecture.md |
| docs/developers/architecture/cross-cutting/foundation/multi-endpoint-coordination-architecture.md |
| docs/developers/architecture/cross-cutting/foundation/nexus-response-markers.md |
| docs/developers/architecture/cross-cutting/foundation/service-bootstrap-config-architecture.md |
| docs/developers/architecture/cross-cutting/foundation/service-call-framework.md |
| docs/developers/architecture/cross-cutting/foundation/thing-config-sync-architecture.md |
| docs/developers/architecture/cross-cutting/foundation/thing-model.md |
| docs/developers/architecture/cross-cutting/observability/diag-event-triage-architecture.md |
| docs/developers/architecture/cross-cutting/observability/metrics-rollup-architecture.md |
| docs/developers/architecture/cross-cutting/observability/otel-pipeline-architecture.md |
| docs/developers/architecture/cross-cutting/observability/otel-span-attributes-architecture.md |
| docs/developers/architecture/cross-cutting/observability/prometheus-naming-architecture.md |
| docs/developers/architecture/cross-cutting/observability/runtime-introspection-architecture.md |
| docs/developers/architecture/cross-cutting/observability/siem-bridge-architecture.md |
| docs/developers/architecture/cross-cutting/observability/test-harness-architecture.md |
| docs/developers/architecture/cross-cutting/observability/trace-id-propagation-architecture.md |
| docs/developers/architecture/cross-cutting/safety/credentials-architecture.md |
| docs/developers/architecture/cross-cutting/safety/emergency-passthrough-architecture.md |
| docs/developers/architecture/cross-cutting/safety/error-taxonomy-architecture.md |
| docs/developers/architecture/cross-cutting/safety/quota-architecture.md |
| docs/developers/architecture/cross-cutting/safety/sse-streaming-compliance-architecture.md |
| docs/developers/architecture/cross-cutting/shared/shared-go-utilities-architecture.md |
| docs/developers/architecture/cross-cutting/shared/shared-package-architecture.md |
| docs/developers/architecture/cross-cutting/shared/shared-utility-subpackages-architecture.md |
| docs/developers/architecture/cross-cutting/shared/shared-wirerewrite-architecture.md |
| docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md |
| docs/developers/architecture/cross-cutting/storage/data-retention-purge-architecture.md |
| docs/developers/architecture/cross-cutting/storage/db-migration-mechanics-architecture.md |
| docs/developers/architecture/cross-cutting/storage/spillstore-architecture.md |
| docs/developers/architecture/cross-cutting/ui/design-tokens-architecture.md |
| docs/developers/architecture/cross-cutting/ui/i18n-pipeline-architecture.md |
| docs/developers/architecture/cross-cutting/ui/recharts-theme-architecture.md |
| docs/developers/architecture/cross-cutting/ui/sidebar-ia-architecture.md |
| docs/developers/architecture/cross-cutting/ui/theme-architecture.md |
| docs/developers/architecture/cross-cutting/ui/ui-shared-architecture.md |
| docs/developers/architecture/cross-cutting/ui/useapi-queryclient-architecture.md |
| docs/developers/architecture/overview.md |
| docs/developers/architecture/project-structure.md |
| docs/developers/architecture/services/agent/agent-attestation-architecture.md |
| docs/developers/architecture/services/agent/agent-autoupdater-architecture.md |
| docs/developers/architecture/services/agent/agent-backpressure-rollup-architecture.md |
| docs/developers/architecture/services/agent/agent-browser-opener-architecture.md |
| docs/developers/architecture/services/agent/agent-enrollment-architecture.md |
| docs/developers/architecture/services/agent/agent-exemption-grants-architecture.md |
| docs/developers/architecture/services/agent/agent-forwarder-architecture.md |
| docs/developers/architecture/services/agent/agent-internals-sibling-pairs-architecture.md |
| docs/developers/architecture/services/agent/agent-keystore-architecture.md |
| docs/developers/architecture/services/agent/agent-linux-platform-architecture.md |
| docs/developers/architecture/services/agent/agent-macos-platform-architecture.md |
| docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md |
| docs/developers/architecture/services/agent/agent-paths-abstraction-architecture.md |
| docs/developers/architecture/services/agent/agent-policy-eval-architecture.md |
| docs/developers/architecture/services/agent/agent-protection-pause-architecture.md |
| docs/developers/architecture/services/agent/agent-sso-enrollment-architecture.md |
| docs/developers/architecture/services/agent/agent-telemetry-architecture.md |
| docs/developers/architecture/services/agent/agent-tray-ipc-architecture.md |
| docs/developers/architecture/services/agent/agent-windows-platform-architecture.md |
| docs/developers/architecture/services/agent/macos-build-signing-architecture.md |
| docs/developers/architecture/services/ai-gateway/ai-gateway-internals-architecture.md |
| docs/developers/architecture/services/ai-gateway/aiguard-architecture.md |
| docs/developers/architecture/services/ai-gateway/cost-estimation-architecture.md |
| docs/developers/architecture/services/ai-gateway/forward-header-allowlist-architecture.md |
| docs/developers/architecture/services/ai-gateway/hook-architecture.md |
| docs/developers/architecture/services/ai-gateway/normalization-architecture.md |
| docs/developers/architecture/services/ai-gateway/prompt-cache-architecture.md |
| docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md |
| docs/developers/architecture/services/ai-gateway/provider-coverage.md |
| docs/developers/architecture/services/ai-gateway/response-cache-architecture.md |
| docs/developers/architecture/services/ai-gateway/routing-architecture.md |
| docs/developers/architecture/services/ai-gateway/smart-routing-architecture.md |
| docs/developers/architecture/services/compliance-proxy/compliance-pipeline-architecture.md |
| docs/developers/architecture/services/compliance-proxy/compliance-proxy-details-architecture.md |
| docs/developers/architecture/services/compliance-proxy/domain-device-predicate-architecture.md |
| docs/developers/architecture/services/control-plane/control-plane-internals-architecture.md |
| docs/developers/architecture/services/control-plane/iam-identity-architecture.md |
| docs/developers/architecture/services/control-plane/idp-sso-architecture.md |
| docs/developers/architecture/services/control-plane/jwt-verifier-architecture.md |
| docs/developers/architecture/services/control-plane/oauth-pkce-admin-auth-architecture.md |
| docs/developers/architecture/services/control-plane/tenancy-architecture.md |
| docs/developers/architecture/services/control-plane/vk-org-resolution.md |
| docs/developers/architecture/services/hub/nexus-hub-internals-architecture.md |

## How to refresh this file

Re-run the inventory walk from the worktree root. The bulk of the file is regenerable from `git`; only the **Code-fix prerequisites** + per-doc **Audit findings** subsections under "Uncommitted drafts" need manual maintenance because they encode work-state the commit history alone does not.

1. Enumerate every architecture doc, excluding `_archive/`:

   ```bash
   find docs/developers/architecture -name '*.md' -not -path '*/_archive/*' | sort > /tmp/all_arch_docs.txt
   ```

2. Classify each doc by branch-local commit presence. The base ref is the integration branch this feature branches from — `origin/develop` on this fork (substitute `origin/main` on upstream layouts):

   ```bash
   while IFS= read -r doc; do
     log=$(git log --oneline origin/develop..HEAD --follow -- "$doc" 2>/dev/null | head -1)
     if [ -n "$log" ]; then
       echo "COMMITTED|$doc|$log"
     else
       echo "PREEXISTING|$doc|"
     fi
   done < /tmp/all_arch_docs.txt > /tmp/arch_inventory.txt
   ```

3. List uncommitted drafts:

   ```bash
   git status --short -- docs/developers/architecture/
   ```

4. Rewrite the **Summary**, **Committed**, and **Pre-existing** tables + the **Uncommitted drafts** working-tree-status lines from `/tmp/arch_inventory.txt` and the `git status` output.

5. Leave **Code-fix prerequisites** + per-draft **Audit findings** subsections untouched unless you have a new audit result or a PR has landed. When a PR lands, update its Status here and remove the blocker line from the affected draft.

6. The `<run-date-recorded-on-update>` placeholder stays literal; the file's git mtime carries the real timestamp.
