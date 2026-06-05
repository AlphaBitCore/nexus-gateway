/**
 * Central lazy imports for authenticated shell routes.
 * Keeps route modules split from App while preserving code-splitting.
 */
import { lazy, type ComponentType, type LazyExoticComponent } from 'react';

const L = (importer: () => Promise<{ default: ComponentType<object> }>): LazyExoticComponent<ComponentType<object>> =>
  lazy(importer);

export const LazyLoginPage = L(() => import('../auth/pages/LoginPage').then((m) => ({ default: m.LoginPage })));
export const LazyForgotPasswordPage = L(() =>
  import('../auth/pages/ForgotPasswordPage').then((m) => ({ default: m.ForgotPasswordPage })),
);
export const LazyCallbackPage = L(() => import('../auth/pages/CallbackPage').then((m) => ({ default: m.CallbackPage })));
export const LazySsoErrorPage = L(() => import('../auth/pages/SsoErrorPage').then((m) => ({ default: m.SsoErrorPage })));

export const LazyDashboardPage = L(() => import('../pages/dashboard/DashboardPage').then((m) => ({ default: m.DashboardPage })));
export const LazyTrafficAnalyticsPage = L(() => import('../pages/traffic/analytics/TrafficAnalyticsPage').then((m) => ({ default: m.TrafficAnalyticsPage })));
export const LazyAnalyticsPage = L(() => import('../pages/analytics/AnalyticsPage').then((m) => ({ default: m.AnalyticsPage })));
export const LazyCacheROIDashboard = L(() => import('../pages/analytics/CacheROIDashboard').then((m) => ({ default: m.CacheROIDashboard })));

// ── AI Gateway ──
export const LazyConfigProvidersPage = L(() => import('../pages/ai-gateway/providers/list/ProviderList').then((m) => ({ default: m.ConfigProvidersPage })));
export const LazyProviderDetail = L(() => import('../pages/ai-gateway/providers/detail').then((m) => ({ default: m.ProviderDetail })));
export const LazyProviderWizard = L(() => import('../pages/ai-gateway/providers/wizard').then((m) => ({ default: m.ProviderWizard })));
export const LazyConfigRoutingPage = L(() => import('../pages/ai-gateway/routing/list/RoutingRuleList').then((m) => ({ default: m.ConfigRoutingPage })));
export const LazyRoutingRuleDetail = L(() => import('../pages/ai-gateway/routing/detail').then((m) => ({ default: m.RoutingRuleDetail })));
export const LazyRoutingRuleCreate = L(() => import('../pages/ai-gateway/routing/create').then((m) => ({ default: m.RoutingRuleCreate })));
export const LazyQuotaPolicyListPage = L(() => import('../pages/ai-gateway/quota-policies/QuotaPolicyList').then((m) => ({ default: m.QuotaPolicyListPage })));
export const LazyQuotaPolicyCreate = L(() => import('../pages/ai-gateway/quota-policies/QuotaPolicyCreate').then((m) => ({ default: m.QuotaPolicyCreate })));
export const LazyQuotaPolicyDetail = L(() => import('../pages/ai-gateway/quota-policies/QuotaPolicyDetail').then((m) => ({ default: m.QuotaPolicyDetail })));
export const LazyQuotaPolicyEdit = L(() => import('../pages/ai-gateway/quota-policies/QuotaPolicyEdit').then((m) => ({ default: m.QuotaPolicyEdit })));
export const LazyQuotaOverrideListPage = L(() => import('../pages/ai-gateway/quota-overrides/QuotaOverrideList').then((m) => ({ default: m.QuotaOverrideListPage })));
export const LazyQuotaOverrideCreate = L(() => import('../pages/ai-gateway/quota-overrides/QuotaOverrideCreate').then((m) => ({ default: m.QuotaOverrideCreate })));
export const LazyQuotaOverrideDetail = L(() => import('../pages/ai-gateway/quota-overrides/QuotaOverrideDetail').then((m) => ({ default: m.QuotaOverrideDetail })));
export const LazyQuotaOverrideEdit = L(() => import('../pages/ai-gateway/quota-overrides/QuotaOverrideEdit').then((m) => ({ default: m.QuotaOverrideEdit })));

// ── Compliance ──
export const LazyConfigHooksPage = L(() => import('../pages/compliance/hooks/list/HookList').then((m) => ({ default: m.ConfigHooksPage })));
export const LazyHookCreate = L(() => import('../pages/compliance/hooks/detail/HookCreate').then((m) => ({ default: m.HookCreate })));
export const LazyHookDetail = L(() => import('../pages/compliance/hooks/detail/HookDetail').then((m) => ({ default: m.HookDetail })));
export const LazyRulePackListPage = L(() =>
  import('../pages/compliance/rule-packs/list/RulePackList').then((m) => ({ default: m.RulePackList })),
);
export const LazyRulePackCreatePage = L(() =>
  import('../pages/compliance/rule-packs/form/RulePackCreatePage').then((m) => ({ default: m.RulePackCreatePage })),
);
export const LazyRulePackEditPage = L(() =>
  import('../pages/compliance/rule-packs/form/RulePackEditPage').then((m) => ({ default: m.RulePackEditPage })),
);
export const LazyRulePackDetailPage = L(() =>
  import('../pages/compliance/rule-packs/detail/RulePackDetail').then((m) => ({ default: m.RulePackDetail })),
);
export const LazyComplianceDashboardPage = L(() => import('../pages/compliance/dashboard/ComplianceDashboardPage').then((m) => ({ default: m.ComplianceDashboardPage })));
export const LazyComplianceExemptionsPage = L(() =>
  import('../pages/compliance/exemptions/ExemptionsPage').then((m) => ({ default: m.ExemptionsPage })),
);
export const LazyComplianceExemptionDetailPage = L(() =>
  import('../pages/compliance/exemptions/ComplianceExemptionDetailPage').then((m) => ({
    default: m.ComplianceExemptionDetailPage,
  })),
);
export const LazyInterceptionDomainsPage = L(() =>
  import('../pages/compliance/interception/InterceptionDomainsPage').then((m) => ({
    default: m.InterceptionDomainsPage,
  })),
);
export const LazyInterceptionDomainDetailPage = L(() =>
  import('../pages/compliance/interception/InterceptionDomainDetailPage').then((m) => ({
    default: m.InterceptionDomainDetailPage,
  })),
);

// ── Alerts ──
export const LazyAlertListPage = L(() =>
  import('../pages/alerts/list/AlertListPage').then((m) => ({ default: m.AlertListPage })),
);
export const LazyAlertRulesListPage = L(() =>
  import('../pages/alerts/rules/AlertRulesListPage').then((m) => ({ default: m.AlertRulesListPage })),
);
export const LazyAlertRuleEditPage = L(() =>
  import('../pages/alerts/rules/AlertRuleEditPage').then((m) => ({ default: m.AlertRuleEditPage })),
);
export const LazyAlertChannelsListPage = L(() =>
  import('../pages/alerts/channels/AlertChannelsListPage').then((m) => ({ default: m.AlertChannelsListPage })),
);
export const LazyAlertChannelEditPage = L(() =>
  import('../pages/alerts/channels/AlertChannelEditPage').then((m) => ({ default: m.AlertChannelEditPage })),
);

// ── Devices ──
export const LazyDeviceListPage = L(() => import('../pages/devices/DeviceListPage').then((m) => ({ default: m.DeviceListPage })));
export const LazyDeviceGroupListPage = L(() => import('../pages/devices/groups/DeviceGroupListPage').then((m) => ({ default: m.DeviceGroupListPage })));
export const LazyDeviceGroupCreatePage = L(() => import('../pages/devices/groups/DeviceGroupCreatePage').then((m) => ({ default: m.DeviceGroupCreatePage })));
export const LazyDeviceGroupDetailPage = L(() => import('../pages/devices/groups/DeviceGroupDetailPage').then((m) => ({ default: m.DeviceGroupDetailPage })));
export const LazyFleetOverviewPage = L(() => import('../pages/fleet-analytics/FleetOverviewPage').then((m) => ({ default: m.FleetOverviewPage })));
export const LazyFleetUserListPage = L(() => import('../pages/fleet/FleetUserListPage').then((m) => ({ default: m.FleetUserListPage })));
export const LazyFleetUserDetailPage = L(() => import('../pages/fleet/FleetUserDetailPage').then((m) => ({ default: m.FleetUserDetailPage })));
export const LazyFleetDeviceDetailPage = L(() => import('../pages/devices/FleetDeviceDetailPage').then((m) => ({ default: m.FleetDeviceDetailPage })));

// ── Governance ──
export const LazyAuditLogPage = L(() => import('../pages/governance/AuditLogPage').then((m) => ({ default: m.AuditLogPage })));
export const LazyDSARPage = L(() => import('../pages/governance/DSARPage').then((m) => ({ default: m.DSARPage })));
export const LazyComplianceReportPage = L(() => import('../pages/governance/ComplianceReportPage').then((m) => ({ default: m.ComplianceReportPage })));

// ── IAM ──
export const LazyOrganizationList = L(() => import('../pages/iam/organizations/OrganizationList').then((m) => ({ default: m.OrganizationList })));
export const LazyOrganizationDetail = L(() => import('../pages/iam/organizations/OrganizationDetail').then((m) => ({ default: m.OrganizationDetail })));
export const LazyOrganizationCreate = L(() => import('../pages/iam/organizations/OrganizationCreate').then((m) => ({ default: m.OrganizationCreate })));
export const LazyProjectList = L(() => import('../pages/iam/projects/ProjectList').then((m) => ({ default: m.ProjectList })));
export const LazyProjectDetail = L(() => import('../pages/iam/projects/ProjectDetail').then((m) => ({ default: m.ProjectDetail })));
export const LazyProjectCreate = L(() => import('../pages/iam/projects/ProjectCreate').then((m) => ({ default: m.ProjectCreate })));
export const LazyVirtualKeyListPage = L(() => import('../pages/ai-gateway/virtual-keys/VirtualKeyList').then((m) => ({ default: m.VirtualKeyListPage })));
export const LazyVirtualKeyCreate = L(() => import('../pages/ai-gateway/virtual-keys/VirtualKeyCreate').then((m) => ({ default: m.VirtualKeyCreate })));
export const LazyVirtualKeyDetail = L(() => import('../pages/ai-gateway/virtual-keys/detail').then((m) => ({ default: m.VirtualKeyDetail })));
export const LazyCredentialListPage = L(() => import('../pages/ai-gateway/credentials/CredentialList').then((m) => ({ default: m.CredentialListPage })));
export const LazyCredentialCreate = L(() => import('../pages/ai-gateway/credentials/CredentialCreate').then((m) => ({ default: m.CredentialCreate })));
export const LazyCredentialDetail = L(() => import('../pages/ai-gateway/credentials/CredentialDetail').then((m) => ({ default: m.CredentialDetail })));
export const LazyIamUserListPage = L(() => import('../pages/iam/users/IamUsersWithOrgsPage').then((m) => ({ default: m.IamUsersWithOrgsPage })));
export const LazyIamUserCreatePage = L(() => import('../pages/iam/users/IamUserCreate').then((m) => ({ default: m.IamUserCreate })));
export const LazyIamUserDetailPage = L(() => import('../pages/iam/user-detail').then((m) => ({ default: m.IamUserDetail })));
export const LazyIamPolicyListPage = L(() => import('../pages/iam/policies/IamPolicyList').then((m) => ({ default: m.IamPolicyList })));
export const LazyIamPolicyDetailPage = L(() => import('../pages/iam/policies/IamPolicyDetail').then((m) => ({ default: m.IamPolicyDetail })));
export const LazyIamPolicyEditorPage = L(() => import('../pages/iam/policies/IamPolicyEditorPage').then((m) => ({ default: m.IamPolicyEditorPage })));
export const LazyIamSimulatorPage = L(() => import('../pages/iam/simulator/IamSimulator').then((m) => ({ default: m.IamSimulator })));
export const LazyIamPrincipalPoliciesPage = L(() => import('../pages/iam/policies/IamPrincipalPolicies').then((m) => ({ default: m.IamPrincipalPolicies })));
export const LazyIamRoleListPage = L(() => import('../pages/iam/roles/IamRoleList').then((m) => ({ default: m.IamRoleList })));
export const LazyIamRoleDetailPage = L(() => import('../pages/iam/roles/IamRoleDetail').then((m) => ({ default: m.IamRoleDetail })));
export const LazyOAuthClientsListPage = L(() => import('../pages/iam/oauth-clients/OAuthClientsListPage').then((m) => ({ default: m.OAuthClientsListPage })));
export const LazyOAuthClientDetailPage = L(() => import('../pages/iam/oauth-clients/OAuthClientDetailPage').then((m) => ({ default: m.OAuthClientDetailPage })));
export const LazyOAuthClientFormPage = L(() => import('../pages/iam/oauth-clients/OAuthClientFormPage').then((m) => ({ default: m.OAuthClientFormPage })));

// ── Proxy (operational pages, not deleted) ──
export const LazyRejectConfigPage = L(() => import('../pages/proxy/reject/RejectConfigPage').then((m) => ({ default: m.RejectConfigPage })));
export const LazyProxyStatusCompliancePage = L(() => import('../pages/proxy/compliance/ProxyStatusCompliancePage').then((m) => ({ default: m.ProxyStatusCompliancePage })));

// ── Infrastructure ──
export const LazyInfraNodesPage = L(() => import('../pages/infrastructure/nodes/InfraNodesPage'));
export const LazyInfraNodeDetailPage = L(() => import('../pages/infrastructure/nodes/InfraNodeDetailPage'));
export const LazyInfraOverridesPage = L(() => import('../pages/infrastructure/overrides/InfraOverridesPage'));
export const LazyInfraConfigSyncPage = L(() => import('../pages/infrastructure/config-sync/InfraConfigSyncPage'));
export const LazyInfraJobsPage = L(() => import('../pages/infrastructure/jobs/InfraJobsPage'));
export const LazyInfraJobDetailPage = L(() => import('../pages/infrastructure/jobs/InfraJobDetailPage'));
export const LazyInfraKillSwitchPage = L(() => import('../pages/infrastructure/kill-switch/InfraKillSwitchPage'));
export const LazyInfraRecentErrorsPage = L(() => import('../pages/infrastructure/recent-errors/InfraRecentErrorsPage'));
export const LazyInfraDlqPage = L(() => import('../pages/infrastructure/dlq/InfraDlqPage'));
export const LazyInfraCrashReportsPage = L(() => import('../pages/infrastructure/crash-reports/InfraCrashReportsPage'));
export const LazyInfraDiagModePage = L(() => import('../pages/infrastructure/diag-mode/InfraDiagModePage'));
export const LazyInfraProxySetupPage = L(() => import('../pages/infrastructure/proxy-rollout/InfraProxySetupPage'));
export const LazyInfraAgentSetupPage = L(() => import('../pages/infrastructure/agent-setup/InfraAgentSetupPage'));
export const LazyInfraCliSetupPage = L(() => import('../pages/infrastructure/cli-setup/InfraCliSetupPage'));
export const LazyProxySetupPage = L(() => import('../pages/proxy/setup/ProxySetupPage'));

// ── Status ──
export const LazyStatusPage = L(() => import('../pages/status/overview/StatusPage').then((m) => ({ default: m.StatusPage })));
export const LazyProviderHealthPage = L(() => import('../pages/status/detail/ProviderHealthPage').then((m) => ({ default: m.ProviderHealthPage })));
export const LazyServiceDetailPage = L(() => import('../pages/status/detail/ServiceDetailPage').then((m) => ({ default: m.ServiceDetailPage })));

// ── System ──
export const LazySetupWizardPage = L(() => import('../pages/setup/SetupWizardPage').then((m) => ({ default: m.SetupWizardPage })));
export const LazyAIGuardPage = L(() => import('../pages/compliance/aiguard/AIGuardPage').then((m) => ({ default: m.AIGuardPage })));
export const LazyObservabilityRetentionPage = L(() => import('../pages/infrastructure/observability-retention/ObservabilityRetention'));
export const LazyAIGatewaySimulatorPage = L(() =>
  import('../pages/tools/ai-gateway-simulator/AIGatewaySimulatorPage').then((m) => ({
    default: m.AIGatewaySimulatorPage,
  })),
);
export const LazyDeviceAuthSettingsPage = L(() => import('../pages/devices/auth/DeviceAuthSettingsPage').then((m) => ({ default: m.DeviceAuthSettingsPage })));
export const LazyIdentityProviderPage = L(() => import('../pages/devices/auth/IdentityProviderPage').then((m) => ({ default: m.IdentityProviderPage })));
export const LazyIdentityProviderCreatePage = L(() => import('../pages/devices/auth/IdentityProviderCreatePage').then((m) => ({ default: m.IdentityProviderCreatePage })));
export const LazyIdentityProviderDetailPage = L(() => import('../pages/devices/auth/IdentityProviderDetailPage').then((m) => ({ default: m.IdentityProviderDetailPage })));

// ── Settings: each tab is a standalone page ──
export const LazyAgentSettingsPage = L(() => import('../pages/_shared/settings/SettingsPageWrappers').then((m) => ({ default: m.AgentSettingsPage })));
export const LazyObservabilityConfigPage = L(() => import('../pages/_shared/settings/SettingsPageWrappers').then((m) => ({ default: m.ObservabilityConfigPage })));
export const LazyPayloadCaptureSettingsPage = L(() => import('../pages/_shared/settings/SettingsPageWrappers').then((m) => ({ default: m.PayloadCaptureSettingsPage })));
export const LazyStreamingComplianceSettingsPage = L(() => import('../pages/_shared/settings/SettingsPageWrappers').then((m) => ({ default: m.StreamingComplianceSettingsPage })));
export const LazySiemSettingsPage = L(() => import('../pages/_shared/settings/SettingsPageWrappers').then((m) => ({ default: m.SiemSettingsPage })));
// ── Cache (consolidated: merges Prompt Cache + Cache Embedding pages) ──
export const LazyCachePage = L(() =>
  import('../pages/ai-gateway/cache/CachePage').then((m) => ({ default: m.CachePage })),
);
export const LazyPassthroughPage = L(() => import('../pages/ai-gateway/passthrough/PassthroughPage').then((m) => ({ default: m.PassthroughPage })));
export const LazyCredentialReliabilitySettingsPage = L(() => import('../pages/_shared/settings/SettingsPageWrappers').then((m) => ({ default: m.CredentialReliabilitySettingsPage })));

// ── Analytics ──
export const LazyQuotaUsageDashboard = L(() => import('../pages/analytics/quota-usage/QuotaUsageDashboard').then((m) => ({ default: m.QuotaUsageDashboard })));

// ── Personal VKs (user settings) ──
export const LazyPersonalVKList = L(() => import('../pages/account/personal-vks/PersonalVKList').then((m) => ({ default: m.PersonalVKList })));
export const LazyPersonalVKCreate = L(() => import('../pages/account/personal-vks/PersonalVKCreate').then((m) => ({ default: m.PersonalVKCreate })));

// ── Account ──
export const LazyMyAccountPage = L(() => import('../pages/account/MyAccountPage').then((m) => ({ default: m.MyAccountPage })));

export const LazyNotFoundPage = L(() => import('../pages/NotFoundPage').then((m) => ({ default: m.NotFoundPage })));
