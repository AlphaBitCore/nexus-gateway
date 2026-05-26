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
 * Decision tree (first match wins):
 *   1. domainRuleId empty                    → untracked
 *   2. bumpStatus FAILED                     → bump_failed
 *   3. hookDecision deny / reject / block    → blocked
 *   4. hookDecision approve                  → processed
 *   5. action == "deny"                      → blocked
 *   6. fallthrough                           → inspect
 */
export function classify(e: AgentEvent): Classification {
  // Untracked: host wasn't in interception_domain at all.
  if (!e.domainRuleId) {
    // Rows without domainRuleId fall back to the action verb for
    // classification. action="inspect" or "deny" means a domain was
    // matched in an older daemon; fall through to the branches below.
    if (e.action === 'inspect' || e.action === 'deny') {
      // fall through
    } else {
      return 'untracked';
    }
  }

  // Bump failures take priority — a non-bumped flow can't have run hooks.
  if (
    e.bumpStatus === 'BUMP_FAILED' ||
    e.bumpStatus === 'BUMP_FAILED_PASSTHROUGH' ||
    e.bumpStatus === 'BUMP_FAILED_MINT_FALLBACK_RELAY'
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
