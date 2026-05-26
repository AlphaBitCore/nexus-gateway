# Flow — Virtual Key lifecycle

## What this flow accomplishes

An admin issues a Virtual Key (VK) to an application; the application uses it on `/v1/*`; traffic events flow back with proper attribution; cost analytics rolls up by org.

## Actors

Admin · Control Plane · Hub (config propagation only) · AI Gateway · App · Postgres · MQ · CP analytics UI.

The Control Plane owns the VK lifecycle. The Hub is **not** in the create/update path — its only role is to fan the resulting config change out to the AI Gateway via the shadow / change-signal pipeline.

## Sequence

1. **Admin → CP UI → New VK** at `/ai-gateway/virtual-keys/new` → scope to org/project → restrict models / attach quota policy → submit.
2. **CP admin handler** (`packages/control-plane/internal/ai/virtualkeys/handler/vk.go:72` `CreateVirtualKey`) → IAM check (`admin:virtual-key.create`) → hash + persist the `VirtualKey` row in Postgres → return the generated plaintext to the admin (shown ONCE in the UI).
3. **CP → Hub** calls `InvalidateConfig(ai-gateway, "virtual_keys")` over the Hub HTTP API. `virtual_keys` is a **Category B** config key (`packages/shared/schemas/configkey/configkey.go:134`): the AI Gateway pulls the fresh list from CP on the next signal.
4. **Hub** broadcasts a change-signal to AI Gateway over the Thing WebSocket; AI Gateway's `OnConfigChanged` callback pulls the updated `virtual_keys` slice from CP.
5. **AI Gateway → CP** loads the new VK metadata into its in-process cache (cross-ref `thing-config-sync-architecture.md`).
6. **App → `POST /v1/chat/completions` `Authorization: Bearer vk-...`** → AI Gateway `vkauth` (`packages/ai-gateway/internal/auth/vkauth/`) resolves the VK → hydrates `VKContext` (org / org-path / project / VK metadata) → routing → upstream → response.
7. **AI Gateway** emits `traffic_event` to MQ; the envelope carries `org_id`, `org_name`, `entity_type` / `entity_id` / `entity_name` (the attribution triplet — `entity_type` is one of `"user"` / `"project"` / `"unknown"` per `schema.prisma:1335`), `request_id`, `trace_id` (no `org_ancestor_path` column — see `tenancy-architecture.md` §6).
8. **Hub audit-sink** → write the `traffic_event` row to Postgres (body overflow to spillstore if ≥ 256 KiB).
9. **CP analytics UI** → query `traffic_event` group by `entity_id` / `org_id` → cost rollup surfaces in `/analytics` and `/quota-usage`.

## Failure modes

- **VK resolution misses** — bad bearer; `vkauth` returns 401 with `code=invalid_vk`.
- **VK revoked mid-request** — request completes if already authenticated; subsequent requests 401.
- **`last_used_at` write rate limit** — once-per-minute per VK; not every request writes.
- **Cost stamping incomplete** — providers without `usage.cost_usd` produce estimates; UI marks with asterisk (cross-ref `provider-adapter-architecture.md`).
- **Personal VK org missing** — personal VKs resolve org via `NexusUser.organizationId`, not `Project.organizationId`. The dual-chain COALESCE in `vkSelectSQL` covers both populations; see `vk-org-resolution.md`.

## Verification

```bash
# 1) Issue a VK and capture the plaintext.
# 2) Smoke a request:
curl -H "Authorization: Bearer vk-..." https://nexus.<tenant>/v1/chat/completions -d '...'
# 3) Confirm a traffic_event row exists:
docker exec postgres psql -U postgres -d nexus_gateway \
  -c "SELECT entity_type, entity_id, entity_name, org_id, org_name, timestamp FROM traffic_event ORDER BY timestamp DESC LIMIT 1"
# 4) Confirm the row appears in /analytics within ~1 minute.
```

## References

- `docs/developers/architecture/cross-cutting/safety/credentials-architecture.md` §1 — VK model.
- `docs/developers/architecture/cross-cutting/foundation/multi-endpoint-coordination-architecture.md` — flow diagram (cross-cutting).
- `docs/developers/architecture/services/control-plane/vk-org-resolution.md` — dual application/personal VK join chain.
- `docs/users/features/cp-ui/ai-gateway.md` — admin surface.
- `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md` — traffic_event ingestion.
- `.claude/skills/smoke-gateway/` — smoke runner.
