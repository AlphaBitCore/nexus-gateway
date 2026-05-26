import type { DeviceAuthMode } from './device';

/**
 * BootstrapInfo is what Hub's GET /api/public/agent-bootstrap returns
 * and what the agent caches locally for the Dashboard to consume.
 * Both fields may be empty during initial discovery; the UI treats
 * empty strings as "still loading".
 */
export interface BootstrapInfo {
  /** Control Plane base URL where SSO enrollment is served. */
  controlPlaneURL: string;
  deviceAuthMode: DeviceAuthMode | '';
}

/**
 * EnrollmentState captures everything the onboarding UI needs to pick
 * a flow: SSO sign-in or token paste. `pending=true` means the daemon
 * is running but has no device cert yet.
 */
export interface EnrollmentState {
  pending: boolean;
  bootstrap: BootstrapInfo;
}
