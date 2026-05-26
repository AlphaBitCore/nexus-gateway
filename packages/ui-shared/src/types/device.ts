/**
 * TrustLevel reflects `thing_agent.trust_level` (0-3) as computed by
 * Hub. The agent caches it locally after each enroll/renew so the
 * Dashboard can render the level without round-tripping to Hub.
 *
 *   0 — revoked or expired certificate; agent traffic should be denied.
 *   1 — enrolled with a valid cert but no DeviceAssignment row
 *       (typical for mtls-only fleets).
 *   2 — level 1 + an active DeviceAssignment (user identity known).
 *   3 — level 2 + agent version meets the operator's min-version policy.
 */
export type TrustLevel = 0 | 1 | 2 | 3;

/** Operator-configured device-auth posture. Pushed from CP system_metadata. */
export type DeviceAuthMode = 'mtls-only' | 'enterprise-login';

/** OS identifier matching `runtime.GOOS`. */
export type DeviceOS = 'darwin' | 'windows' | 'linux';

/**
 * Device is the identity + slow-moving metadata shape that both the
 * agent's statusapi and the Control Plane admin API can produce. CP
 * extends this with admin-only fields (enrolledBy, fleet metadata,
 * lifecycle status); the agent Dashboard extends it with live
 * runtime status. Status fields deliberately live in the consumer
 * types because "admin status" (ENROLLED/OFFLINE) and "runtime
 * status" (active/paused) are different concepts.
 */
export interface Device {
  id: string;
  hostname: string;
  os: DeviceOS;
  osVersion: string;
  agentVersion: string;
  /** RFC3339 timestamp of the agent's most recent heartbeat. */
  lastHeartbeat?: string | null;
  /** RFC3339 timestamp when the device certificate expires. */
  certExpiresAt?: string | null;
  /** Computed trust level (0-3). May be omitted by older agents. */
  trustLevel?: TrustLevel;
  /**
   * Operator-configured posture surfaced via Hub bootstrap. The
   * Dashboard uses it to gate the SSO vs token onboarding branches.
   */
  deviceAuthMode?: DeviceAuthMode;
}
