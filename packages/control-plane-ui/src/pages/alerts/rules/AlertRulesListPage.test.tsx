/**
 * AlertRulesListPage — integration tests.
 *
 * Stubs `alertsApi.listRules` / `alertsApi.updateRule` at the module
 * boundary and asserts:
 *   - rule rows render
 *   - toggling the Enabled Switch fires updateRule(id, { enabled: next })
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import '@testing-library/jest-dom/vitest';

import { renderWithRouter } from '@/test/test-utils';
import { alertsApi } from '@/api/services/alerts/alerts';
import type { AlertRule } from '@/api/services/alerts/alerts';
import { AlertRulesListPage } from './AlertRulesListPage';

function sampleRule(overrides: Partial<AlertRule> = {}): AlertRule {
  return {
    id: 'quota.threshold',
    displayName: 'Quota Threshold Crossed',
    sourceType: 'quota',
    defaultSeverity: 'high',
    requiresAck: true,
    enabled: true,
    params: { thresholds: [80, 95] },
    paramsSchema: {
      type: 'object',
      properties: {
        thresholds: { type: 'array', items: { type: 'integer' } },
      },
    },
    cooldownSec: 300,
    updatedAt: '2026-04-22T10:00:00Z',
    ...overrides,
  };
}

describe('AlertRulesListPage', () => {
  beforeEach(() => {
    vi.spyOn(alertsApi, 'listRules').mockResolvedValue({
      rules: [
        sampleRule(),
        sampleRule({
          id: 'quota.vk_expiring',
          displayName: 'Virtual Key Expiring',
          defaultSeverity: 'medium',
          requiresAck: false,
          enabled: false,
          params: { warnDays: [30, 7] },
        }),
      ],
      total: 2,
      limit: 20,
      offset: 0,
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('renders a row per rule', async () => {
    renderWithRouter(<AlertRulesListPage />);
    expect(await screen.findByText('Quota Threshold Crossed')).toBeInTheDocument();
    expect(screen.getByText('Virtual Key Expiring')).toBeInTheDocument();
    expect(screen.getByText('quota.threshold')).toBeInTheDocument();
    expect(screen.getByText('quota.vk_expiring')).toBeInTheDocument();
  });

  it('toggles Enabled via updateRule when the Switch is clicked', async () => {
    const updateSpy = vi
      .spyOn(alertsApi, 'updateRule')
      .mockResolvedValue(sampleRule({ enabled: false }));
    const user = userEvent.setup();

    renderWithRouter(<AlertRulesListPage />);
    await screen.findByText('Quota Threshold Crossed');

    // Radix Switch renders as role=switch; first row is currently enabled.
    const switches = screen.getAllByRole('switch');
    expect(switches.length).toBeGreaterThan(0);
    await user.click(switches[0]);

    await waitFor(() => {
      expect(updateSpy).toHaveBeenCalledWith('quota.threshold', { enabled: false });
    });
  });
});
