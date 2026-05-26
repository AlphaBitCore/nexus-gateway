-- Drop three columns from RoutingRule that no longer have any code reading or
-- writing them. Each was retired in the 2026-05-20 routing-rule cleanup:
--   * preferResponsesAPI (E57): the auto-upgrade hook in proxy.go was removed
--     because the customer value was thin (response shape was translated back
--     to chat-completions so reasoning content / built-in tools never reached
--     the client) and the hardcoded Go prefix list was a maintenance tax.
--   * response_cache_policy (E61-S2): per-route cache policy was misplaced on
--     the routing rule. L2 semantic cache is now gated by the fleet-wide
--     semantic_cache_config singleton; L1 extract policy will move to the
--     unified cache hub in the follow-up phase.
--   * defaultExtensions (E61-D3): per-rule nexus.ext.* injection — replaced by
--     adapter-side PrepareBody defaults (canonical pattern: Anthropic
--     max_tokens default in packages/ai-gateway/internal/providers/specs/
--     anthropic/codec/codec.go).
ALTER TABLE "RoutingRule" DROP COLUMN IF EXISTS "preferResponsesAPI";
ALTER TABLE "RoutingRule" DROP COLUMN IF EXISTS response_cache_policy;
ALTER TABLE "RoutingRule" DROP COLUMN IF EXISTS "defaultExtensions";
