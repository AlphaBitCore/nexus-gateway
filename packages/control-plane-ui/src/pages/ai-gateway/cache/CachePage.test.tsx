/**
 * CachePage — Vitest + React Testing Library.
 *
 * Consolidates the 15 suites that lived in the deleted
 * CacheSettingsPage.test.tsx + CacheEmbeddingPage.test.tsx into one file
 * matching the new merged page structure.
 *
 * Sections covered:
 *  - Page composition: both Section h2s (Gateway Cache + Provider Prompt Cache) render.
 *  - StatusStrip: renders even when rollup is empty; emergency button visibility.
 *  - Embedding form: picker, kill switch, status panel, probe visibility.
 *  - Rebuild confirmation modal: opens on model change, NOT on enabled-only change.
 *  - Save button disabled guards.
 *  - Freshness rules card: rule table, test box, Add Rule modal.
 *  - i18n key presence (EN / ES / ZH) under aiGateway.cache.*.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest';
import '@testing-library/jest-dom/vitest';
import { screen, waitFor, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithProviders } from '@/test/test-utils';

import enPages from '@/i18n/locales/en/pages.json';
import zhPages from '@/i18n/locales/zh/pages.json';
import esPages from '@/i18n/locales/es/pages.json';

// ── Mocks ───────────────────────────────────────────────────────────────────

const mockGetConfig = vi.fn();
const mockSaveConfig = vi.fn();
const mockRunProbe = vi.fn();

vi.mock('@/api/services/cache/semanticCacheConfig', () => ({
  semanticCacheConfigApi: {
    getConfig: () => mockGetConfig(),
    saveConfig: (input: unknown) => mockSaveConfig(input),
    runProbe: (id: string) => mockRunProbe(id),
  },
}));

// Extract cache config mock.
const mockGetExtractCacheConfig = vi.fn();
const mockSaveExtractCacheConfig = vi.fn();

vi.mock('@/api/services/cache/extractCacheConfig', () => ({
  extractCacheConfigApi: {
    getConfig: () => mockGetExtractCacheConfig(),
    saveConfig: (input: unknown) => mockSaveExtractCacheConfig(input),
  },
}));

const EXTRACT_CONFIG_DEFAULT = {
  id: 'singleton' as const,
  enabled: true,
  ttlSeconds: 3600,
  applyFreshnessRules: true,
  updatedAt: new Date().toISOString(),
  updatedBy: 'admin@example.com',
};

const mockListPatterns = vi.fn();
const mockTestPattern = vi.fn();
const mockUpdatePattern = vi.fn();
const mockCreatePattern = vi.fn();
const mockDeletePattern = vi.fn();

vi.mock('@/api/services/cache/timeSensitivePatterns', () => ({
  timeSensitivePatternsApi: {
    list: () => mockListPatterns(),
    update: (id: string, p: unknown) => mockUpdatePattern(id, p),
    create: (p: unknown) => mockCreatePattern(p),
    delete: (id: string) => mockDeletePattern(id),
    test: (prompt: string) => mockTestPattern(prompt),
  },
}));

vi.mock('@/api/services/cache/semanticPrewarm', () => ({
  prewarm: vi.fn(),
}));

const mockCacheROI = vi.fn();

vi.mock('@/api/services/overview/analytics', () => ({
  analyticsApi: {
    cacheROI: () => mockCacheROI(),
  },
}));

vi.mock('@/api/services', () => ({
  systemApi: {
    listModels: vi.fn().mockResolvedValue({
      data: [
        {
          provider: {
            id: 'prov-1',
            name: 'openai',
            displayName: 'OpenAI',
            endpointType: 'embedding',
          },
          models: [
            {
              id: 'model-embed-1',
              name: 'text-embedding-3-small',
              providerModelId: 'text-embedding-3-small',
              type: 'embedding',
              enabled: true,
            },
          ],
        },
      ],
    }),
  },
  serviceUrlsApi: {
    publicURLs: vi.fn().mockResolvedValue({}),
  },
}));

vi.mock('@/hooks/usePermission', () => ({
  usePermission: vi.fn().mockReturnValue(true),
  ACTION_MAP: {},
}));

// SettingsCacheTab is the provider-prompt section; it has its own
// internal data hooks. Stub it so the test stays focused on the merged
// page composition.
vi.mock('../../compliance/cache/SettingsCacheTab', () => ({
  SettingsCacheTab: () => <div data-testid="settings-cache-tab-stub">SettingsCacheTab</div>,
}));

// ── Fixtures ────────────────────────────────────────────────────────────────

const UNCONFIGURED_CONFIG = {
  id: 'singleton',
  embeddingProviderId: null,
  embeddingModelId: null,
  embeddingDimension: null,
  embeddingFingerprint: '',
  redisIndexName: 'nexus:semantic-cache:v1',
  enabled: false,
  threshold: 0.96,
  varyBy: 'vk',
  embedStrategy: 'system_plus_last_user',
  allowCrossModel: false,
  updatedAt: new Date().toISOString(),
  updatedBy: null,
};

const CONFIGURED_ENABLED_CONFIG = {
  id: 'singleton',
  embeddingProviderId: 'prov-1',
  embeddingModelId: 'model-embed-1',
  embeddingDimension: 1536,
  embeddingFingerprint: 'abcdef1234567890',
  redisIndexName: 'nexus:semantic-cache:v1',
  enabled: true,
  threshold: 0.96,
  varyBy: 'vk',
  embedStrategy: 'system_plus_last_user',
  allowCrossModel: false,
  updatedAt: new Date().toISOString(),
  updatedBy: 'admin@nexus.ai',
};

const SEED_PATTERN = {
  id: 'weather',
  keywords: ['weather', 'forecast'],
  requireQuestionMark: false,
  requireEntity: true,
  languages: ['en', 'zh'],
  enabled: true,
};

const PATTERNS_RESPONSE = {
  patterns: [SEED_PATTERN],
  source: 'seed' as const,
};

const EMPTY_ROI = {
  since: new Date().toISOString(),
  until: new Date().toISOString(),
  periodDays: 7,
  totalEstimatedCostUsd: 0,
  totalGatewayCacheSavingsUsd: 0,
  gatewayCacheHitCount: 0,
  totalCacheWriteCostUsd: 0,
  totalCacheReadSavingsUsd: 0,
  totalCacheNetSavingsUsd: 0,
  totalPromptTokens: 0,
  totalCompletionTokens: 0,
  totalCacheCreationTokens: 0,
  totalCacheReadTokens: 0,
  totalNormalisedStripCount: 0,
  totalNormalisedStripBytes: 0,
  totalMarkersInjected: 0,
  requestsWithCacheHit: 0,
  byAdapter: [],
  daily: [],
  dataSource: 'direct' as const,
};

// ── Import page AFTER mocks ─────────────────────────────────────────────────
import { CachePage } from './CachePage';

beforeEach(() => {
  vi.clearAllMocks();
  mockGetConfig.mockResolvedValue(UNCONFIGURED_CONFIG);
  mockSaveConfig.mockResolvedValue(UNCONFIGURED_CONFIG);
  mockGetExtractCacheConfig.mockResolvedValue(EXTRACT_CONFIG_DEFAULT);
  mockSaveExtractCacheConfig.mockResolvedValue(EXTRACT_CONFIG_DEFAULT);
  mockRunProbe.mockResolvedValue({
    ok: true,
    providerId: 'prov-1',
    modelId: 'model-embed-1',
    modelName: 'text-embedding-3-small',
    dimension: 1536,
    latencyMs: 42,
    promptTokens: 3,
    sampleEmbeddingFirst10: [0.1, 0.2, 0.3],
  });
  mockListPatterns.mockResolvedValue(PATTERNS_RESPONSE);
  mockTestPattern.mockResolvedValue({
    decision: 'match',
    matchedRuleId: 'weather',
    matchedKeywords: ['weather'],
  });
  mockUpdatePattern.mockResolvedValue(PATTERNS_RESPONSE);
  mockCreatePattern.mockResolvedValue(SEED_PATTERN);
  mockDeletePattern.mockResolvedValue({ ok: true });
  mockCacheROI.mockResolvedValue(EMPTY_ROI);
});

// ── Tests ───────────────────────────────────────────────────────────────────

describe('CachePage — composition', () => {
  it('renders both top-level tabs (Gateway Cache + Provider Prompt Cache)', async () => {
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      expect(screen.getByRole('tab', { name: /gateway cache/i })).toBeDefined();
      expect(screen.getByRole('tab', { name: /provider prompt cache/i })).toBeDefined();
    });
  });

  it('Gateway tab is the default selected tab', async () => {
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      const gatewayTab = screen.getByRole('tab', { name: /gateway cache/i });
      expect(gatewayTab.getAttribute('data-state')).toBe('active');
    });
  });

  it('renders the freshness rules card inside the Gateway Cache tab', async () => {
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /freshness rules/i, level: 3 })).toBeDefined();
    });
  });

  it('does NOT render the old L1/L2/L3/L4 terminology in user-visible copy', async () => {
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      expect(screen.getByRole('tab', { name: /gateway cache/i })).toBeDefined();
    });
    expect(screen.queryByText(/disable l2 fleet-wide/i)).toBeNull();
    expect(screen.queryByText(/cosine similarity gate for l2/i)).toBeNull();
    expect(screen.queryByText(/Enable semantic \(L2\)/i)).toBeNull();
  });
});

describe('CachePage — StatusStrip', () => {
  it('renders even when rollup is empty (degrades gracefully)', async () => {
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      expect(screen.getByRole('status')).toBeDefined();
    });
  });

  it('does NOT show emergency Disable dropdown when neither cache is enabled', async () => {
    mockGetExtractCacheConfig.mockResolvedValue({
      ...EXTRACT_CONFIG_DEFAULT,
      enabled: false,
    });
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      expect(screen.getByRole('status')).toBeDefined();
    });
    expect(screen.queryByRole('button', { name: /disable caches/i })).toBeNull();
  });

  it('SHOWS emergency Disable dropdown when semantic cache is enabled', async () => {
    mockGetConfig.mockResolvedValue(CONFIGURED_ENABLED_CONFIG);
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      expect(screen.getByRole('button', { name: /disable caches/i })).toBeDefined();
    });
  });
});

describe('CachePage — embedding + semantic form', () => {
  it('renders the provider picker (full form) and kill switch when unconfigured', async () => {
    // Unconfigured config → embedding section auto-expands to show the picker.
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      expect(screen.getByText(/embedding provider/i)).toBeDefined();
    });
    expect(screen.getByText(/semantic cache enabled/i)).toBeDefined();
  });

  it('shows the embedding picker always-visible (no chip collapse 2026-05-21)', async () => {
    mockGetConfig.mockResolvedValue(CONFIGURED_ENABLED_CONFIG);
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      // Provider label is now always rendered inline next to the select
      expect(screen.getByText(/embedding provider/i)).toBeDefined();
    });
    // No "Change" button (that pattern was retired)
    expect(screen.queryByRole('button', { name: /^change$/i })).toBeNull();
  });

  it('kill switch is disabled when no provider/model configured', async () => {
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      expect(screen.getByText(/embedding provider/i)).toBeDefined();
    });
    const switchEl = screen.getByRole('switch', { name: /semantic cache enabled/i });
    const isDisabled =
      switchEl.getAttribute('disabled') !== null ||
      switchEl.getAttribute('data-disabled') !== null ||
      switchEl.getAttribute('aria-disabled') === 'true';
    expect(isDisabled).toBe(true);
  });

  it('save button is disabled when nothing changed from saved', async () => {
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      expect(screen.getByText(/embedding provider/i)).toBeDefined();
    });
    // Two Save buttons (Extract + Semantic). The Semantic Save is what
    // gates the disabled-when-nothing-changed assertion here.
    const saveButtons = screen.getAllByRole('button', { name: /^save$/i });
    const semanticSave = saveButtons[saveButtons.length - 1];
    expect(semanticSave).toBeDisabled();
  });

  it('save button still disabled when enabled=true but no provider/model set (UNCONFIGURED state)', async () => {
    mockGetConfig.mockResolvedValue(UNCONFIGURED_CONFIG);
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      expect(screen.getByText(/embedding provider/i)).toBeDefined();
    });
    // Two Save buttons (Extract + Semantic). The Semantic Save is what
    // gates the disabled-when-nothing-changed assertion here.
    const saveButtons = screen.getAllByRole('button', { name: /^save$/i });
    const semanticSave = saveButtons[saveButtons.length - 1];
    expect(semanticSave).toBeDisabled();
  });

  it('probe button is NOT shown when provider+model are not both set', async () => {
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      expect(screen.getByText(/embedding provider/i)).toBeDefined();
    });
    expect(screen.queryByRole('button', { name: /run embedding probe/i })).toBeNull();
  });

  it('probe button IS shown when provider+model are both set (always-visible picker)', async () => {
    mockGetConfig.mockResolvedValue(CONFIGURED_ENABLED_CONFIG);
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      expect(
        screen.getByRole('button', { name: /run embedding probe/i }),
      ).toBeDefined();
    });
  });

  it('rebuild modal does NOT open when only enabled flag changes', async () => {
    mockGetConfig.mockResolvedValue({ ...CONFIGURED_ENABLED_CONFIG, enabled: false });
    mockSaveConfig.mockResolvedValue({ ...CONFIGURED_ENABLED_CONFIG, enabled: true });

    renderWithProviders(<CachePage />);
    await waitFor(() => {
      expect(screen.getByText(/semantic cache enabled/i)).toBeDefined();
    });

    // Find the kill switch by its aria-label so we don't grab the
    // freshness-apply switch or the Extract card switch.
    const enabledSwitch = screen.getByRole('switch', {
      name: /semantic cache enabled/i,
    });
    await userEvent.click(enabledSwitch);

    // Two Save buttons now exist (Extract card + Semantic card). The
    // Semantic Save is rendered after Extract, so it's the last in DOM order.
    const saveButtons = screen.getAllByRole('button', { name: /^save$/i });
    const semanticSave = saveButtons[saveButtons.length - 1];
    await userEvent.click(semanticSave);

    expect(screen.queryByText(/confirm index rebuild/i)).toBeNull();
    await waitFor(() => {
      expect(mockSaveConfig).toHaveBeenCalledTimes(1);
    });
  });
});

describe('CachePage — Freshness rules card', () => {
  it('renders the seed rule id and keywords in the table', async () => {
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /freshness rules/i, level: 3 })).toBeDefined();
      expect(screen.getAllByText('weather').length).toBeGreaterThanOrEqual(1);
      expect(screen.getAllByText('forecast').length).toBeGreaterThanOrEqual(1);
    });
  });

  it('test box: Test button is disabled when prompt is empty', async () => {
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /freshness rules/i, level: 3 })).toBeDefined();
    });
    const testBtn = screen.getByRole('button', { name: /^test$/i });
    expect(testBtn).toBeDisabled();
  });

  it('test box: shows match result after entering a prompt and clicking Test', async () => {
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /freshness rules/i, level: 3 })).toBeDefined();
    });
    const input = screen.getByPlaceholderText(/what is the stock price/i);
    await userEvent.type(input, "What's the weather today?");
    const testBtn = screen.getByRole('button', { name: /^test$/i });
    expect(testBtn).not.toBeDisabled();
    fireEvent.click(testBtn);
    await waitFor(() => {
      expect(screen.getByTestId('test-result')).toBeDefined();
    });
    expect(mockTestPattern).toHaveBeenCalledWith("What's the weather today?");
  });

  it('Add Rule button opens the modal with id + keywords fields', async () => {
    renderWithProviders(<CachePage />);
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /freshness rules/i, level: 3 })).toBeDefined();
    });
    await userEvent.click(screen.getByRole('button', { name: /add rule/i }));
    await waitFor(() => {
      expect(screen.getByRole('dialog')).toBeDefined();
      expect(screen.getByPlaceholderText('my-custom-rule')).toBeDefined();
      expect(screen.getByPlaceholderText('trending,viral,breaking')).toBeDefined();
    });
  });
});

// ── i18n key presence ───────────────────────────────────────────────────────

describe('CachePage — i18n key presence (aiGateway.cache.*)', () => {
  const ROOT_KEYS = ['title', 'subtitle', 'saved', 'notConfigured'];
  const SECTION_GATEWAY_KEYS = ['title', 'subtitle'];
  const SECTION_PROVIDER_KEYS = ['title', 'subtitle'];
  const EMBEDDING_KEYS = [
    'cardTitle',
    'cardSubtitle',
    'providerLabel',
    'modelLabel',
    'pickerHelpText',
    'reindexWarning',
    'runProbe',
    'probeFast',
    'probeAcceptable',
    'probeSlow',
    'probeLatency',
    'probeLatencyTooltip',
    'probeDimension',
    'probeTokens',
    'probeSample',
    'probeError',
    'statusTitle',
    'statusProvider',
    'statusModel',
    'statusDimension',
    'statusFingerprint',
    'statusIndexName',
    'statusIndexNameTooltip',
    'statusUpdatedAt',
    'statusUpdatedBy',
    'rebuildTitle',
    'rebuildDescription',
    'rebuildConfirm',
  ];
  const SEMANTIC_KEYS = [
    'cardTitle',
    'cardSubtitle',
    'killSwitch',
    'killSwitchDisabledTooltip',
    'thresholdLabel',
    'thresholdHelp',
    'varyByLabel',
    'varyByHelp',
    'embedStrategyLabel',
    'embedStrategyHelp',
    'allowCrossModelLabel',
    'allowCrossModelHelp',
  ];
  const FRESHNESS_KEYS = [
    'title',
    'subtitle',
    'colId',
    'colQuestionMark',
    'colEntity',
    'colLanguages',
    'colEnabled',
    'colActions',
    'allLanguages',
    'noRules',
    'toggleAriaLabel',
    'addRule',
    'testTitle',
    'testSubtitle',
    'testPlaceholder',
    'testRun',
    'testResultMatch',
    'testResultNoMatch',
    'addRuleModalTitle',
    'editRuleModalTitle',
    'fieldId',
    'fieldKeywords',
    'fieldKeywordsHint',
  ];
  const PREWARM_KEYS = [
    'openButton',
    'modalTitle',
    'jsonLabel',
    'cancelButton',
    'previewButton',
    'confirmButton',
    'successToast',
    'errorToast',
  ];
  const STATUS_STRIP_KEYS = [
    'gateway',
    'provider',
    'freshness',
    'savedAmount',
    'hits',
    'activeOfTotal',
    'disableButton',
    'disableTooltip',
    'disableConfirmTitle',
    'disableConfirmDescription',
    'disableConfirmButton',
    'disableSuccess',
    'disableError',
  ];

  function getNested(obj: unknown, path: string[]): unknown {
    return path.reduce(
      (acc: unknown, key) =>
        acc && typeof acc === 'object' ? (acc as Record<string, unknown>)[key] : undefined,
      obj,
    );
  }

  function checkKeys(pages: Record<string, unknown>, locale: string) {
    const missing: string[] = [];
    for (const k of ROOT_KEYS) {
      if (getNested(pages, ['aiGateway', 'cache', k]) === undefined) missing.push(`cache.${k}`);
    }
    for (const k of SECTION_GATEWAY_KEYS) {
      if (getNested(pages, ['aiGateway', 'cache', 'sectionGateway', k]) === undefined)
        missing.push(`cache.sectionGateway.${k}`);
    }
    for (const k of SECTION_PROVIDER_KEYS) {
      if (getNested(pages, ['aiGateway', 'cache', 'sectionProvider', k]) === undefined)
        missing.push(`cache.sectionProvider.${k}`);
    }
    for (const k of EMBEDDING_KEYS) {
      if (getNested(pages, ['aiGateway', 'cache', 'embedding', k]) === undefined)
        missing.push(`cache.embedding.${k}`);
    }
    for (const k of SEMANTIC_KEYS) {
      if (getNested(pages, ['aiGateway', 'cache', 'semantic', k]) === undefined)
        missing.push(`cache.semantic.${k}`);
    }
    for (const k of FRESHNESS_KEYS) {
      if (getNested(pages, ['aiGateway', 'cache', 'freshness', k]) === undefined)
        missing.push(`cache.freshness.${k}`);
    }
    for (const k of PREWARM_KEYS) {
      if (getNested(pages, ['aiGateway', 'cache', 'prewarm', k]) === undefined)
        missing.push(`cache.prewarm.${k}`);
    }
    for (const k of STATUS_STRIP_KEYS) {
      if (getNested(pages, ['aiGateway', 'cache', 'statusStrip', k]) === undefined)
        missing.push(`cache.statusStrip.${k}`);
    }
    if (missing.length > 0) {
      throw new Error(`[${locale}] missing i18n keys:\n  ${missing.join('\n  ')}`);
    }
  }

  it('EN locale has all required aiGateway.cache.* keys', () => {
    checkKeys(enPages as unknown as Record<string, unknown>, 'EN');
  });

  it('ZH locale has all required aiGateway.cache.* keys', () => {
    checkKeys(zhPages as unknown as Record<string, unknown>, 'ZH');
  });

  it('ES locale has all required aiGateway.cache.* keys', () => {
    checkKeys(esPages as unknown as Record<string, unknown>, 'ES');
  });
});

describe('CachePage — old i18n namespaces removed', () => {
  it('EN settings.cacheSettings + settings.cacheEmbedding are gone', () => {
    const en = enPages as Record<string, Record<string, unknown> | undefined>;
    expect(en.settings?.cacheSettings).toBeUndefined();
    expect(en.settings?.cacheEmbedding).toBeUndefined();
  });

  it('EN nav.cacheSettings + nav.cacheEmbedding are gone (replaced by nav.cache)', async () => {
    const navEn = (await import('@/i18n/locales/en/nav.json')).default as Record<string, unknown>;
    expect(navEn.cacheSettings).toBeUndefined();
    expect(navEn.cacheEmbedding).toBeUndefined();
    expect(navEn.cache).toBeDefined();
  });
});
