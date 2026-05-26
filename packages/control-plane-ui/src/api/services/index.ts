/**
 * API service barrel — typed, centralized access to all backend endpoints.
 *
 * Usage:
 *   import { providerApi, routingApi } from '@/api/services';
 *   const providers = await providerApi.list({ q: 'openai' });
 *   const rule = await routingApi.get(id);
 */

export { providerApi } from './ai-gateway/providers';
export type { CreateProviderInput, UpdateProviderInput, ProviderListParams, ProviderConnectivityResult } from './ai-gateway/providers';

export { routingApi } from './ai-gateway/routing';
export type {
  RoutingRuleListParams,
  RoutingRuleWritePayload,
  RoutingRuleUpdatePayload,
  RoutingSimulateRequest,
  RoutingSimulateResponse,
  RoutingSimulateStage,
  RoutingSimulateTrace,
  RoutingSimulateTarget,
  RoutingNarrowingSummary,
} from './ai-gateway/routing';

export { hookApi } from './compliance/hooks';
export type { HookWritePayload, HookUpdatePayload, HookTestRequestBody, HookReorderRequestBody } from './compliance/hooks';

export { rulePacksApi } from './compliance/rulepacks';
export type {
  RulePackMeta,
  RulePackRule,
  RulePack,
  RulePackCreateInput,
  RulePackUpdateInput,
  RulePackMatch,
  RulePackPreviewResult,
  RulePackImportResult,
  RulePackInstall,
  RulePackOverride,
  EffectiveRuleSet,
} from './compliance/rulepacks';

export { analyticsApi } from './overview/analytics';

export { iamApi } from './iam/iam';
export type {
  CreateAdminUserInput,
  UpdateAdminUserInput,
  CreateAdminApiKeyInput,
  PatchAdminApiKeyInput,
  PatchMeInput,
  CreateIamPolicyInput,
  UpdateIamPolicyInput,
  IamGroupWriteInput,
  IamGroupUpdateInput,
  IamAddGroupMemberInput,
  IamAttachPolicyInput,
  IamSimulateRequestBody,
  ActionCatalogAction,
  ActionCatalogEntry,
  ActionCatalogResponse,
} from './iam/iam';

export { credentialApi, reliabilitySettingsApi } from './ai-gateway/credentials';
export type { CreateCredentialInput, UpdateCredentialInput } from './ai-gateway/credentials';

export { virtualKeyApi } from './ai-gateway/virtualKeys';
export type { CreateVirtualKeyInput, UpdateVirtualKeyInput } from './ai-gateway/virtualKeys';

export { organizationApi } from './iam/organizations';
export type { CreateOrganizationInput, UpdateOrganizationInput } from './iam/organizations';

export { projectApi } from './iam/projects';
export type { CreateProjectInput, UpdateProjectInput } from './iam/projects';

export { interceptionDomainApi } from './compliance/interceptionDomains';
export type {
  InterceptionDomain,
  InterceptionPath,
  InterceptionDomainListResponse,
  InterceptionDomainListParams,
  InterceptionDomainCreatePayload,
  InterceptionDomainUpdatePayload,
  InterceptionPathCreatePayload,
  InterceptionPathUpdatePayload,
  HostMatchType,
  PathMatchType,
  PathAction,
  DefaultPathAction,
  FailureAction,
  NetworkZone,
} from './compliance/interceptionDomains';

export { systemApi } from './infrastructure/misc/system';
export type { UpdateSettingsInput } from './infrastructure/misc/system';

export { thingStatsApi } from './infrastructure/nodes/thingStats';
export type {
  ThingStatsResponse,
  ThingStatsRow,
  ThingStatsParams,
  ThingStatsGranule,
} from './infrastructure/nodes/thingStats';

export { opsMetricsApi } from './infrastructure/ops/opsmetrics';
export type {
  OpsMetricSample,
  OpsMetricBucket,
  OpsMetricsCurrentParams,
  OpsMetricsTimeseriesParams,
  OpsMetricsFleetParams,
  TimeseriesResponse,
  NodeTypeFilter,
  MetricKind,
  Granularity,
  ResolvedGranularity,
} from './infrastructure/ops/opsmetrics';

export { diagEventsApi } from './infrastructure/diag/diagevents';
export type {
  DiagEvent,
  DiagGroup,
  CrashCohort,
  DiagLevel,
  ListDiagEventsParams,
  DiagEventListResponse,
  DiagGroupsParams,
  CrashCohortsParams,
} from './infrastructure/diag/diagevents';

export { diagModeApi } from './infrastructure/diag/diagmode';
export type {
  DiagModeWindow,
  EnableDiagModeRequest,
  BulkDiagModeRequest,
  BulkDiagModeFilter,
  BulkDiagModeItem,
  BulkDiagModeResult,
} from './infrastructure/diag/diagmode';

export { retentionApi } from './infrastructure/ops/retention';
export type {
  RetentionLayer,
  RetentionLayerName,
  RetentionGetResponse,
  RetentionUpdate,
  RetentionPutResponse,
} from './infrastructure/ops/retention';

export { authApi, AuthserverError } from './iam/authserver';
export type {
  AuthserverErrorCode,
  IdpEntry,
  IdpListResponse,
  IdpType,
  PasswordSubmitResponse,
} from './iam/authserver';

export { proxyApi } from './infrastructure/misc/proxy';

export { devicesApi } from './devices/devices';

export { agentEventsApi } from './system/agent-events';
export type { AgentEventExportResponse, AgentEventListRow } from './system/agent-events';

export { fleetAnalyticsApi } from './overview/fleet-analytics';
export type {
  FleetSummary,
  FleetTrendBucket,
  FleetTrendsResponse,
  TopDestination,
  TopDestinationsResponse,
} from './overview/fleet-analytics';

export { deviceGroupsApi } from './devices/device-groups';
export type {
  DeviceGroup,
  DeviceGroupListItem,
  DeviceGroupDetail,
  DeviceGroupMembership,
  CreateDeviceGroupInput,
  UpdateDeviceGroupInput,
  PreviewMembershipResponse,
  BulkActionResult,
  BulkActionResponse,
} from './devices/device-groups';
export { fleetApi } from './devices/fleet';

export { quotaPolicyApi } from './ai-gateway/quotaPolicies';
export type { QuotaPolicy, CreateQuotaPolicyInput } from './ai-gateway/quotaPolicies';

export { quotaOverrideApi } from './ai-gateway/quotaOverrides';
export type { QuotaOverride, CreateQuotaOverrideInput } from './ai-gateway/quotaOverrides';

export { personalVKApi } from './ai-gateway/personalVirtualKeys';
export type { CreatePersonalVKInput } from './ai-gateway/personalVirtualKeys';

export { personalApiKeyApi } from './iam/personalApiKeys';
export type { CreatePersonalApiKeyInput } from './iam/personalApiKeys';

export { quotaAnalyticsApi } from './ai-gateway/quotaAnalytics';
export type { QuotaUsageRow, QuotaTrendPoint, QuotaTopConsumer } from './ai-gateway/quotaAnalytics';

export { alertsApi } from './alerts/alerts';
export type {
  Alert,
  AlertRule,
  AlertChannel,
  AlertSeverity,
  AlertState,
  AlertListResponse,
  AlertDetailResponse,
  AlertDispatch,
  ListAlertsParams,
} from './alerts/alerts';

export { hubApi } from './infrastructure/nodes/hub';
export { serviceUrlsApi } from './infrastructure/misc/service-urls';
export type { ServicePublicURLs } from './infrastructure/misc/service-urls';
export type {
  Node,
  NodeListResponse,
  ConfigSyncComparison,
  OutOfSyncItem,
  ConfigHistoryEvent,
  ConfigHistoryResponse,
  ConfigCatalogEntry,
  ConfigCatalogResponse,
  ScheduledJob,
  ConfigUpdateRequest,
  EnrollmentToken,
} from './infrastructure/nodes/hub';

export type {
  ProxyStatus,
  ProxyConnection,
  RejectConfig,
  ComplianceCoverage,
} from './infrastructure/misc/proxy';

export { passthroughApi, validatePassthroughPayload, PASSTHROUGH_MAX_EXPIRY_HOURS, PASSTHROUGH_MIN_REASON_LEN } from './ai-gateway/passthrough';
export type { PassthroughPayload, PassthroughTier, PassthroughSnapshot } from './ai-gateway/passthrough';
