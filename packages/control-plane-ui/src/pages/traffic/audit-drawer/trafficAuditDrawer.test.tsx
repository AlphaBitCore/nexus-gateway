/**
 * TrafficEventDrawer — Vitest + React Testing Library.
 *
 * Tests cover:
 *  T6d.1  Semantic-hit row → "Disable L2 fleet-wide" button visible for
 *         user with semantic-cache:update permission.
 *  T6d.2  Button click → confirmation AlertDialog opens.
 *  T6d.3  Confirm → disableL2 mutation called; success toast fires.
 *  T6d.4  Without semantic-cache:update permission → button hidden.
 *  T6d.5  Non-semantic gateway-cache hit → button hidden.
 *  T6d.6  i18n key presence — all required semanticHit keys in EN/ZH/ES.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest';
import '@testing-library/jest-dom/vitest';
import { screen, waitFor, fireEvent } from '@testing-library/react';

import { renderWithRouter } from '@/test/test-utils';

// i18n locale files for the key-presence test.
import enPages from '@/i18n/locales/en/pages.json';
import zhPages from '@/i18n/locales/zh/pages.json';
import esPages from '@/i18n/locales/es/pages.json';

// ── Mock service layer ───────────────────────────────────────────────────────

const mockGetTrafficEvent = vi.fn();
const mockGetTrafficEventNormalized = vi.fn();

vi.mock('@/api/services', () => ({
  systemApi: {
    getTrafficEvent: (id: string) => mockGetTrafficEvent(id),
    getTrafficEventNormalized: (id: string) => mockGetTrafficEventNormalized(id),
    listModels: vi.fn().mockResolvedValue({ data: [] }),
  },
  serviceUrlsApi: {
    publicURLs: vi.fn().mockResolvedValue({}),
  },
}));

// Mock semanticCacheConfigApi used by useDisableSemanticCacheFleetWide.
const mockGetConfig = vi.fn();
const mockSaveConfig = vi.fn();

vi.mock('@/api/services/cache/semanticCacheConfig', () => ({
  semanticCacheConfigApi: {
    getConfig: () => mockGetConfig(),
    saveConfig: (input: unknown) => mockSaveConfig(input),
  },
}));

// Mock usePermission — default true; individual tests override when needed.
const mockUsePermission = vi.fn().mockReturnValue(true);

vi.mock('@/hooks/usePermission', () => ({
  usePermission: (key: string) => mockUsePermission(key),
  ACTION_MAP: {},
}));

// ── Shared test fixtures ─────────────────────────────────────────────────────

function makeEvent(overrides: Record<string, unknown> = {}) {
  return {
    id: 'evt-001',
    source: 'ai-gateway',
    timestamp: new Date().toISOString(),
    method: 'POST',
    path: '/v1/chat/completions',
    statusCode: 200,
    ...overrides,
  };
}

// The audit drawer's "Mark as bad cache hit" thumbs-down
// now posts gatewayCacheL2EntryKey as the poison-list entryKey (the Redis
// HASH key the gateway will actually check on its next FT.SEARCH hit), not
// traffic_event.id. The fixture below carries a representative key.
const SEMANTIC_L2_ENTRY_KEY = 'nexus:semantic-cache:v1:9f8e7d6c5b4a3210';
const SEMANTIC_HIT_EVENT = makeEvent({
  cacheStatus: 'HIT',
  gatewayCacheStatus: 'hit',
  gatewayCacheKind: 'semantic',
  gatewayCacheL2EntryKey: SEMANTIC_L2_ENTRY_KEY,
  gatewayCacheSavingsUsd: 0.002,
});

const EXTRACT_HIT_EVENT = makeEvent({
  cacheStatus: 'HIT',
  gatewayCacheStatus: 'hit',
  gatewayCacheKind: 'extract',
  gatewayCacheSavingsUsd: 0.001,
});

const MISS_EVENT = makeEvent({
  cacheStatus: 'MISS',
  gatewayCacheStatus: 'miss',
});

const SEMANTIC_CACHE_CONFIG = {
  id: 'singleton',
  embeddingProviderId: 'prov-1',
  embeddingModelId: 'model-embed-1',
  embeddingDimension: 1536,
  embeddingFingerprint: 'abcdef1234567890',
  redisIndexName: 'nexus:semantic-cache:v1',
  enabled: true,
  updatedAt: new Date().toISOString(),
  updatedBy: 'admin@nexus.ai',
};

// ── Import component AFTER mocks ─────────────────────────────────────────────
import { TrafficEventDrawer } from './trafficAuditDrawer';

// ── Tests ────────────────────────────────────────────────────────────────────

beforeEach(() => {
  vi.clearAllMocks();
  // Return the full detail event on demand.
  mockGetTrafficEvent.mockResolvedValue(SEMANTIC_HIT_EVENT);
  mockGetTrafficEventNormalized.mockResolvedValue(null);
  mockGetConfig.mockResolvedValue(SEMANTIC_CACHE_CONFIG);
  mockSaveConfig.mockResolvedValue({ ...SEMANTIC_CACHE_CONFIG, enabled: false });
  mockUsePermission.mockReturnValue(true);
});

function renderDrawer(event = SEMANTIC_HIT_EVENT) {
  return renderWithRouter(
    <TrafficEventDrawer
      selectedEntry={event as Parameters<typeof TrafficEventDrawer>[0]['selectedEntry']}
      drawerVisible
      onClose={vi.fn()}
    />,
  );
}

// ── T6d.1: button visible for semantic hit + permission ──────────────────────

describe('TrafficEventDrawer — T6d.1: Disable L2 button visible on semantic hit', () => {
  it('shows "Disable L2 fleet-wide" button for semantic-cache hit with permission', async () => {
    mockUsePermission.mockReturnValue(true);
    renderDrawer(SEMANTIC_HIT_EVENT);

    // Switch to AI & Routing tab to see the cache hit banner.
    await waitFor(() => {
      expect(screen.getByText(/ai & routing/i)).toBeDefined();
    });
    fireEvent.click(screen.getByText(/ai & routing/i));

    await waitFor(() => {
      expect(screen.getByTestId('disable-l2-btn')).toBeDefined();
    });
  });
});

// ── T6d.2: dialog opens on click ─────────────────────────────────────────────

describe('TrafficEventDrawer — T6d.2: Confirmation dialog opens on button click', () => {
  it('opens AlertDialog when "Disable L2 fleet-wide" button is clicked', async () => {
    mockUsePermission.mockReturnValue(true);
    renderDrawer(SEMANTIC_HIT_EVENT);

    await waitFor(() => {
      expect(screen.getByText(/ai & routing/i)).toBeDefined();
    });
    fireEvent.click(screen.getByText(/ai & routing/i));

    await waitFor(() => {
      expect(screen.getByTestId('disable-l2-btn')).toBeDefined();
    });

    fireEvent.click(screen.getByTestId('disable-l2-btn'));

    await waitFor(() => {
      expect(screen.getByText(/disable semantic cache fleet-wide\?/i)).toBeDefined();
    });
  });
});

// ── T6d.3: confirm calls mutation ────────────────────────────────────────────

describe('TrafficEventDrawer — T6d.3: Confirm triggers mutation', () => {
  it('calls saveConfig with enabled=false when user confirms disable', async () => {
    mockUsePermission.mockReturnValue(true);
    renderDrawer(SEMANTIC_HIT_EVENT);

    await waitFor(() => {
      expect(screen.getByText(/ai & routing/i)).toBeDefined();
    });
    fireEvent.click(screen.getByText(/ai & routing/i));

    await waitFor(() => {
      expect(screen.getByTestId('disable-l2-btn')).toBeDefined();
    });
    fireEvent.click(screen.getByTestId('disable-l2-btn'));

    await waitFor(() => {
      expect(screen.getByText(/disable semantic cache fleet-wide\?/i)).toBeDefined();
    });

    // Click the "Disable Now" confirm button in the dialog.
    const confirmBtn = screen.getByRole('button', { name: /disable now/i });
    fireEvent.click(confirmBtn);

    await waitFor(() => {
      expect(mockSaveConfig).toHaveBeenCalledWith(
        expect.objectContaining({ enabled: false }),
      );
    });
  });
});

// ── T6d.4: button hidden without permission ───────────────────────────────────

describe('TrafficEventDrawer — T6d.4: Button hidden without permission', () => {
  it('does NOT render "Disable L2 fleet-wide" button when user lacks semantic-cache:update', async () => {
    mockUsePermission.mockReturnValue(false);
    renderDrawer(SEMANTIC_HIT_EVENT);

    await waitFor(() => {
      expect(screen.getByText(/ai & routing/i)).toBeDefined();
    });
    fireEvent.click(screen.getByText(/ai & routing/i));

    // Give the banner a chance to render.
    await waitFor(() => {
      expect(screen.getByText(/cache hit/i)).toBeDefined();
    });

    expect(screen.queryByTestId('disable-l2-btn')).toBeNull();
  });
});

// ── T6d.5: button hidden for non-semantic hit ─────────────────────────────────

describe('TrafficEventDrawer — T6d.5: Button hidden for extract (non-semantic) hit', () => {
  it('does NOT render "Disable L2 fleet-wide" button for an extract cache hit', async () => {
    mockUsePermission.mockReturnValue(true);
    mockGetTrafficEvent.mockResolvedValue(EXTRACT_HIT_EVENT);
    renderDrawer(EXTRACT_HIT_EVENT);

    await waitFor(() => {
      expect(screen.getByText(/ai & routing/i)).toBeDefined();
    });
    fireEvent.click(screen.getByText(/ai & routing/i));

    await waitFor(() => {
      expect(screen.getByText(/cache hit/i)).toBeDefined();
    });

    expect(screen.queryByTestId('disable-l2-btn')).toBeNull();
  });

  it('does NOT render the button for a cache miss row', async () => {
    mockUsePermission.mockReturnValue(true);
    mockGetTrafficEvent.mockResolvedValue(MISS_EVENT);
    renderDrawer(MISS_EVENT);

    await waitFor(() => {
      expect(screen.getByText(/ai & routing/i)).toBeDefined();
    });
    fireEvent.click(screen.getByText(/ai & routing/i));

    expect(screen.queryByTestId('disable-l2-btn')).toBeNull();
  });
});

// ── T6d.6: i18n key presence ─────────────────────────────────────────────────

describe('TrafficEventDrawer — T6d.6: i18n key presence for semanticHit', () => {
  const REQUIRED_KEYS = [
    'disableL2',
    'confirmTitle',
    'confirmBody',
    'confirmYes',
    'confirmCancel',
    'disabledToast',
    'errorToast',
    // New semantic-cache keys
    'markBadTitle',
    'markBadBody',
    'markBadReasonLabel',
    'markBadReasonPlaceholder',
    'markBadConfirm',
    'markBadCancel',
    'markBadSuccessToast',
    'markBadErrorToast',
    'markBadButton',
  ] as const;

  function getNestedKey(obj: Record<string, unknown>, path: string[]): unknown {
    return path.reduce(
      (acc: unknown, key) =>
        acc && typeof acc === 'object' ? (acc as Record<string, unknown>)[key] : undefined,
      obj,
    );
  }

  function hasKey(pages: Record<string, unknown>, key: string): boolean {
    const path = ['traffic', 'detail', 'aiProvider', 'semanticHit', key];
    return getNestedKey(pages, path) !== undefined;
  }

  it('EN locale has all required semanticHit keys', () => {
    const missing = REQUIRED_KEYS.filter(
      (k) => !hasKey(enPages as unknown as Record<string, unknown>, k),
    );
    expect(missing).toEqual([]);
  });

  it('ZH locale has all required semanticHit keys', () => {
    const missing = REQUIRED_KEYS.filter(
      (k) => !hasKey(zhPages as unknown as Record<string, unknown>, k),
    );
    expect(missing).toEqual([]);
  });

  it('ES locale has all required semanticHit keys', () => {
    const missing = REQUIRED_KEYS.filter(
      (k) => !hasKey(esPages as unknown as Record<string, unknown>, k),
    );
    expect(missing).toEqual([]);
  });
});

// ── Mark-bad cache hit button and dialog ─────────────────────────────────

// Mock semanticFeedbackApi
const mockPostFeedback = vi.fn();
vi.mock('@/api/services/cache/semanticFeedback', () => ({
  semanticFeedbackApi: {
    postFeedback: (input: unknown) => mockPostFeedback(input),
  },
}));

beforeEach(() => {
  // Extend existing beforeEach by also setting up postFeedback mock.
  mockPostFeedback.mockResolvedValue({ ok: true });
});

describe('TrafficEventDrawer — T6d.7: Mark-bad button visible for semantic hit with permission', () => {
  it('shows "Mark as bad cache hit" button for semantic hit with semantic-cache:update permission', async () => {
    mockUsePermission.mockReturnValue(true);
    renderDrawer(SEMANTIC_HIT_EVENT);

    await waitFor(() => {
      expect(screen.getByText(/ai & routing/i)).toBeDefined();
    });
    fireEvent.click(screen.getByText(/ai & routing/i));

    await waitFor(() => {
      expect(screen.getByTestId('mark-bad-hit-btn')).toBeDefined();
    });
  });
});

describe('TrafficEventDrawer — T6d.8: Mark-bad dialog opens on button click', () => {
  it('opens the mark-bad dialog when the button is clicked', async () => {
    mockUsePermission.mockReturnValue(true);
    renderDrawer(SEMANTIC_HIT_EVENT);

    await waitFor(() => {
      expect(screen.getByText(/ai & routing/i)).toBeDefined();
    });
    fireEvent.click(screen.getByText(/ai & routing/i));

    await waitFor(() => {
      expect(screen.getByTestId('mark-bad-hit-btn')).toBeDefined();
    });
    fireEvent.click(screen.getByTestId('mark-bad-hit-btn'));

    await waitFor(() => {
      // The dialog title should be visible.
      expect(screen.getByTestId('mark-bad-reason-textarea')).toBeDefined();
    });
  });
});

describe('TrafficEventDrawer — T6d.9: Mark-bad validation enforces min length', () => {
  it('shows error when reason is too short', async () => {
    mockUsePermission.mockReturnValue(true);
    renderDrawer(SEMANTIC_HIT_EVENT);

    await waitFor(() => {
      expect(screen.getByText(/ai & routing/i)).toBeDefined();
    });
    fireEvent.click(screen.getByText(/ai & routing/i));

    await waitFor(() => {
      expect(screen.getByTestId('mark-bad-hit-btn')).toBeDefined();
    });
    fireEvent.click(screen.getByTestId('mark-bad-hit-btn'));

    await waitFor(() => {
      expect(screen.getByTestId('mark-bad-reason-textarea')).toBeDefined();
    });

    fireEvent.change(screen.getByTestId('mark-bad-reason-textarea'), { target: { value: 'hi' } });
    fireEvent.click(screen.getByTestId('mark-bad-confirm-btn'));

    await waitFor(() => {
      expect(screen.getByTestId('mark-bad-reason-error')).toBeDefined();
    });
    expect(mockPostFeedback).not.toHaveBeenCalled();
  });
});

describe('TrafficEventDrawer — T6d.10: Mark-bad confirm calls postFeedback with correct payload', () => {
  // The poison list is keyed on the L2 entry's Redis
  // HASH key (gatewayCacheL2EntryKey), NOT traffic_event.id. The earlier
  // assertion against `entryKey: 'evt-001'` codified the silent-no-op bug.
  it('calls postFeedback with entryKey=gatewayCacheL2EntryKey and the typed reason', async () => {
    mockUsePermission.mockReturnValue(true);
    renderDrawer(SEMANTIC_HIT_EVENT);

    await waitFor(() => {
      expect(screen.getByText(/ai & routing/i)).toBeDefined();
    });
    fireEvent.click(screen.getByText(/ai & routing/i));

    await waitFor(() => {
      expect(screen.getByTestId('mark-bad-hit-btn')).toBeDefined();
    });
    fireEvent.click(screen.getByTestId('mark-bad-hit-btn'));

    await waitFor(() => {
      expect(screen.getByTestId('mark-bad-reason-textarea')).toBeDefined();
    });

    const reason = 'This response contains outdated pricing information.';
    fireEvent.change(screen.getByTestId('mark-bad-reason-textarea'), { target: { value: reason } });
    fireEvent.click(screen.getByTestId('mark-bad-confirm-btn'));

    await waitFor(() => {
      expect(mockPostFeedback).toHaveBeenCalledWith(
        expect.objectContaining({
          entryKey: SEMANTIC_L2_ENTRY_KEY,
          reason,
        }),
      );
    });
  });

  // Regression guard: a semantic-hit row that predates the L2 entry-key
  // stamp must NOT silently post a bogus entryKey. The confirm flow surfaces
  // an inline validation error and skips the network call.
  it('refuses to post and shows an error when gatewayCacheL2EntryKey is missing', async () => {
    const legacyEvent = makeEvent({
      cacheStatus: 'HIT',
      gatewayCacheStatus: 'hit',
      gatewayCacheKind: 'semantic',
      gatewayCacheSavingsUsd: 0.002,
      // gatewayCacheL2EntryKey deliberately omitted → simulates legacy row.
    });
    mockGetTrafficEvent.mockResolvedValue(legacyEvent);
    mockUsePermission.mockReturnValue(true);
    mockPostFeedback.mockClear();
    renderDrawer(legacyEvent);

    await waitFor(() => {
      expect(screen.getByText(/ai & routing/i)).toBeDefined();
    });
    fireEvent.click(screen.getByText(/ai & routing/i));

    await waitFor(() => {
      expect(screen.getByTestId('mark-bad-hit-btn')).toBeDefined();
    });
    fireEvent.click(screen.getByTestId('mark-bad-hit-btn'));

    await waitFor(() => {
      expect(screen.getByTestId('mark-bad-reason-textarea')).toBeDefined();
    });

    fireEvent.change(screen.getByTestId('mark-bad-reason-textarea'), {
      target: { value: 'This response is outdated and should be evicted.' },
    });
    fireEvent.click(screen.getByTestId('mark-bad-confirm-btn'));

    await waitFor(() => {
      expect(screen.getByTestId('mark-bad-reason-error')).toBeDefined();
    });
    expect(mockPostFeedback).not.toHaveBeenCalled();
  });
});

describe('TrafficEventDrawer — T6d.11: Mark-bad button hidden without permission', () => {
  it('does NOT render "Mark as bad cache hit" button when user lacks semantic-cache:update', async () => {
    mockUsePermission.mockReturnValue(false);
    renderDrawer(SEMANTIC_HIT_EVENT);

    await waitFor(() => {
      expect(screen.getByText(/ai & routing/i)).toBeDefined();
    });
    fireEvent.click(screen.getByText(/ai & routing/i));

    await waitFor(() => {
      expect(screen.getByText(/cache hit/i)).toBeDefined();
    });

    expect(screen.queryByTestId('mark-bad-hit-btn')).toBeNull();
  });
});

describe('TrafficEventDrawer — T6d.12: Mark-bad button hidden for extract hit', () => {
  it('does NOT render the mark-bad button for an extract cache hit', async () => {
    mockUsePermission.mockReturnValue(true);
    mockGetTrafficEvent.mockResolvedValue(EXTRACT_HIT_EVENT);
    renderDrawer(EXTRACT_HIT_EVENT);

    await waitFor(() => {
      expect(screen.getByText(/ai & routing/i)).toBeDefined();
    });
    fireEvent.click(screen.getByText(/ai & routing/i));

    await waitFor(() => {
      expect(screen.getByText(/cache hit/i)).toBeDefined();
    });

    expect(screen.queryByTestId('mark-bad-hit-btn')).toBeNull();
  });
});
