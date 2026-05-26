/** Action taken by the agent policy engine on a connection. */
export type AuditAction =
  | 'inspect'
  | 'passthrough'
  | 'deny'
  | 'agent.shutdown';

/**
 * AuditEvent is the end-user-facing shape returned by both the local
 * agent (statusapi) and the admin API (when scoped to a device).
 * Admin-only enrichment fields are optional in this base so the agent
 * path can omit them; the CP UI's `AgentAuditEvent` extends this and
 * marks the fields it always returns as required.
 *
 * `action` is typed as `string` rather than the narrower `AuditAction`
 * union because consumers (CP) often surface server-defined extension
 * actions that aren't in the closed union yet. The `AuditAction`
 * literal type is exported for components that want to type-narrow.
 */
export interface AuditEvent {
  id: string;
  /** RFC3339 timestamp the connection was observed. */
  timestamp: string;
  /** Process name on the client that initiated the connection. */
  sourceProcess: string;
  /** Destination host (FQDN or IP). */
  destHost: string;
  /** Destination IP after DNS resolution. */
  destIp?: string;
  /** Destination port. May be 0 for ICMP / unknown protocols. */
  destPort?: number;
  /** Action taken by the policy engine. */
  action: string;
  /** Bytes sent from client to server in this connection. */
  bytesIn?: number | null;
  /** Bytes sent from server to client in this connection. */
  bytesOut?: number | null;
  /** Connection duration in milliseconds. */
  duration?: number | null;

  /** Device that produced the event. Set by admin path; the local
   * agent omits this because every event is implicitly its own. */
  deviceId?: string;
  /** OS-user that owned the source process, when available. */
  sourceUser?: string | null;
  /** Foreign key into the policy_rule table. */
  policyRuleId?: string | null;
  /** TLS-bump outcome ("ok", "skipped", "failed", …). */
  bumpStatus?: string | null;
  /** Subject (e.g. user-claim) attached by the auth pipeline. */
  subjectId?: string | null;
  /** Hook decision returned by the v2 compliance pipeline. */
  requestHookDecision?: string | null;
}

/** Today's aggregate counts surfaced on the menu bar and Dashboard Overview. */
export interface TodayStats {
  inspected: number;
  passthrough: number;
  denied: number;
}
