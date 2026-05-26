-- Drop the per-VK hard-cap budget column. Per-VK quota enforcement now
-- runs exclusively through the QuotaPolicy + QuotaOverride hierarchical
-- system in ai-gateway/internal/policy/quota; the X-Nexus-Quota-Used and
-- X-Nexus-Quota-Limit response headers continue to be emitted, sourced
-- from the new system's VK-level chain entry.

ALTER TABLE "VirtualKey" DROP COLUMN "budgetLimitUsd";
