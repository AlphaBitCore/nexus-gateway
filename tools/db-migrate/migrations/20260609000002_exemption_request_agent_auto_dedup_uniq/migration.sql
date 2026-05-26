-- Partial unique index for agent auto-uploaded exemption requests.
--
-- Wired in: Hub-side ExemptionConsumer
-- (packages/nexus-hub/internal/jobs/consumer/exemption.go) reads
-- nexus.event.exemption events the agent uploads via
-- POST /api/internal/things/exemption and INSERTs them as
-- status='PENDING' rows for admin review at /compliance/exemptions.
--
-- Dedup invariant: at most one PENDING request per (target_host, requested_by).
-- ON CONFLICT (target_host, requested_by) DO UPDATE refreshes created_at +
-- reason on every retry so the row stays "fresh" without piling up rows.
-- An admin approval flips status to APPROVED, removing the row from this
-- partial index — a fresh auto-detect after approval can therefore re-INSERT,
-- which is correct (the original grant has its own expiry).
CREATE UNIQUE INDEX exemption_request_pending_dedup_uniq
  ON exemption_request (target_host, requested_by)
  WHERE status = 'PENDING';
