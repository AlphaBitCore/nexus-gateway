/**
 * Default MSW request handlers — realistic mock responses for common endpoints.
 *
 * These provide sensible defaults so most tests don't need custom handlers.
 * Individual tests can override specific endpoints via server.use().
 */
import { http, HttpResponse } from 'msw';

/** Echo limit/offset from the request so list stubs match Control Plane pagination JSON shape. */
function listPaginationParamsFromUrl(url: string): { limit: number; offset: number } {
  const u = new URL(url, 'http://localhost');
  const limitRaw = u.searchParams.get('limit');
  const offsetRaw = u.searchParams.get('offset');
  const limit = limitRaw != null && limitRaw !== '' ? Number(limitRaw) : 20;
  const offset = offsetRaw != null && offsetRaw !== '' ? Number(offsetRaw) : 0;
  return {
    limit: Number.isFinite(limit) && limit > 0 ? limit : 20,
    offset: Number.isFinite(offset) && offset >= 0 ? offset : 0,
  };
}

export const mockProvider = {
  id: 'provider-1',
  name: 'openai',
  displayName: 'OpenAI',
  baseUrl: 'https://api.openai.com',
  adapterType: 'openai',
  enabled: true,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
};

export const mockModel = {
  id: 'model-1',
  name: 'GPT-4o',
  modelId: 'gpt-4o',
  providerId: 'provider-1',
  type: 'chat',
  status: 'active',
  features: ['streaming', 'function_calling'],
};

export const mockRoutingRule = {
  id: 'rule-1',
  name: 'smart-auto-routing',
  strategyType: 'smart',
  priority: 25,
  enabled: true,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
};

export const mockUser = {
  id: 'user-1',
  displayName: 'admin',
  email: 'admin@example.com',
  status: 'active',
  canAccessControlPlane: true,
  roles: ['SuperAdmin'],
  lastLoginAt: '2026-03-31T12:00:00Z',
  createdAt: '2026-01-01T00:00:00Z',
};

export const mockAnalyticsSummary = {
  totalRequests: 12345,
  totalTokens: 1500000,
  totalPromptTokens: 900000,
  totalCompletionTokens: 600000,
  totalEstimatedCostUsd: 45.67,
  avgLatencyMs: 342,
  errorCount: 23,
  errorRate: 0.0019,
};

export const mockTrafficStorage = {
  traffic: { enabled: true, queryable: true, sink: 'database' },
};

export const mockHook = {
  id: 'hook-1',
  name: 'pii-redaction',
  type: 'builtin',
  stage: 'request',
  priority: 10,
  timeoutMs: 5000,
  failBehavior: 'open',
  enabled: true,
  config: {},
  category: 'compliance',
  classification: {
    category: 'compliance',
    categoryLabel: 'Compliance',
    implementationLabel: 'PII Redaction',
    dualPhaseCapable: false,
  },
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
};

export const mockInterceptionPath = {
  id: 'path-1',
  domainId: 'domain-1',
  pathPattern: ['/v1/chat/completions'],
  matchType: 'PREFIX' as const,
  action: 'PROCESS' as const,
  priority: 10,
  enabled: true,
  createdAt: '2026-04-22T00:00:00Z',
  updatedAt: '2026-04-22T00:00:00Z',
};

/** Mirrors `adapters.BuiltinTrafficAdapterIDs()` from Go (sorted). */
export const mockTrafficAdapterCatalog = {
  data: [
    'anthropic',
    'azure-openai',
    'bedrock',
    'deepseek',
    'gemini',
    'generic-jsonpath',
    'glm',
    'minimax',
    'openai-compat',
    'vertex',
  ],
};

export const mockInterceptionDomain = {
  id: 'domain-1',
  name: 'OpenAI',
  description: 'Primary OpenAI API endpoint',
  hostPattern: 'api.openai.com',
  hostMatchType: 'EXACT' as const,
  adapterId: 'openai-compat',
  adapterConfig: null,
  enabled: true,
  priority: 100,
  defaultPathAction: 'PROCESS' as const,
  onAdapterError: 'FAIL_OPEN' as const,
  networkZone: 'PUBLIC' as const,
  source: 'admin',
  createdAt: '2026-04-22T00:00:00Z',
  updatedAt: '2026-04-22T00:00:00Z',
  createdBy: null,
  paths: [mockInterceptionPath],
};

export const mockOrganization = {
  id: 'org-1',
  name: 'Acme Corp',
  code: 'acme',
  enabled: true,
  children: [],
  _count: { projects: 2 },
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
};

export const mockProject = {
  id: 'project-1',
  name: 'Alpha',
  code: 'alpha',
  status: 'active',
  organizationId: 'org-1',
  organization: { name: 'Acme Corp' },
  _count: { virtualKeys: 3 },
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
};

export const mockMe = {
  keyId: 'user-1',
  keyName: 'admin',
  roles: ['super-admins'],
  authPrincipalType: 'admin_user' as const,
  email: 'admin@example.com',
};

export const mockNode = {
  id: 'node-gw-1',
  type: 'ai-gateway',
  name: 'gateway-east-1',
  status: 'online',
  listen_address: ':3050',
  metrics_url: 'http://localhost:9090/metrics',
  version: '1.2.0',
  role: 'primary',
  auth_type: 'mtls',
  conn_protocol: 'grpc',
  targetConfig: { routing: { version: 3 } },
  targetVersion: 3,
  appliedConfig: { routing: { version: 3 } },
  appliedVersion: 3,
  last_seen_at: '2026-04-17T08:00:00Z',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-04-17T08:00:00Z',
  overrideCount: 2,
  overrideStaleCount: 1,
};

export const mockNode2 = {
  id: 'node-proxy-1',
  type: 'compliance-proxy',
  name: 'proxy-west-1',
  status: 'online',
  listen_address: ':3040',
  metrics_url: null,
  version: '1.1.0',
  role: null,
  auth_type: 'mtls',
  conn_protocol: 'grpc',
  targetConfig: { killswitch: { engaged: false } },
  targetVersion: 2,
  appliedConfig: { killswitch: { engaged: false } },
  appliedVersion: 2,
  last_seen_at: '2026-04-17T07:55:00Z',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-04-17T07:55:00Z',
  overrideCount: 0,
  overrideStaleCount: 0,
};

export const mockNode3 = {
  id: 'node-agent-1',
  type: 'agent',
  name: 'agent-laptop-alice',
  status: 'offline',
  listen_address: null,
  metrics_url: null,
  version: '0.9.0',
  role: null,
  auth_type: 'token',
  conn_protocol: 'websocket',
  targetConfig: { hooks: { version: 1 } },
  targetVersion: 1,
  appliedConfig: { hooks: { version: 1 } },
  appliedVersion: 1,
  last_seen_at: '2026-04-16T18:00:00Z',
  created_at: '2026-02-01T00:00:00Z',
  updated_at: '2026-04-16T18:00:00Z',
  overrideCount: 0,
  overrideStaleCount: 0,
};

export const mockConfigHistoryEvent = {
  id: 'evt-1',
  nodeType: 'compliance-proxy',
  configKey: 'killswitch',
  action: 'update',
  actorId: 'user-1',
  actorName: 'admin',
  newVersion: 2,
  sourceIp: '10.0.0.1',
  createdAt: '2026-04-17T09:00:00Z',
};

export const mockJob = {
  id: 'out-of-sync-detector',
  name: 'out-of-sync-detector',
  description: 'Flags things whose shadow desired/reported diverged past the threshold.',
  interval: 300_000_000_000, // 5m in ns
  enabled: true,
  lastRun: '2026-04-17T09:55:00Z',
  lastDuration: 120_000_000, // 120ms
  lastStatus: 'ok',
  nextRun: '2026-04-17T10:00:00Z',
  runCount: 42,
  errorCount: 0,
};

export const mockJob2 = {
  id: 'stale-node-reaper',
  name: 'stale-node-reaper',
  description: 'Marks things offline when their lastSeen crosses the stale window.',
  interval: 3_600_000_000_000, // 1h in ns
  enabled: true,
  lastRun: '2026-04-17T09:00:00Z',
  lastDuration: 340_000_000, // 340ms
  lastStatus: 'failed',
  lastError: 'connection timeout',
  nextRun: '2026-04-17T10:00:00Z',
  runCount: 17,
  errorCount: 3,
};

export const mockJobRun = {
  id: 'run-1',
  jobId: 'out-of-sync-detector',
  startedAt: '2026-04-17T09:55:00Z',
  finishedAt: '2026-04-17T09:55:00Z',
  durationMs: 120,
  status: 'success',
  replicaId: 'hub-0',
};

export const mockJobRun2 = {
  id: 'run-2',
  jobId: 'out-of-sync-detector',
  startedAt: '2026-04-17T09:50:00Z',
  finishedAt: '2026-04-17T09:50:00Z',
  durationMs: 98,
  status: 'success',
  replicaId: 'hub-0',
};

// ── Diag events fixtures (Recent Errors / Crash Reports pages) ──
export const mockDiagEventError = {
  id: 'de-1',
  nodeId: 'agent-1',
  nodeType: 'agent',
  occurredAt: '2026-04-27T10:00:00Z',
  receivedAt: '2026-04-27T10:00:01Z',
  level: 'error',
  eventType: 'agent.error',
  source: 'agent',
  message: 'dial to upstream failed',
  messageHash: 'a8f23deadbeef',
  attrs: { url: 'https://api.openai.com', err: 'EOF' },
  repeatCount: 1,
  agentVersion: '1.4.2',
};

export const mockDiagEventFatal = {
  id: 'de-2',
  nodeId: 'agent-2',
  nodeType: 'agent',
  occurredAt: '2026-04-27T09:30:00Z',
  receivedAt: '2026-04-27T09:30:01Z',
  level: 'fatal',
  eventType: 'agent.crash',
  source: 'agent',
  message: 'panic: nil deref in relay loop',
  messageHash: 'b1c4cafe1234',
  attrs: { goroutines: 23 },
  stackTrace: 'goroutine 1 [running]:\nmain.run()\n\t/x.go:42 +0x1c',
  repeatCount: 1,
  agentVersion: '1.4.2',
};

export const mockDiagGroupError = {
  level: 'error',
  messageHash: 'a8f23deadbeef',
  sampleMessage: 'dial to upstream failed',
  affectedNodes: 12,
  totalOccurrences: 1420,
  lastSeenAt: '2026-04-27T10:00:00Z',
};

export const mockDiagGroupFatal = {
  level: 'fatal',
  messageHash: 'b1c4cafe1234',
  sampleMessage: 'panic: nil deref in relay loop',
  affectedNodes: 3,
  totalOccurrences: 5,
  lastSeenAt: '2026-04-27T09:30:00Z',
};

export const mockCrashCohort1 = {
  agentVersion: '1.4.2',
  os: 'darwin',
  osVersion: '14.4',
  affectedNodes: 8,
  totalCrashes: 12,
  firstSeenAt: '2026-04-26T00:00:00Z',
  lastSeenAt: '2026-04-27T09:30:00Z',
};

export const mockCrashCohort2 = {
  agentVersion: '1.4.1',
  os: 'linux',
  osVersion: 'Ubuntu 22.04',
  affectedNodes: 3,
  totalCrashes: 3,
  firstSeenAt: '2026-04-25T00:00:00Z',
  lastSeenAt: '2026-04-26T12:00:00Z',
};

/** Full set of admin actions granted to the default test principal (super-admin). */
export const mockMePermissions: string[] = [
  'admin:provider.read', 'admin:provider.create', 'admin:provider.update', 'admin:provider.delete',
  'admin:model.read', 'admin:model.create', 'admin:model.update', 'admin:model.delete',
  'admin:credential.read', 'admin:credential.create', 'admin:credential.update', 'admin:credential.delete',
  'admin:virtual-key.read', 'admin:virtual-key.create', 'admin:virtual-key.update', 'admin:virtual-key.delete',
  'admin:virtual-key.approve', 'admin:virtual-key.reject', 'admin:virtual-key.revoke', 'admin:virtual-key.renew',
  'admin:routing-rule.read', 'admin:routing-rule.create', 'admin:routing-rule.update', 'admin:routing-rule.delete',
  'admin:routing-rule.simulate',
  'admin:hook.read', 'admin:hook.read', 'admin:hook.create', 'admin:hook.update', 'admin:hook.update', 'admin:hook.delete',
  'admin:user.read', 'admin:user.create', 'admin:user.update', 'admin:user.delete',
  'admin:api-key.read', 'admin:api-key.create', 'admin:api-key.update', 'admin:api-key.delete',
  'admin:oauth-client.read', 'admin:oauth-client.create', 'admin:oauth-client.update', 'admin:oauth-client.delete', 'admin:oauth-client.rotate',
  'admin:organization.read', 'admin:organization.create', 'admin:organization.update', 'admin:organization.delete',
  'admin:project.read', 'admin:project.create', 'admin:project.update', 'admin:project.delete',
  'admin:iam-policy.read', 'admin:iam-policy.create', 'admin:iam-policy.update', 'admin:iam-policy.delete',
  'admin:traffic-log.read', 'admin:audit-log.read', 'admin:audit-log.export',
  'admin:analytics.read', 'admin:quota-analytics.read',
  'admin:quota-policy.read', 'admin:quota-policy.create', 'admin:quota-policy.update', 'admin:quota-policy.delete',
  'admin:quota-override.read', 'admin:quota-override.create', 'admin:quota-override.update', 'admin:quota-override.delete',
  'admin:settings.read', 'admin:settings.update', 'admin:settings.write',
  'admin:agent-device.read', 'admin:agent-device.create', 'admin:agent-device.update', 'admin:agent-device.delete',
  'admin:device-group.read', 'admin:device-group.create', 'admin:device-group.update', 'admin:device-group.delete',
  'admin:compliance-exemption.read', 'admin:compliance-exemption.update', 'admin:compliance-exemption.delete',
  'admin:kill-switch.toggle',
  'admin:rule-pack.read', 'admin:rule-pack.update',
  'admin:ai-guard-config.read', 'admin:ai-guard-config.update',
  'admin:revocation.read',
  'admin:alert.read', 'admin:alert.update',
  'admin:observability.read', 'admin:observability.write',
  'admin:node.force-resync', 'admin:node.write-override',
];

export const defaultHandlers = [
  // Auth
  http.get('/api/admin/me', () =>
    HttpResponse.json(mockMe),
  ),
  http.get('/api/admin/me/permissions', () =>
    HttpResponse.json({ actions: mockMePermissions }),
  ),

  // Providers
  http.get('/api/admin/providers', () =>
    HttpResponse.json({ data: [mockProvider], total: 1 }),
  ),
  http.get('/api/admin/providers/:id', () =>
    HttpResponse.json(mockProvider),
  ),

  // Models
  http.get('/api/admin/models', () =>
    HttpResponse.json({ data: [mockModel] }),
  ),
  http.get('/api/admin/models/flat', () =>
    HttpResponse.json({ data: [mockModel] }),
  ),

  // Routing
  http.get('/api/admin/routing-rules', () =>
    HttpResponse.json({ data: [mockRoutingRule], total: 1 }),
  ),
  http.get('/api/admin/routing-rules/:id', () =>
    HttpResponse.json(mockRoutingRule),
  ),

  // Analytics
  http.get('/api/admin/analytics/summary', () =>
    HttpResponse.json(mockAnalyticsSummary),
  ),
  http.get('/api/admin/analytics/by-provider', () =>
    HttpResponse.json({ data: [] }),
  ),
  http.get('/api/admin/analytics/cost', () =>
    HttpResponse.json({ data: [] }),
  ),
  http.get('/api/admin/analytics/usage', () =>
    HttpResponse.json({ data: [] }),
  ),

  // Traffic
  http.get('/api/admin/traffic/storage', () =>
    HttpResponse.json(mockTrafficStorage),
  ),
  http.get('/api/admin/traffic', ({ request }) => {
    const { limit, offset } = listPaginationParamsFromUrl(request.url);
    return HttpResponse.json({ data: [], total: 0, limit, offset });
  }),

  // Admin audit
  http.get('/api/admin/admin-audit-logs', ({ request }) => {
    const { limit, offset } = listPaginationParamsFromUrl(request.url);
    return HttpResponse.json({ data: [], total: 0, limit, offset });
  }),

  // Users
  http.get('/api/admin/users', () =>
    HttpResponse.json({ data: [mockUser], total: 1 }),
  ),

  // Settings
  http.get('/api/admin/settings', () =>
    HttpResponse.json({
      uptime: 86400,
      version: '1.0.0',
      goVersion: 'go1.25.0',
      maintenanceMode: false,
      logLevel: 'info',
      trafficFlushIntervalMs: 30000,
    }),
  ),

  // Health
  http.get('/api/admin/provider-health', () =>
    HttpResponse.json({ data: [] }),
  ),

  // Metrics
  http.get('/api/admin/metrics/aggregates', () =>
    HttpResponse.json({ data: [] }),
  ),

  // Hooks
  http.get('/api/admin/hooks', () =>
    HttpResponse.json({ data: [mockHook], total: 1 }),
  ),
  http.get('/api/admin/hooks/:id', () =>
    HttpResponse.json(mockHook),
  ),
  http.get('/api/admin/hooks/execution-chain', () =>
    HttpResponse.json({ requestHooks: [], responseHooks: [] }),
  ),
  http.get('/api/admin/hooks/implementations', () =>
    HttpResponse.json({
      data: [
        {
          implementationId: 'pii-detector',
          hookType: 'builtin',
          category: 'compliance',
          supportedStages: ['request', 'response'],
        },
      ],
      hookCategories: [
        { code: 'compliance', name: 'Compliance' },
        { code: 'custom', name: 'Custom' },
      ],
    }),
  ),

  // Traffic adapter catalog (shared/traffic/adapters)
  http.get('/api/admin/traffic-adapters', () =>
    HttpResponse.json(mockTrafficAdapterCatalog),
  ),

  // Interception domains
  http.get('/api/admin/interception-domains', () =>
    HttpResponse.json({ data: [mockInterceptionDomain], total: 1 }),
  ),
  http.get('/api/admin/interception-domains/:id', () =>
    HttpResponse.json(mockInterceptionDomain),
  ),
  http.post('/api/admin/interception-domains', () =>
    HttpResponse.json(mockInterceptionDomain, { status: 201 }),
  ),
  http.put('/api/admin/interception-domains/:id', () =>
    HttpResponse.json(mockInterceptionDomain),
  ),
  http.delete('/api/admin/interception-domains/:id', () =>
    new HttpResponse(null, { status: 204 }),
  ),
  http.post('/api/admin/interception-domains/:id/paths', () =>
    HttpResponse.json(mockInterceptionPath, { status: 201 }),
  ),
  http.put('/api/admin/interception-domains/:id/paths/:pathId', () =>
    HttpResponse.json(mockInterceptionPath),
  ),
  http.delete('/api/admin/interception-domains/:id/paths/:pathId', () =>
    new HttpResponse(null, { status: 204 }),
  ),

  // Organizations
  http.get('/api/admin/organizations', () =>
    HttpResponse.json({ data: [mockOrganization] }),
  ),
  http.get('/api/admin/organizations/tree', () =>
    HttpResponse.json({ data: [mockOrganization] }),
  ),

  // Projects
  http.get('/api/admin/projects', () =>
    HttpResponse.json({ data: [mockProject], total: 1 }),
  ),

  // API keys (used by SettingsPage)
  http.get('/api/admin/api-keys', () =>
    HttpResponse.json({ data: [] }),
  ),

  // Instances (used by StatusPage)
  http.get('/api/admin/instances', () =>
    HttpResponse.json({ instances: [], count: 1 }),
  ),

  // Admin readiness (used by StatusPage)
  http.get('/api/admin/ready', () =>
    HttpResponse.json({ status: 'ok', checks: { database: 'ok', redis: 'connected' } }),
  ),

  // My admin audit logs (used by SettingsPage)
  http.get('/api/admin/me/admin-audit-logs', () =>
    HttpResponse.json({ data: [], total: 0 }),
  ),

  // Virtual keys
  http.get('/api/admin/virtual-keys', () =>
    HttpResponse.json({ data: [], total: 0 }),
  ),

  // Forward-proxy BFF endpoints
  http.get('/api/admin/proxy/health', () =>
    HttpResponse.json({
      status: 'ok',
      uptimeSeconds: 3600,
      connectionsActive: 12,
      bumpEnabled: true,
      redisConnected: true,
    }),
  ),
  http.get('/api/admin/proxy/connections', () =>
    HttpResponse.json({ connections: [], total: 0 }),
  ),
  http.get('/api/admin/proxy/killswitch', () =>
    HttpResponse.json({ enabled: true, lastChanged: '2026-03-30T10:00:00Z', changedBy: 'admin' }),
  ),
  http.get('/api/admin/proxy/compliance/coverage', () =>
    HttpResponse.json({
      coveragePercent: 92.5,
      breakdown: { bumped: 185, tunneled: 15 },
      period: { start: '2026-03-31T00:00:00Z', end: '2026-04-01T00:00:00Z' },
    }),
  ),

  // Infrastructure — Nodes, Config Sync, Jobs (product-facing admin API)
  http.get('/api/admin/nodes', () =>
    HttpResponse.json({
      nodes: [mockNode, mockNode2, mockNode3],
      total: 3,
      page: 1,
      pageSize: 50,
    }),
  ),
  http.get('/api/admin/nodes/:id', () =>
    HttpResponse.json(mockNode),
  ),
  http.get('/api/admin/config-sync/out-of-sync', () =>
    HttpResponse.json({ outOfSync: [], total: 0 }),
  ),
  http.get('/api/admin/config-sync/history', () =>
    HttpResponse.json({
      events: [mockConfigHistoryEvent],
      total: 1,
      page: 1,
      pageSize: 20,
    }),
  ),
  http.post('/api/admin/config-sync/update', () =>
    HttpResponse.json({ ok: true, version: 2, nodesNotified: 1, nodesOnline: 1 }),
  ),
  http.post('/api/admin/nodes/:id/resync', async ({ request, params }) => {
    const body = await request.json() as { configKey?: string };
    return HttpResponse.json({ ok: true, nodeId: String(params.id), configKey: body?.configKey ?? '' });
  }),

  // Per-Thing override registry (global view).
  http.get('/api/admin/nodes/overrides', () =>
    HttpResponse.json({
      overrides: [],
      total: 0,
      summary: { totalNodes: 0, totalOverrides: 0, staleCount: 0, expiringSoonCount: 0 },
    }),
  ),
  http.delete('/api/admin/nodes/:id/overrides/:configKey', () =>
    HttpResponse.json({ ok: true }),
  ),
  http.post('/api/admin/nodes/:id/resync', async ({ request, params }) => {
    const body = await request.json().catch(() => ({})) as { configKey?: string };
    return HttpResponse.json({
      ok: true,
      nodeId: String(params.id),
      configKey: body?.configKey,
    });
  }),
  http.get('/api/admin/config-sync/catalog', () =>
    HttpResponse.json({
      entries: [
        { nodeType: 'ai-gateway', configKeys: ['credentials', 'hooks', 'observability', 'routing_rules'] },
        { nodeType: 'compliance-proxy', configKeys: ['exemptions', 'hooks', 'killswitch'] },
      ],
    }),
  ),
  http.get('/api/admin/jobs', () =>
    HttpResponse.json({ jobs: [mockJob, mockJob2] }),
  ),
  http.get('/api/admin/jobs/:id', ({ params }) =>
    HttpResponse.json(params.id === mockJob2.id ? mockJob2 : mockJob),
  ),
  http.get('/api/admin/jobs/:id/runs', () =>
    HttpResponse.json({ runs: [mockJobRun, mockJobRun2] }),
  ),
  http.put('/api/admin/jobs/:id', ({ params }) =>
    HttpResponse.json({ ok: true, jobId: String(params.id), enabled: true }),
  ),
  http.post('/api/admin/jobs/:id/trigger', ({ params }) =>
    HttpResponse.json({ ok: true, jobId: String(params.id), triggeredAt: '2026-04-17T10:00:00Z' }),
  ),

  // Diag events (Recent Errors / Crash Reports pages).
  // The CP `level` filter validates one value at a time, so the Recent Errors
  // page issues two parallel calls (`level=error` and `level=fatal`). The
  // default handler returns one event per requested level so a merged page
  // surfaces both rows out of the box.
  http.get('/api/admin/diag-events', ({ request }) => {
    const u = new URL(request.url);
    const lvl = u.searchParams.get('level');
    if (lvl === 'fatal') {
      return HttpResponse.json({ data: [mockDiagEventFatal], nextCursor: '' });
    }
    if (lvl === 'error') {
      return HttpResponse.json({ data: [mockDiagEventError], nextCursor: '' });
    }
    return HttpResponse.json({
      data: [mockDiagEventError, mockDiagEventFatal],
      nextCursor: '',
    });
  }),
  http.get('/api/admin/diag-events/groups', () =>
    HttpResponse.json({ data: [mockDiagGroupError, mockDiagGroupFatal] }),
  ),
  http.get('/api/admin/diag-events/crash-cohorts', () =>
    HttpResponse.json({ data: [mockCrashCohort1, mockCrashCohort2] }),
  ),
  http.post('/api/admin/agents/:nodeId/diagnostic-mode', ({ params }) =>
    HttpResponse.json({
      window: {
        id: 'win-1',
        nodeId: String(params.nodeId),
        nodeType: 'agent',
        startedAt: '2026-04-27T10:00:00Z',
        endedAt: '2026-04-27T11:00:00Z',
        setBy: 'admin',
        reason: 'from-recent-errors',
        createdAt: '2026-04-27T10:00:00Z',
      },
    }),
  ),
  // Active diag-mode windows list — empty by default; tests override per-case.
  http.get('/api/admin/agents/diagnostic-mode', () =>
    HttpResponse.json({ data: [] }),
  ),
  http.delete('/api/admin/agents/:nodeId/diagnostic-mode', () =>
    HttpResponse.json({ ok: true }),
  ),
  http.post('/api/admin/agents/diagnostic-mode/bulk', () =>
    HttpResponse.json({ ok: true, total: 0, failed: 0, items: [] }),
  ),

  // Ops metrics (Status / Node Detail Metrics tab).
  // The UI calls /current and /timeseries; defaults return empty payloads so
  // pages render their "no data in this window" empty states out of the box.
  // Tests that need populated charts override these per-case.
  http.get('/api/admin/ops-metrics/current', () =>
    HttpResponse.json({ data: [] }),
  ),
  http.get('/api/admin/ops-metrics/timeseries', () =>
    HttpResponse.json({ data: [], granularity: 'raw' }),
  ),
  http.get('/api/admin/ops-metrics/fleet', () =>
    HttpResponse.json({ data: [], granularity: 'raw' }),
  ),

  // Observability retention (Settings page + Node Detail Metrics range chrome).
  // Defaults mirror the spec §5.5 seed values so range buttons render unblocked.
  http.get('/api/admin/observability/retention', () =>
    HttpResponse.json({
      retention: {
        runtime_5m: { value: 7, min: 1, max: 30 },
        runtime_1h: { value: 90, min: 30, max: 365 },
        runtime_1d: { value: 365, min: 90, max: 1095 },
        runtime_1mo: { value: 1825, min: 365, max: 3650 },
        business_5m: { value: 7, min: 1, max: 30 },
        business_1h: { value: 90, min: 30, max: 365 },
        business_1d: { value: 365, min: 90, max: 1095 },
        business_1mo: { value: 1825, min: 365, max: 3650 },
        diag_info: { value: 14, min: 1, max: 90 },
        diag_warn: { value: 30, min: 7, max: 90 },
        diag_error: { value: 180, min: 30, max: 730 },
        diag_fatal: { value: 365, min: 90, max: 1825 },
      },
    }),
  ),

  // Payload capture settings. Defaults match payloadcapture.DefaultConfig so a
  // test that does not override this handler renders the
  // "all off, 64 KiB audit cap, 10 MiB network caps" UI state.
  http.get('/api/admin/settings/payload-capture', () =>
    HttpResponse.json({
      storeRequestBody: false,
      storeResponseBody: false,
      maxInlineBodyBytes: 65536,
      maxRequestBytes: 10 * 1024 * 1024,
      maxResponseBytes: 10 * 1024 * 1024,
    }),
  ),
  http.put('/api/admin/settings/payload-capture', async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as Partial<{
      storeRequestBody: boolean;
      storeResponseBody: boolean;
      maxInlineBodyBytes: number;
      maxRequestBytes: number;
      maxResponseBytes: number;
    }>;
    return HttpResponse.json({
      storeRequestBody: body.storeRequestBody ?? false,
      storeResponseBody: body.storeResponseBody ?? false,
      maxInlineBodyBytes: body.maxInlineBodyBytes ?? 65536,
      maxRequestBytes: body.maxRequestBytes ?? 10 * 1024 * 1024,
      maxResponseBytes: body.maxResponseBytes ?? 10 * 1024 * 1024,
    });
  }),

  http.get('/api/admin/traffic-adapters', () =>
    HttpResponse.json(mockTrafficAdapterCatalog),
  ),

  // Interception domains admin CRUD.
  // Default canned list has one enabled domain with a single PROCESS path, so
  // list-page tests render non-empty without having to override; detail-page
  // tests read the same canned shape via the `/:id` handler.
  http.get('/api/admin/interception-domains', () =>
    HttpResponse.json({
      data: [mockInterceptionDomain],
      total: 1,
    }),
  ),
  http.get('/api/admin/interception-domains/:id', ({ params }) =>
    HttpResponse.json({ ...mockInterceptionDomain, id: String(params.id) }),
  ),
  http.post('/api/admin/interception-domains', async ({ request }) => {
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    return HttpResponse.json(
      { ...mockInterceptionDomain, id: 'new-domain-id', ...body, paths: [] },
      { status: 201 },
    );
  }),
  http.put('/api/admin/interception-domains/:id', async ({ params, request }) => {
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    return HttpResponse.json({
      ...mockInterceptionDomain,
      id: String(params.id),
      ...body,
    });
  }),
  http.delete('/api/admin/interception-domains/:id', () =>
    new HttpResponse(null, { status: 204 }),
  ),
  http.post('/api/admin/interception-domains/:id/paths', async ({ params, request }) => {
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    return HttpResponse.json(
      { ...mockInterceptionPath, id: 'new-path-id', domainId: String(params.id), ...body },
      { status: 201 },
    );
  }),
  http.put('/api/admin/interception-domains/:id/paths/:pathId', async ({ params, request }) => {
    const body = (await request.json().catch(() => ({}))) as Record<string, unknown>;
    return HttpResponse.json({
      ...mockInterceptionPath,
      id: String(params.pathId),
      domainId: String(params.id),
      ...body,
    });
  }),
  http.delete('/api/admin/interception-domains/:id/paths/:pathId', () =>
    new HttpResponse(null, { status: 204 }),
  ),
];
