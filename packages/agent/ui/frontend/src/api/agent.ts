// Wails JS ↔ Go bridge wrapper.
//
// Wails generates TypeScript bindings under ../bindings/main/AgentBridge
// at build time. Importing from there directly works in production
// builds (inside the WebView). For Vite dev mode the bindings file
// may be stale; the runtime check below detects "Wails isn't here"
// and surfaces a NoBridgeError that the UI's "Agent not running"
// fallback renders.
//
// Every method returns the same JSON shape the daemon's statusapi
// produces (see packages/agent/internal/statusapi/server.go) — the
// bridge does not transform values, it just shuttles them.

declare global {
  // Wails injects a `go` namespace on window at runtime.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  interface Window { go?: any }
}

export class NoBridgeError extends Error {
  constructor() {
    super('Wails bridge unavailable — is the Dashboard launched as the bundled app?');
    this.name = 'NoBridgeError';
  }
}

function bridge() {
  const b = window.go?.main?.AgentBridge;
  if (!b) throw new NoBridgeError();
  return b;
}

export interface StatusSnapshot {
  state: 'active' | 'degraded' | 'error';
  stateReason: string;
  gatewayConnected: boolean;
  todayStats: {
    inspected: number;
    passthrough: number;
    denied: number;
    /** Null when no traffic today has phase data yet. */
    avgUsOverheadMs?: number | null;
    avgUpstreamTotalMs?: number | null;
  };
  recentEvents: Array<{ time: string; processName: string; destHost: string; action: string }>;
  policyRules: Array<{ domain: string; action: string }>;
  auditQueue: { unsyncedCount: number; lastSyncTime: string };
  agent: {
    version: string;
    updateAvailable: boolean;
    /**
     * Full URL the UpdateBanner opens for "Install update". Composed
     * daemon-side from the operator-configured `cpURL` in agent.yaml
     * + a platform-specific path suffix (`/downloads/NexusAgent-
     * latest.pkg` on macOS). Empty string when cpURL is unset — UI
     * hides the install button rather than open a broken link.
     */
    downloadURL: string;
    certExpiresAt: string;
    lastHeartbeat: string;
    heartbeatIntervalSec: number;
    deviceID: string;
    trustLevel: number;
    deviceAuthMode?: string;
    ssoEmail?: string;
  };
  dashboardURL: string;
  paused: boolean;
  pausedUntil?: string;
}

/**
 * SpillRef mirrors the daemon's audit.SpillRef — the out-of-band pointer for an
 * oversize body. `backend` is "localfs" (kept on the device; the daemon reads
 * it back into the inline body for the detail view) or "s3" (uploaded at drain;
 * the agent has no S3 GET credential, so the drawer shows a "view in Control
 * Plane" affordance instead of the bytes).
 */
export interface SpillRef {
  backend: string;
  key: string;
  size: number;
  sha256?: string;
  contentType?: string;
  truncated?: boolean;
}

/**
 * AgentEvent mirrors the daemon's QUERY_EVENTS IPC response. The daemon emits
 * every column; consumers pick which fields to render.
 */
export interface AgentEvent {
  id: string;
  timestamp?: string;
  sourceProcess?: string;
  /** Decoded mirror of `targetHost` from the daemon. */
  destHost?: string;
  /** Raw `targetHost` field name used by some daemon code paths. */
  targetHost?: string;
  destIp?: string;
  destPort?: number;
  /** HTTP method captured at flow_inspect / bridge time. */
  method?: string;
  /** HTTP path captured at flow_inspect / bridge time. */
  path?: string;
  action?: string;
  /** AI-only signals; empty / 0 on non-AI traffic. */
  providerName?: string;
  modelName?: string;
  apiKeyClass?: string;
  apiKeyFingerprint?: string;
  promptTokens?: number;
  completionTokens?: number;
  usageExtractionStatus?: string;
  /** Hook pipeline decision (allow/deny/inspect) when applicable. */
  hookDecision?: string;
  hookReason?: string;
  hookReasonCode?: string;
  /** TLS bump status (success / failed / exempt) for compliance-proxy origin. */
  bumpStatus?: string;
  /** Bytes counted by the agent's MITM layer; 0 when capture is off. */
  bytesIn?: number;
  bytesOut?: number;
  /** End-to-end latency ms — empty when the daemon couldn't compute it. */
  latencyMs?: number;
  /** Local enforcement policy that matched, if any. */
  policyRuleId?: string;
  complianceTags?: string[];
  /** Latency phase breakdown from the agent's MITM relay. Null for passthrough or inspect-fallback flows. */
  upstreamTtfbMs?: number | null;
  upstreamTotalMs?: number | null;
  requestHooksMs?: number | null;
  responseHooksMs?: number | null;
  /** Closed-key map (agent populates only `intercept_ms`). */
  latencyBreakdown?: Record<string, number> | null;
  /**
   * Per-hook execution trace. Backend serializes this from a Go
   * json.RawMessage field, so the wire shape is the RAW JSON value
   * (an array of HookRow), not a quoted string. Older versions sent
   * a string; render code accepts either.
   */
  hooksPipeline?: unknown;

  /**
   * `domainRuleId` is non-empty when the host matched an interception_domain row.
   * Empty / undefined means Untracked. `pathAction` is the resolved per-path or
   * default-domain action ("PROCESS" / "PASSTHROUGH" / "BLOCK"). Together they let
   * classify(event) distinguish Inspect (matched + PASSTHROUGH) from
   * Processed (matched + PROCESS + hooks ran APPROVE).
   */
  domainRuleId?: string;
  pathAction?: string;

  /**
   * Captured request/response bodies. Wire shape is base64-encoded
   * string (Go json.Marshal of []byte yields base64). Only populated on
   * the EVENT_BY_ID detail fetch (the list query omits bodies). Empty /
   * undefined when capture is off, the body was zero-length, or it spilled
   * to S3 (see {request,response}SpillRef). A localfs-spilled body is read
   * back off disk by the daemon and surfaces here inline.
   */
  payloadRequest?: string;
  payloadResponse?: string;

  /**
   * Out-of-band pointers for oversize bodies. Present on the detail fetch
   * when a body exceeded the inline cap. An s3-backend ref means the body
   * was uploaded at drain and is not locally readable — the drawer renders
   * a "view in Control Plane" affordance. A localfs ref is normally already
   * hydrated into payload{Request,Response} by the daemon.
   */
  requestSpillRef?: SpillRef;
  responseSpillRef?: SpillRef;

  /**
   * V2 (#58) — pre-normalized payload JSON, computed by forward_handler
   * after runtimeNormalize. Renders into the Normalized tab via
   * NormalizedPayloadView. Empty / undefined when no AI adapter
   * matched (non-LLM traffic, non-bumped flow) — UI shows banner.
   */
  normalizedRequest?: import('@/components/normalized/types').NormalizedPayload;
  normalizedResponse?: import('@/components/normalized/types').NormalizedPayload;
}

export interface EventPage {
  events: AgentEvent[];
  total: number;
  error?: string;
}

/**
 * EventDetail is the EVENT_BY_ID response — the full event (body + normalized
 * + spill refs) fetched on demand when the detail drawer opens, so the list
 * query stays lightweight. `event` is null when the id no longer exists.
 */
export interface EventDetail {
  event: AgentEvent | null;
  error?: string;
}

/**
 * LifecycleEvent describes what the daemon DID (startup, shutdown, paused,
 * resumed, sso_login, sso_logout) rather than what it OBSERVED on the network.
 * Distinct from AgentEvent which carries per-connection decisions.
 */
export interface LifecycleEvent {
  id: string;
  /** RFC3339 timestamp of when the lifecycle action happened on this device. */
  occurredAt: string;
  /** Stable wire identifier — one of agent.{startup,shutdown,paused,resumed,sso_login,sso_logout}. */
  action: string;
  /** Human-readable summary the daemon stamped on the event ("agent started" etc.). */
  message: string;
  /** Always "info" today; reserved for future per-action level escalation. */
  level: string;
  /** Free-form per-action data: { reason: "user_quit_flag" } for shutdown, { durationSec: 900 } for paused, etc. */
  attrs?: Record<string, unknown>;
}

export interface LifecycleEventPage {
  events: LifecycleEvent[];
  total: number;
  error?: string;
}

/**
 * AppliedConfig is returned by the GET_APPLIED_CONFIG IPC and rendered
 * as the Policies page's section cards. Each array field may be empty when
 * the admin has not pushed config yet.
 */
export interface AppliedConfig {
  sync: PolicySyncStatus;
  interceptionDomains: PolicyInterceptionDomain[];
  hooks: PolicyHook[];
  exemptions: PolicyExemption[];
  deviceDefaults: PolicyDeviceDefaults;
  killSwitch: PolicyKillSwitch;
  rulePacks: PolicyRulePack[];
  /** Current user assigned to this device — sourced from DeviceAssignment + NexusUser. */
  userContext?: PolicyUserContext;
  /** Root → current-org chain (materialized path), one node per ancestor. */
  organizationTree?: PolicyOrganization[];
  diagMode?: PolicyDiagMode;
}

export interface PolicySyncStatus {
  desiredVersion: number;
  reportedVersion: number;
  /** True when desiredVersion == reportedVersion — every admin change is now applied locally. */
  inSync: boolean;
  /** RFC3339 timestamp of the most recent successful shadow_report; empty when never reported. */
  lastReportedAt: string;
}

export interface PolicyInterceptionDomain {
  id: string;
  name: string;
  hostPattern: string;
  hostMatchType?: string;
  adapterId?: string;
  enabled: boolean;
  priority?: number;
  defaultPathAction?: string;
  onAdapterError?: string;
  networkZone?: string;
  paths?: PolicyInterceptionPath[];
}

export interface PolicyInterceptionPath {
  id: string;
  pathPattern: string[];
  matchType?: string;
  action?: string;
  priority?: number;
  enabled: boolean;
}

export interface PolicyHook {
  id: string;
  name: string;
  implementationId?: string;
  /** Execution stage: preInbound / preOutbound / postOutbound / etc. */
  stage?: string;
  priority?: number;
  failBehavior?: string;
  timeoutMs?: number;
  applicableIngress?: string[];
  /** Raw per-hook config payload — kept as opaque JSON for the detail view to pretty-print. */
  config?: unknown;
  enabled: boolean;
}

export interface PolicyExemption {
  id: string;
  host?: string;
  user?: string;
  reason?: string;
}

export interface PolicyDeviceDefaults {
  quitAllowed?: boolean;
  heartbeatIntervalSec?: number;
  auditDrainIntervalSec?: number;
  configSyncIntervalSec?: number;
  auditBatchSize?: number;
  shutdownWarningEnabled?: boolean;
  shutdownWarning?: Record<string, string>;
  autoUpdateEnabled?: boolean;
  autoUpdateChannel?: string;
  logLevel?: string;
  trafficUploadLevel?: string;
  /**
   * Theme pack ID pushed by the admin. When non-empty, ThemeProvider treats
   * this as authoritative over the user's localStorage pick. Unknown IDs fall
   * back to the bundled `default` theme.
   */
  themeId?: string;
  /**
   * macOS-only: bundle IDs the NE proxy closes UDP for, forcing QUIC → TCP
   * fallback so the agent's TLS-bump path can inspect traffic.
   * Managed in the Control Plane (Settings → Agent → QUIC fallback bundles).
   * Empty / absent = no UDP is closed.
   */
  forceQUICFallbackBundles?: string[];
}

export interface PolicyKillSwitch {
  /**
   * `true` = kill switch ENGAGED (admin paused interception → passthrough, no
   * TLS-bumping); `false` = normal operation (bumping active). Matches the
   * backend `interception.Killswitch{engaged}` wire field — the engaged state
   * is the danger state.
   *
   * NOTE: this field used to be `enabled` with the OPPOSITE meaning. The
   * backend renamed it to `engaged` (no inversion); the frontend was missed in
   * that rename, so `killSwitch.enabled` was always `undefined` and `!enabled`
   * was always true — the Dashboard showed "Kill switch engaged" on every
   * device regardless of the real state. Now read `engaged` directly.
   */
  engaged: boolean;
  reason?: string;
}

export interface PolicyRulePack {
  id: string;
  packId?: string;
  name: string;
  version?: string;
  maintainer?: string;
  description?: string;
  boundHookId?: string;
  enabled: boolean;
  ruleCount: number;
  installedAt?: string;
  rules?: PolicyRule[];
}

export interface PolicyRule {
  id: string;
  ruleId?: string;
  category?: string;
  severity?: string;
  pattern?: string;
  flags?: string;
  description?: string;
  labels?: string[];
}

export interface PolicyUserContext {
  id: string;
  displayName: string;
  email?: string;
  status?: string;
  source?: string;
  organizationId: string;
}

export interface PolicyOrganization {
  id: string;
  name: string;
  code: string;
  parentId?: string;
  path: string;
  description?: string;
  timezone?: string;
}

export interface PolicyDiagMode {
  active: boolean;
  /** RFC3339 expiry of the diag-mode window. */
  until?: string;
}

/**
 * StatsRow mirrors the daemon's QueryStatsRow over the wire.
 * thingId is omitted — the agent has only one Thing so identity is implicit.
 */
export interface StatsRow {
  bucketStart: string;
  metricName: string;
  dimensionKey?: string;
  subDimension?: string;
  value: number;
  metadata?: unknown;
}

export interface StatsResponse {
  startTime: string;
  endTime: string;
  /** Server-chosen rollup granule: "5m" | "1h" | "1d" | "1mo". */
  granule: string;
  rows: StatsRow[];
  /** Present only when the daemon rejected the query. */
  error?: string;
}

export interface StatsFilter {
  /** RFC3339; empty = end - 24h. */
  start?: string;
  /** RFC3339; empty = now. */
  end?: string;
  /** Exact metric-name filter. Empty array = no filter. */
  metrics?: string[];
  /** Dimension name (e.g. "target_host" / "source_process" / "action"); empty = global rows only. */
  dimension?: string;
  /** Exact subDimension match — rarely used from UI; mainly for cross-link drilldowns. */
  subDimension?: string;
}

export interface Diagnostics {
  hubReachable: boolean;
  certPath: string;
  logTail: string[];
  /** Active kernel/userspace interception mechanism. One of
   *  "NETransparentProxy" (macOS), "iptables" (Linux), "WinDivert"
   *  (Windows kernel driver loaded), or "SystemProxyFallback"
   *  (Windows degraded fallback). Empty when the platform shim
   *  predates the reporter interface. */
  interceptionMode?:
    | 'NETransparentProxy'
    | 'iptables'
    | 'WinDivert'
    | 'SystemProxyFallback'
    | string;
  error?: string;
}

export const agentApi = {
  getStatus: () => bridge().GetStatus() as Promise<StatusSnapshot>,
  queryEvents: (filter: {
    search: string;
    action: string;
    offset: number;
    limit: number;
    // #88 — backend now supports AI-only + Since filters; UI passes
    // them so server-side SQL does the narrowing (was: client-side
    // over-fetch + JS filter with broken pagination + wrong total).
    aiOnly?: boolean;
    sinceUnixMillis?: number;
  }) => bridge().QueryEvents(filter) as Promise<EventPage>,
  // Detail-by-id: the drawer fetches body + normalized + spill on open so the
  // list query (queryEvents) stays metadata-only.
  eventById: (id: string) => bridge().EventByID(id) as Promise<EventDetail>,
  queryLifecycle: (filter: { offset: number; limit: number }) =>
    bridge().QueryLifecycleEvents(filter) as Promise<LifecycleEventPage>,
  getAppliedConfig: () => bridge().GetAppliedConfig() as Promise<AppliedConfig>,
  refreshPolicies: () =>
    bridge().RefreshPolicies() as Promise<{ ok: boolean; error?: string }>,
  restartDaemon: () =>
    bridge().RestartDaemon() as Promise<{ acknowledged: boolean; error?: string }>,
  pauseProtection: (seconds: number) =>
    bridge().PauseProtection(seconds) as Promise<{ paused: boolean; resumes_at?: string; error?: string }>,
  resumeProtection: () =>
    bridge().ResumeProtection() as Promise<{ paused: boolean; error?: string }>,
  checkUpdate: () =>
    bridge().CheckUpdate() as Promise<{ available: boolean; version?: string; error?: string }>,
  getDiagnostics: () => bridge().GetDiagnostics() as Promise<Diagnostics>,
  enrollWithToken: (token: string) =>
    bridge().EnrollWithToken(token) as Promise<{ success: boolean; device_id?: string; error?: string }>,
  authenticateSSO: () =>
    bridge().AuthenticateSSO() as Promise<{
      success?: boolean;
      confirmation_required?: boolean;
      device_id?: string;
      message?: string;
      email?: string;
      error?: string;
    }>,
  authenticateConfirm: () =>
    bridge().AuthenticateConfirm() as Promise<{
      success?: boolean;
      device_id?: string;
      email?: string;
      error?: string;
    }>,
  authenticateCancel: () =>
    bridge().AuthenticateCancel() as Promise<{ acknowledged?: boolean }>,
  unenroll: () =>
    bridge().Unenroll() as Promise<{ acknowledged: boolean; error?: string }>,
  openBrowser: (url: string) =>
    bridge().OpenBrowser(url) as Promise<{ opened: boolean; error?: string }>,
  queryStats: (filter: StatsFilter) =>
    bridge().QueryStats({
      start: filter.start ?? '',
      end: filter.end ?? '',
      metrics: filter.metrics ?? [],
      dimension: filter.dimension ?? '',
      subDimension: filter.subDimension ?? '',
    }) as Promise<StatsResponse>,
};
