-- E57 — Auto-upgrade /v1/chat/completions onto OpenAI /v1/responses upstream.
--
-- When this column is true on a routing rule AND the resolved target is
-- OpenAI AND the resolved model is in spec_openai's empirically verified
-- Responses-API support list, the AI Gateway rewrites the upstream
-- request to /v1/responses (gaining reasoning + native built-in tool
-- support) while preserving the client-facing chat-completions response
-- shape. See docs/developers/specs/e56/e56-s11-prefer-responses-api-flag.md (the spec
-- predates the rename from S11 to E57).
--
-- ALTER ADD COLUMN with a non-NULL default is O(1) metadata-only on
-- PG11+ — safe on the live routing_rule table.

ALTER TABLE "RoutingRule"
    ADD COLUMN "preferResponsesAPI" BOOLEAN NOT NULL DEFAULT false;
