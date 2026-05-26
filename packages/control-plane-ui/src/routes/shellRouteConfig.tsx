/**
 * Single source for authenticated shell routes and sidebar nav metadata.
 * App renders routes from this list; Sidebar builds sections via buildSidebarNavSections().
 *
 * Section taxonomy (domain-driven):
 *   overview      — Dashboard, Traffic, Analytics, Quota Usage, Cache ROI
 *   aiGateway     — Providers, Credentials, Credential Reliability, Routing, Virtual Keys,
 *                   Quota Policies, Quota Overrides, Prompt Cache
 *   compliance    — Overview, Hooks, Rule Packs, Interception Domains, Exemptions,
 *                   AI Guard Backend, Streaming Compliance, Payload Capture,
 *                   Operation Logs, Data Subject Requests
 *   alerts        — Inbox, Rules, Channels
 *   devices       — Devices, Device Groups, Device Auth, Agent Defaults
 *   infrastructure— Nodes, Config Sync, Overrides, Scheduled Jobs, Recent Errors,
 *                   Crash Reports, Diag Mode, Observability, Observability Retention,
 *                   SIEM, Proxy Rollout, Agent Setup, Kill Switch
 *   iam           — Organizations, Projects, Users, Roles, Policies, Simulator,
 *                   Identity Provider
 *   system        — AI Gateway Simulator, Status & Health, Setup Wizard
 *
 * Pre-GA policy (CLAUDE.md): no backward-compat redirect routes. When a path
 * moves or renames, the old path is removed in the same commit.
 */
import type { ComponentType, LazyExoticComponent } from 'react';
import * as L from './lazyPages';

export type NavSectionKey =
  | 'overview' | 'aiGateway' | 'compliance' | 'alerts' | 'devices'
  | 'infrastructure' | 'iam' | 'system' | 'settings';

export const NAV_SECTION_META: Record<
  NavSectionKey,
  { titleKey: string; collapsible: boolean; defaultOpen?: boolean }
> = {
  overview:       { titleKey: 'overview',       collapsible: false },
  aiGateway:      { titleKey: 'aiGateway',      collapsible: true, defaultOpen: true },
  compliance:     { titleKey: 'compliance',     collapsible: true, defaultOpen: true },
  alerts:         { titleKey: 'alerts',         collapsible: true, defaultOpen: true },
  devices:        { titleKey: 'devicesSection', collapsible: true, defaultOpen: true },
  infrastructure: { titleKey: 'infrastructure', collapsible: true, defaultOpen: true },
  iam:            { titleKey: 'iam',            collapsible: true, defaultOpen: true },
  system:         { titleKey: 'system',         collapsible: true, defaultOpen: true },
  // Fleet-wide singleton settings (embedding config, etc.).
  settings:       { titleKey: 'settingsSection', collapsible: true, defaultOpen: true },
};

export interface ShellNavItem {
  labelKey: string;
  path: string;
  /** IAM action(s) — nav item visible when any action is in permissions[]. */
  allowedActions?: string[];
  /** Regex sources whose match against the URL keeps this item active. */
  relatedPaths?: string[];
}

export interface ShellNavSection {
  titleKey: string;
  collapsible: boolean;
  defaultOpen?: boolean;
  items: ShellNavItem[];
}

export interface ShellRouteConfig {
  path?: string;
  index?: boolean;
  LazyPage: LazyExoticComponent<ComponentType<object>>;
  /** IAM action(s) — route is accessible when any action is in permissions[]. */
  allowedActions?: string[];
  nav?: {
    sectionKey: NavSectionKey;
    labelKey: string;
    to: string;
    /** IAM action(s) — nav item visible when any action is in permissions[]. */
    allowedActions?: string[];
    /** Sort order within the section (lower first). */
    order: number;
    /**
     * Additional pathname patterns that should keep this nav item active.
     * Each entry is a regex source matched against the full pathname (no
     * leading ^ / trailing $ added — write them in the pattern). Use when
     * the landing page's drill-down lives elsewhere (e.g. proxy-rollout →
     * /nodes/:id/setup).
     */
    relatedPaths?: string[];
  };
}

export const SHELL_ROUTES: ShellRouteConfig[] = [
  // ── Overview ──
  { index: true, LazyPage: L.LazyDashboardPage, nav: { sectionKey: 'overview', labelKey: 'dashboard', to: '/', order: 0 } },
  {
    path: 'traffic',
    LazyPage: L.LazyTrafficAnalyticsPage,
    allowedActions: ['admin:traffic-log.read'],
    nav: { sectionKey: 'overview', labelKey: 'traffic', to: '/traffic', allowedActions: ['admin:traffic-log.read'], order: 1 },
  },
  {
    path: 'analytics',
    LazyPage: L.LazyAnalyticsPage,
    allowedActions: ['admin:analytics.read'],
    nav: { sectionKey: 'overview', labelKey: 'analytics', to: '/analytics', allowedActions: ['admin:analytics.read'], order: 2 },
  },
  {
    path: 'quota-usage',
    LazyPage: L.LazyQuotaUsageDashboard,
    allowedActions: ['admin:quota-analytics.read'],
    nav: { sectionKey: 'overview', labelKey: 'quotaUsage', to: '/quota-usage', allowedActions: ['admin:quota-analytics.read'], order: 3 },
  },
  {
    path: 'cache-roi',
    LazyPage: L.LazyCacheROIDashboard,
    allowedActions: ['admin:analytics.read'],
    nav: { sectionKey: 'overview', labelKey: 'cacheRoi', to: '/cache-roi', allowedActions: ['admin:analytics.read'], order: 4 },
  },

  // ── AI Gateway ──
  // Setup-first ordering: Providers → Credentials → Credential Reliability →
  // Routing → Virtual Keys → Quota Policies → Quota Overrides → Prompt Cache.
  // Every nav entry carries allowedActions so principals without the list
  // permission don't see the menu item (otherwise it would lead to a 403
  // empty list — the Frank case).
  {
    path: 'ai-gateway/providers',
    LazyPage: L.LazyConfigProvidersPage,
    allowedActions: ['admin:provider.read'],
    nav: { sectionKey: 'aiGateway', labelKey: 'providersModels', to: '/ai-gateway/providers', allowedActions: ['admin:provider.read'], order: 0 },
  },
  { path: 'ai-gateway/providers/new', LazyPage: L.LazyProviderWizard, allowedActions: ['admin:provider.create'] },
  { path: 'ai-gateway/providers/:id', LazyPage: L.LazyProviderDetail, allowedActions: ['admin:provider.read'] },
  {
    path: 'ai-gateway/credentials',
    LazyPage: L.LazyCredentialListPage,
    allowedActions: ['admin:credential.read'],
    nav: { sectionKey: 'aiGateway', labelKey: 'credentials', to: '/ai-gateway/credentials', allowedActions: ['admin:credential.read'], order: 1 },
  },
  { path: 'ai-gateway/credentials/new', LazyPage: L.LazyCredentialCreate, allowedActions: ['admin:credential.create'] },
  { path: 'ai-gateway/credentials/:id', LazyPage: L.LazyCredentialDetail, allowedActions: ['admin:credential.read'] },
  {
    path: 'ai-gateway/credential-reliability',
    LazyPage: L.LazyCredentialReliabilitySettingsPage,
    allowedActions: ['admin:credential.read'],
    nav: { sectionKey: 'aiGateway', labelKey: 'credentialReliability', to: '/ai-gateway/credential-reliability', allowedActions: ['admin:credential.read'], order: 2 },
  },
  {
    path: 'ai-gateway/routing',
    LazyPage: L.LazyConfigRoutingPage,
    allowedActions: ['admin:routing-rule.read'],
    nav: { sectionKey: 'aiGateway', labelKey: 'routingRules', to: '/ai-gateway/routing', allowedActions: ['admin:routing-rule.read'], order: 3 },
  },
  { path: 'ai-gateway/routing/new', LazyPage: L.LazyRoutingRuleCreate, allowedActions: ['admin:routing-rule.create'] },
  { path: 'ai-gateway/routing/:id', LazyPage: L.LazyRoutingRuleDetail, allowedActions: ['admin:routing-rule.update'] },
  {
    path: 'ai-gateway/virtual-keys',
    LazyPage: L.LazyVirtualKeyListPage,
    allowedActions: ['admin:virtual-key.read'],
    nav: { sectionKey: 'aiGateway', labelKey: 'virtualKeys', to: '/ai-gateway/virtual-keys', allowedActions: ['admin:virtual-key.read'], order: 4 },
  },
  { path: 'ai-gateway/virtual-keys/new', LazyPage: L.LazyVirtualKeyCreate, allowedActions: ['admin:virtual-key.create'] },
  { path: 'ai-gateway/virtual-keys/:id', LazyPage: L.LazyVirtualKeyDetail, allowedActions: ['admin:virtual-key.read'] },
  {
    path: 'ai-gateway/quota-policies',
    LazyPage: L.LazyQuotaPolicyListPage,
    allowedActions: ['admin:quota-policy.read'],
    nav: { sectionKey: 'aiGateway', labelKey: 'quotaPolicies', to: '/ai-gateway/quota-policies', allowedActions: ['admin:quota-policy.read'], order: 5 },
  },
  { path: 'ai-gateway/quota-policies/new', LazyPage: L.LazyQuotaPolicyCreate, allowedActions: ['admin:quota-policy.create'] },
  { path: 'ai-gateway/quota-policies/:id', LazyPage: L.LazyQuotaPolicyDetail, allowedActions: ['admin:quota-policy.read'] },
  { path: 'ai-gateway/quota-policies/:id/edit', LazyPage: L.LazyQuotaPolicyEdit, allowedActions: ['admin:quota-policy.update'] },
  {
    path: 'ai-gateway/quota-overrides',
    LazyPage: L.LazyQuotaOverrideListPage,
    allowedActions: ['admin:quota-override.read'],
    nav: { sectionKey: 'aiGateway', labelKey: 'quotaOverrides', to: '/ai-gateway/quota-overrides', allowedActions: ['admin:quota-override.read'], order: 6 },
  },
  { path: 'ai-gateway/quota-overrides/new', LazyPage: L.LazyQuotaOverrideCreate, allowedActions: ['admin:quota-override.create'] },
  { path: 'ai-gateway/quota-overrides/:id', LazyPage: L.LazyQuotaOverrideDetail, allowedActions: ['admin:quota-override.read'] },
  { path: 'ai-gateway/quota-overrides/:id/edit', LazyPage: L.LazyQuotaOverrideEdit, allowedActions: ['admin:quota-override.update'] },
  {
    // Cache — single fleet-wide configuration page merging Prompt Cache +
    // Cache Embedding. Page-level read is the union (either action grants
    // visibility); each section gates its own save action.
    path: 'ai-gateway/cache',
    LazyPage: L.LazyCachePage,
    allowedActions: ['admin:prompt-cache.read', 'admin:semantic-cache.read'],
    nav: {
      sectionKey: 'aiGateway',
      labelKey: 'cache',
      to: '/ai-gateway/cache',
      allowedActions: ['admin:prompt-cache.read', 'admin:semantic-cache.read'],
      order: 7,
    },
  },
  {
    // Emergency Passthrough — incident-response kill-switch that bypasses
    // hooks / cache / normalize at any of 3 tiers. Read is wide (every
    // Provider / Compliance / Incident Response admin can inspect); enabling
    // requires the emergency-enable action gated to NexusIncidentResponse
    // + super-admin.
    path: 'ai-gateway/passthrough',
    LazyPage: L.LazyPassthroughPage,
    allowedActions: ['admin:passthrough.read'],
    nav: { sectionKey: 'aiGateway', labelKey: 'passthrough', to: '/ai-gateway/passthrough', allowedActions: ['admin:passthrough.read'], order: 9 },
  },

  // ── Compliance ──
  // Order: Overview → Hooks → Rule Packs → Scope (Domains, Exemptions) →
  // Scanning backends (AI Guard, Streaming) → Data retention (Payload
  // Capture) → Audit + privacy (Logs, DSAR).
  {
    path: 'compliance/overview',
    LazyPage: L.LazyComplianceDashboardPage,
    allowedActions: ['admin:compliance-report.read'],
    nav: { sectionKey: 'compliance', labelKey: 'complianceOverview', to: '/compliance/overview', allowedActions: ['admin:compliance-report.read'], order: 0 },
  },
  {
    path: 'compliance/hooks',
    LazyPage: L.LazyConfigHooksPage,
    allowedActions: ['admin:hook.read'],
    nav: { sectionKey: 'compliance', labelKey: 'hooksPolicies', to: '/compliance/hooks', allowedActions: ['admin:hook.read'], order: 1 },
  },
  { path: 'compliance/hooks/new', LazyPage: L.LazyHookCreate, allowedActions: ['admin:hook.create'] },
  { path: 'compliance/hooks/:id', LazyPage: L.LazyHookDetail, allowedActions: ['admin:hook.update'] },
  {
    path: 'compliance/rule-packs',
    LazyPage: L.LazyRulePackListPage,
    allowedActions: ['admin:rule-pack.read'],
    nav: { sectionKey: 'compliance', labelKey: 'rulePacks', to: '/compliance/rule-packs', allowedActions: ['admin:rule-pack.read'], order: 2 },
  },
  { path: 'compliance/rule-packs/create', LazyPage: L.LazyRulePackCreatePage, allowedActions: ['admin:rule-pack.create'] },
  { path: 'compliance/rule-packs/:id/edit', LazyPage: L.LazyRulePackEditPage, allowedActions: ['admin:rule-pack.update'] },
  { path: 'compliance/rule-packs/:id', LazyPage: L.LazyRulePackDetailPage, allowedActions: ['admin:rule-pack.read'] },
  {
    path: 'compliance/interception-domains',
    LazyPage: L.LazyInterceptionDomainsPage,
    allowedActions: ['admin:interception-domain.read'],
    nav: { sectionKey: 'compliance', labelKey: 'interceptionDomains', to: '/compliance/interception-domains', allowedActions: ['admin:interception-domain.read'], order: 3 },
  },
  { path: 'compliance/interception-domains/:id', LazyPage: L.LazyInterceptionDomainDetailPage, allowedActions: ['admin:interception-domain.read'] },
  {
    path: 'compliance/exemptions',
    LazyPage: L.LazyComplianceExemptionsPage,
    allowedActions: ['admin:compliance-exemption.read'],
    nav: { sectionKey: 'compliance', labelKey: 'exemptions', to: '/compliance/exemptions', allowedActions: ['admin:compliance-exemption.read'], order: 4 },
  },
  { path: 'compliance/exemptions/:id', LazyPage: L.LazyComplianceExemptionDetailPage, allowedActions: ['admin:compliance-exemption.read'] },
  {
    path: 'compliance/ai-guard',
    LazyPage: L.LazyAIGuardPage,
    allowedActions: ['admin:ai-guard-config.read'],
    nav: { sectionKey: 'compliance', labelKey: 'aiGuardBackend', to: '/compliance/ai-guard', allowedActions: ['admin:ai-guard-config.read'], order: 5 },
  },
  {
    path: 'compliance/streaming',
    LazyPage: L.LazyStreamingComplianceSettingsPage,
    allowedActions: ['admin:settings.read'],
    nav: { sectionKey: 'compliance', labelKey: 'streamingCompliance', to: '/compliance/streaming', allowedActions: ['admin:settings.read'], order: 6 },
  },
  {
    // Moved from aiGateway — payload capture controls what request/response
    // bytes are persisted across ai-gateway, compliance-proxy, AND agent.
    // The decision is fundamentally a data-retention / privacy compliance
    // question, not an AI-gateway-specific feature.
    path: 'compliance/payload-capture',
    LazyPage: L.LazyPayloadCaptureSettingsPage,
    allowedActions: ['admin:payload-capture.read'],
    nav: { sectionKey: 'compliance', labelKey: 'payloadCapture', to: '/compliance/payload-capture', allowedActions: ['admin:payload-capture.read'], order: 7 },
  },
  {
    path: 'compliance/audit-logs',
    LazyPage: L.LazyAuditLogPage,
    allowedActions: ['admin:audit-log.read'],
    nav: { sectionKey: 'compliance', labelKey: 'operationLogs', to: '/compliance/audit-logs', allowedActions: ['admin:audit-log.read'], order: 8 },
  },
  {
    path: 'compliance/dsar',
    LazyPage: L.LazyDSARPage,
    allowedActions: ['admin:dsar.read'],
    nav: { sectionKey: 'compliance', labelKey: 'dsar', to: '/compliance/dsar', allowedActions: ['admin:dsar.read'], order: 9 },
  },
  { path: 'compliance/compliance-report', LazyPage: L.LazyComplianceReportPage, allowedActions: ['admin:compliance-report.read'] },
  // Forward-proxy operational sub-pages (no nav — accessed from Compliance Overview).
  { path: 'proxy/status', LazyPage: L.LazyProxyStatusCompliancePage, allowedActions: ['admin:settings.read'] },
  { path: 'proxy/reject-config', LazyPage: L.LazyRejectConfigPage, allowedActions: ['admin:settings.read'] },

  // ── Alerts ──
  {
    path: 'alerts',
    LazyPage: L.LazyAlertListPage,
    allowedActions: ['admin:alert.read'],
    nav: { sectionKey: 'alerts', labelKey: 'alertsInbox', to: '/alerts', allowedActions: ['admin:alert.read'], order: 0 },
  },
  {
    path: 'alerts/rules',
    LazyPage: L.LazyAlertRulesListPage,
    allowedActions: ['admin:alert.update'],
    nav: { sectionKey: 'alerts', labelKey: 'alertsRules', to: '/alerts/rules', allowedActions: ['admin:alert.update'], order: 1 },
  },
  { path: 'alerts/rules/:id', LazyPage: L.LazyAlertRuleEditPage, allowedActions: ['admin:alert.update'] },
  {
    path: 'alerts/channels',
    LazyPage: L.LazyAlertChannelsListPage,
    allowedActions: ['admin:alert.update'],
    nav: { sectionKey: 'alerts', labelKey: 'alertsChannels', to: '/alerts/channels', allowedActions: ['admin:alert.update'], order: 2 },
  },
  { path: 'alerts/channels/new', LazyPage: L.LazyAlertChannelEditPage, allowedActions: ['admin:alert.update'] },
  { path: 'alerts/channels/:id', LazyPage: L.LazyAlertChannelEditPage, allowedActions: ['admin:alert.update'] },

  // ── Devices ──
  // Order: Devices → Device Groups → Device Auth (foundational) → Agent
  // Defaults (renamed from "Agent Settings" — it's the agent runtime
  // defaults page, distinct from infrastructure/agent-setup which is the
  // install wizard).
  { path: 'fleet-overview', LazyPage: L.LazyFleetOverviewPage, allowedActions: ['admin:agent-device.read'] },
  {
    path: 'devices',
    LazyPage: L.LazyDeviceListPage,
    allowedActions: ['admin:agent-device.read'],
    nav: { sectionKey: 'devices', labelKey: 'devices', to: '/devices', allowedActions: ['admin:agent-device.read'], order: 0 },
  },
  {
    path: 'devices/groups',
    LazyPage: L.LazyDeviceGroupListPage,
    allowedActions: ['admin:device-group.read'],
    nav: { sectionKey: 'devices', labelKey: 'deviceGroups', to: '/devices/groups', allowedActions: ['admin:device-group.read'], order: 1 },
  },
  { path: 'devices/groups/new', LazyPage: L.LazyDeviceGroupCreatePage, allowedActions: ['admin:device-group.create'] },
  { path: 'devices/groups/:id', LazyPage: L.LazyDeviceGroupDetailPage, allowedActions: ['admin:device-group.read'] },
  {
    path: 'devices/device-auth',
    LazyPage: L.LazyDeviceAuthSettingsPage,
    allowedActions: ['admin:settings.update'],
    nav: { sectionKey: 'devices', labelKey: 'deviceAuth', to: '/devices/device-auth', allowedActions: ['admin:settings.update'], order: 2 },
  },
  {
    path: 'devices/device-defaults',
    LazyPage: L.LazyAgentSettingsPage,
    allowedActions: ['admin:device-defaults.read'],
    nav: { sectionKey: 'devices', labelKey: 'deviceDefaults', to: '/devices/device-defaults', allowedActions: ['admin:device-defaults.read'], order: 3 },
  },

  // ── Infrastructure ──
  // Order: cluster state first, then ops history, then diagnostics, then
  // observability config, then the one-time setup wizards, with Kill Switch
  // pushed to the bottom (red emergency button — visible but not the
  // first thing under a misclick).
  { path: 'infrastructure/nodes', LazyPage: L.LazyInfraNodesPage, allowedActions: ['admin:node.read'], nav: { sectionKey: 'infrastructure', labelKey: 'nodes', to: '/infrastructure/nodes', allowedActions: ['admin:node.read'], order: 0 } },
  { path: 'infrastructure/nodes/:id', LazyPage: L.LazyInfraNodeDetailPage, allowedActions: ['admin:node.read'] },
  { path: 'infrastructure/nodes/:id/setup', LazyPage: L.LazyInfraProxySetupPage, allowedActions: ['admin:node.read'] },
  { path: 'infrastructure/config-sync', LazyPage: L.LazyInfraConfigSyncPage, allowedActions: ['admin:settings.read'], nav: { sectionKey: 'infrastructure', labelKey: 'configSync', to: '/infrastructure/config-sync', allowedActions: ['admin:settings.read'], order: 1 } },
  { path: 'infrastructure/overrides', LazyPage: L.LazyInfraOverridesPage, allowedActions: ['admin:node.write-override'], nav: { sectionKey: 'infrastructure', labelKey: 'overrides', to: '/infrastructure/overrides', allowedActions: ['admin:node.write-override'], order: 2 } },
  { path: 'infrastructure/jobs', LazyPage: L.LazyInfraJobsPage, allowedActions: ['admin:settings.read'], nav: { sectionKey: 'infrastructure', labelKey: 'scheduledJobs', to: '/infrastructure/jobs', allowedActions: ['admin:settings.read'], order: 3 } },
  { path: 'infrastructure/jobs/:id', LazyPage: L.LazyInfraJobDetailPage, allowedActions: ['admin:settings.read'] },
  { path: 'infrastructure/errors', LazyPage: L.LazyInfraRecentErrorsPage, allowedActions: ['admin:settings.read'], nav: { sectionKey: 'infrastructure', labelKey: 'recentErrors', to: '/infrastructure/errors', allowedActions: ['admin:settings.read'], order: 4 } },
  { path: 'infrastructure/dlq', LazyPage: L.LazyInfraDlqPage, allowedActions: ['admin:observability-dlq.read'], nav: { sectionKey: 'infrastructure', labelKey: 'dlq', to: '/infrastructure/dlq', allowedActions: ['admin:observability-dlq.read'], order: 4.5 } },
  { path: 'infrastructure/crashes', LazyPage: L.LazyInfraCrashReportsPage, allowedActions: ['admin:settings.read'], nav: { sectionKey: 'infrastructure', labelKey: 'crashReports', to: '/infrastructure/crashes', allowedActions: ['admin:settings.read'], order: 5 } },
  { path: 'infrastructure/diag-mode', LazyPage: L.LazyInfraDiagModePage, allowedActions: ['admin:diagnostic-mode.read'], nav: { sectionKey: 'infrastructure', labelKey: 'diagMode', to: '/infrastructure/diag-mode', allowedActions: ['admin:diagnostic-mode.read'], order: 6 } },
  { path: 'infrastructure/observability-config', LazyPage: L.LazyObservabilityConfigPage, allowedActions: ['admin:observability.read'], nav: { sectionKey: 'infrastructure', labelKey: 'observabilityConfig', to: '/infrastructure/observability-config', allowedActions: ['admin:observability.read'], order: 7 } },
  { path: 'infrastructure/observability-retention', LazyPage: L.LazyObservabilityRetentionPage, allowedActions: ['admin:observability.read'], nav: { sectionKey: 'infrastructure', labelKey: 'observabilityRetention', to: '/infrastructure/observability-retention', allowedActions: ['admin:observability.read'], order: 8 } },
  { path: 'infrastructure/siem', LazyPage: L.LazySiemSettingsPage, allowedActions: ['admin:audit-log.read'], nav: { sectionKey: 'infrastructure', labelKey: 'siem', to: '/infrastructure/siem', allowedActions: ['admin:audit-log.read'], order: 9 } },
  // proxy-rollout is the landing list; clicking "Configure" navigates to
  // /infrastructure/nodes/:id/setup, which is the same conceptual flow.
  // relatedPaths keeps the nav highlight on the rollout entry while the
  // operator is in the per-node setup drill-down.
  { path: 'infrastructure/proxy-rollout', LazyPage: L.LazyProxySetupPage, allowedActions: ['admin:settings.read'], nav: { sectionKey: 'infrastructure', labelKey: 'proxySetup', to: '/infrastructure/proxy-rollout', allowedActions: ['admin:settings.read'], order: 10, relatedPaths: ['^/infrastructure/nodes/[^/]+/setup$'] } },
  { path: 'infrastructure/agent-setup', LazyPage: L.LazyInfraAgentSetupPage, allowedActions: ['admin:settings.read'], nav: { sectionKey: 'infrastructure', labelKey: 'agentSetup', to: '/infrastructure/agent-setup', allowedActions: ['admin:settings.read'], order: 11 } },
  { path: 'infrastructure/kill-switch', LazyPage: L.LazyInfraKillSwitchPage, allowedActions: ['admin:kill-switch.toggle'], nav: { sectionKey: 'infrastructure', labelKey: 'killSwitch', to: '/infrastructure/kill-switch', allowedActions: ['admin:kill-switch.toggle'], order: 12 } },

  // ── IAM ──
  // Order: tenant hierarchy (Org → Project) → principals (Users) →
  // permissions (Roles → Policies → Simulator) → identity source (IdP).
  // The legacy "Authentication" route is gone — its only content was the
  // device-auth form, which now lives at /devices/device-auth.
  {
    path: 'iam/organizations',
    LazyPage: L.LazyOrganizationList,
    allowedActions: ['admin:organization.read'],
    nav: { sectionKey: 'iam', labelKey: 'organizations', to: '/iam/organizations', allowedActions: ['admin:organization.read'], order: 0 },
  },
  { path: 'iam/organizations/new', LazyPage: L.LazyOrganizationCreate, allowedActions: ['admin:organization.create'] },
  { path: 'iam/organizations/:id', LazyPage: L.LazyOrganizationDetail, allowedActions: ['admin:organization.read'] },
  {
    path: 'iam/projects',
    LazyPage: L.LazyProjectList,
    allowedActions: ['admin:project.read'],
    nav: { sectionKey: 'iam', labelKey: 'projects', to: '/iam/projects', allowedActions: ['admin:project.read'], order: 1 },
  },
  { path: 'iam/projects/new', LazyPage: L.LazyProjectCreate, allowedActions: ['admin:project.create'] },
  { path: 'iam/projects/:id', LazyPage: L.LazyProjectDetail, allowedActions: ['admin:project.read'] },
  {
    path: 'iam/users',
    LazyPage: L.LazyIamUserListPage,
    allowedActions: ['admin:user.read'],
    nav: { sectionKey: 'iam', labelKey: 'users', to: '/iam/users', allowedActions: ['admin:user.read'], order: 2 },
  },
  { path: 'iam/users/new', LazyPage: L.LazyIamUserCreatePage, allowedActions: ['admin:user.create'] },
  { path: 'iam/users/:id', LazyPage: L.LazyIamUserDetailPage, allowedActions: ['admin:user.read'] },
  {
    path: 'iam/roles',
    LazyPage: L.LazyIamRoleListPage,
    allowedActions: ['admin:iam-group.read'],
    nav: { sectionKey: 'iam', labelKey: 'roles', to: '/iam/roles', allowedActions: ['admin:iam-group.read'], order: 3 },
  },
  { path: 'iam/roles/:id', LazyPage: L.LazyIamRoleDetailPage, allowedActions: ['admin:iam-group.read'] },
  {
    path: 'iam/policies',
    LazyPage: L.LazyIamPolicyListPage,
    allowedActions: ['admin:iam-policy.read'],
    nav: { sectionKey: 'iam', labelKey: 'policies', to: '/iam/policies', allowedActions: ['admin:iam-policy.read'], order: 4 },
  },
  { path: 'iam/policies/new', LazyPage: L.LazyIamPolicyEditorPage, allowedActions: ['admin:iam-policy.create'] },
  { path: 'iam/policies/:id/edit', LazyPage: L.LazyIamPolicyEditorPage, allowedActions: ['admin:iam-policy.update'] },
  { path: 'iam/policies/:id', LazyPage: L.LazyIamPolicyDetailPage, allowedActions: ['admin:iam-policy.read'] },
  {
    path: 'iam/simulator',
    LazyPage: L.LazyIamSimulatorPage,
    allowedActions: ['admin:iam-policy.read'],
    nav: { sectionKey: 'iam', labelKey: 'simulator', to: '/iam/simulator', allowedActions: ['admin:iam-policy.read'], order: 5 },
  },
  { path: 'iam/principals/:type/:id', LazyPage: L.LazyIamPrincipalPoliciesPage, allowedActions: ['admin:iam-policy.read'] },
  {
    path: 'iam/identity-providers',
    LazyPage: L.LazyIdentityProviderPage,
    allowedActions: ['admin:identity-provider.read'],
    nav: { sectionKey: 'iam', labelKey: 'identityProvider', to: '/iam/identity-providers', allowedActions: ['admin:identity-provider.read'], order: 6 },
  },
  { path: 'iam/identity-providers/new', LazyPage: L.LazyIdentityProviderCreatePage, allowedActions: ['admin:identity-provider.create'] },
  { path: 'iam/identity-providers/:id', LazyPage: L.LazyIdentityProviderDetailPage, allowedActions: ['admin:identity-provider.read'] },

  // ── System (slim — generic ops tools only) ──
  {
    path: 'tools/ai-gateway-simulator',
    LazyPage: L.LazyAIGatewaySimulatorPage,
    allowedActions: ['admin:virtual-key.read'],
    nav: { sectionKey: 'system', labelKey: 'aiGatewaySimulator', to: '/tools/ai-gateway-simulator', allowedActions: ['admin:virtual-key.read'], order: 0 },
  },
  { path: 'status', LazyPage: L.LazyStatusPage, nav: { sectionKey: 'system', labelKey: 'statusHealth', to: '/status', order: 1 } },
  { path: 'status/services/:serviceName', LazyPage: L.LazyServiceDetailPage },
  { path: 'status/health', LazyPage: L.LazyProviderHealthPage },
  { path: 'setup', LazyPage: L.LazySetupWizardPage, allowedActions: ['admin:settings.update'], nav: { sectionKey: 'system', labelKey: 'setup', to: '/setup', allowedActions: ['admin:settings.update'], order: 2 } },


  // ── Personal VKs (user settings, no nav) ──
  { path: 'settings/personal-vks', LazyPage: L.LazyPersonalVKList },
  { path: 'settings/personal-vks/new', LazyPage: L.LazyPersonalVKCreate },

  // ── Fleet user/device detail (no nav) ──
  { path: 'fleet/users', LazyPage: L.LazyFleetUserListPage, allowedActions: ['admin:user.read'] },
  { path: 'fleet/users/:id', LazyPage: L.LazyFleetUserDetailPage, allowedActions: ['admin:user.read'] },
  { path: 'fleet/devices/:id', LazyPage: L.LazyFleetDeviceDetailPage, allowedActions: ['admin:agent-device.read'] },
  { path: 'devices/:id', LazyPage: L.LazyFleetDeviceDetailPage, allowedActions: ['admin:agent-device.read'] },

  // ── Account (no nav) ──
  { path: 'account', LazyPage: L.LazyMyAccountPage },

  // Catch-all 404 inside the shell
  { path: '*', LazyPage: L.LazyNotFoundPage },
];

const NAV_SECTION_ORDER: NavSectionKey[] = [
  'overview', 'aiGateway', 'compliance', 'devices', 'alerts', 'infrastructure', 'iam', 'settings', 'system',
];

type BucketItem = ShellNavItem & { _order: number };

export function buildSidebarNavSections(): ShellNavSection[] {
  const buckets = new Map<NavSectionKey, BucketItem[]>();
  for (const route of SHELL_ROUTES) {
    if (!route.nav) continue;
    const item: BucketItem = {
      labelKey: route.nav.labelKey,
      path: route.nav.to,
      _order: route.nav.order,
      ...(route.nav.allowedActions ? { allowedActions: route.nav.allowedActions } : {}),
      ...(route.nav.relatedPaths ? { relatedPaths: route.nav.relatedPaths } : {}),
    };
    const list = buckets.get(route.nav.sectionKey) ?? [];
    list.push(item);
    buckets.set(route.nav.sectionKey, list);
  }

  return NAV_SECTION_ORDER.map((sectionKey) => {
    const meta = NAV_SECTION_META[sectionKey];
    const raw = buckets.get(sectionKey) ?? [];
    const items: ShellNavItem[] = [...raw]
      .sort((a, b) => a._order - b._order)
      .map(({ _order: _ignored, ...rest }) => rest);
    return {
      titleKey: meta.titleKey,
      collapsible: meta.collapsible,
      ...(meta.defaultOpen !== undefined ? { defaultOpen: meta.defaultOpen } : {}),
      items,
    };
  }).filter((s) => s.items.length > 0);
}
