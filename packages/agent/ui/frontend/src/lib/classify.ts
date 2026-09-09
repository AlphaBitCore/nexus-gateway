/**
 * Traffic event classification — single source of truth for the
 * Status badge + AI tag rendering across Traffic.tsx / Stats.tsx.
 *
 * Derived from audit_event fields the daemon persists. Mirrors
 * `audit.Classify` on the Go side so the same row classifies the same
 * way locally vs Hub-side.
 *
 * Field semantics:
 *   domainRuleId  — non-empty when host matched interception_domain.
 *                   Empty / undefined = Untracked.
 *   pathAction    — "PROCESS" | "PASSTHROUGH" | "BLOCK".
 *   hookDecision  — "approve" | "reject_hard" | "block_soft" | "deny" |
 *                   "" (no hook ran).
 *   bumpStatus    — "BUMP_SUCCESS" | "BUMP_FAILED_*" | "" (not bumped).
 *   action        — legacy terse verb; used as fallback for rows that
 *                   predate domainRuleId.
 */
import type { AgentEvent } from '@/api/agent';

export type Classification =
  | 'untracked'
  | 'inspect'
  | 'processed'
  | 'blocked'
  | 'bump_failed';

/**
 * Decision tree (first match wins). Mirrors
 * packages/agent/internal/observability/audit/classify/classify.go —
 * keep the two in step, they decide the same row.
 *
 *   1. bumpStatus FAILED*, on a matched flow      → bump_failed
 *   2. hookDecision deny / reject / block        → blocked
 *   3. action == "deny"                          → blocked
 *   4. hookDecision approve                      → processed
 *   5. not matched                               → untracked
 *   6. fallthrough                               → inspect
 *
 * The untracked fallback is LAST. Reaching it first and escaping with a
 * fall-through when action is "inspect"/"deny" gets the same badge here, but
 * the Go side has no such escape: it classifies the same row "untracked" and,
 * because untracked is not uploaded at the default level, the denial never
 * reaches the console while this badge says "blocked". Consulting the
 * verdicts first says the same thing without the trick.
 */
/**
 * Whether an interception_domain row applied to this flow.
 *
 * Normally a stamped domainRuleId. A row from an OLDER DAEMON carries the verb
 * instead — action "inspect" or "deny" means a domain matched even though no
 * rule id was recorded. Mirrors matched() in classify.go; naming it once on
 * each side is what keeps them from disagreeing about which rows count.
 */
function matched(e: AgentEvent): boolean {
  return Boolean(e.domainRuleId) || e.action === 'inspect' || e.action === 'deny';
}

export function classify(e: AgentEvent): Classification {
  // Bump failures take priority — a non-bumped flow can't have run hooks.
  // Gated on matched: a transport error on a host we never intended to bump
  // is not a bump failure.
  if (
    matched(e) &&
    (e.bumpStatus === 'BUMP_FAILED' ||
      e.bumpStatus === 'BUMP_FAILED_PASSTHROUGH' ||
      e.bumpStatus === 'BUMP_FAILED_MINT_FALLBACK_RELAY')
  ) {
    return 'bump_failed';
  }

  // Daemon emits hookDecision in mixed case ("APPROVE", "DENY", etc.).
  // Normalise before matching to avoid silent fall-through on casing differences.
  switch ((e.hookDecision ?? '').toLowerCase()) {
    case 'reject_hard':
    case 'block_soft':
    case 'deny':
      return 'blocked';
    case 'approve':
      return 'processed';
  }

  // action="deny" without a hook decision still means denied.
  if (e.action === 'deny') {
    return 'blocked';
  }

  // Only now: no verdict of any kind, and nothing matched.
  if (!matched(e)) {
    return 'untracked';
  }

  // PathAction PASSTHROUGH explicitly means admin asked us to skip
  // hooks; otherwise we fell through with no hook output for an
  // unknown reason — both surface as Inspect.
  return 'inspect';
}

/**
 * Returns true when the event should be tagged with the "AI" chip.
 * Derived from domainRuleId (host matched interception_domain).
 * Falls back to action="inspect" for rows that predate domainRuleId.
 */
export function isAITraffic(e: AgentEvent): boolean {
  if (e.domainRuleId) return true;
  // Rows without domainRuleId: legacy daemon stamped action="inspect"
  // for matched hosts. Use as an approximate AI signal.
  return e.action === 'inspect';
}

/**
 * Human-readable label + tone for the Status badge. The agent UI uses
 * tone to map onto its --color-success / --color-warning / --color-danger
 * variables.
 */
export interface StatusDescriptor {
  classification: Classification;
  label: string;
  tone: 'good' | 'warn' | 'bad' | 'muted';
}

export function statusDescriptor(e: AgentEvent): StatusDescriptor {
  const c = classify(e);
  switch (c) {
    case 'untracked':
      return { classification: c, label: 'Untracked', tone: 'muted' };
    case 'inspect':
      return { classification: c, label: 'Inspected', tone: 'good' };
    case 'processed':
      return { classification: c, label: 'Processed', tone: 'warn' };
    case 'blocked':
      return { classification: c, label: 'Blocked', tone: 'bad' };
    case 'bump_failed':
      return { classification: c, label: 'Bump failed', tone: 'bad' };
  }
}
