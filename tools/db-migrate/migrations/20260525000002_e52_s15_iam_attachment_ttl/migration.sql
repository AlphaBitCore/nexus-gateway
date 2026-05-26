-- E52-S15: time-bounded IAM policy attachments.
--
-- A NULL expires_at means "permanent" (today's behaviour). A non-NULL
-- value scopes the attachment to a window: the IAM Engine drops the
-- policy from a principal's effective set the instant the deadline
-- passes (filter at loadPolicies time).
--
-- Use case: break-glass / incident response. Grant
-- `NexusIncidentResponse` to an on-call admin for 4 hours during an
-- active incident; access automatically revokes at deadline without
-- a manual revoke step or fear-of-forgetting.
--
-- Schema is additive only; existing rows have NULL and behave
-- identically to today.

ALTER TABLE "IamPolicyAttachment" ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ NULL;

-- Partial index on non-NULL expires_at — most attachments are
-- permanent, so a partial index keeps the index size proportional
-- to the temporary-grant population.
CREATE INDEX IF NOT EXISTS iam_policy_attachment_expires_idx
    ON "IamPolicyAttachment"(expires_at)
    WHERE expires_at IS NOT NULL;
