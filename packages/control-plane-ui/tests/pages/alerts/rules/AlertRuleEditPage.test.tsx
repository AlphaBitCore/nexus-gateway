/**
 * AlertRuleEditPage — integration tests.
 *
 * Stubs `alertsApi.getRule`, `alertsApi.updateRule`, and `alertsApi.resetRule`
 * and verifies:
 *   - the rule is fetched and seeded into the form
 *   - Save dispatches updateRule with the current form body (lowercase severity)
 *   - Reset opens confirmation then dispatches resetRule
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { Routes, Route } from 'react-router-dom';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithRouter } from '@/test/test-utils';
import { alertsApi } from '@/api/services/alerts/alerts';
import type { AlertRule } from '@/api/services/alerts/alerts';
import { AlertRuleEditPage } from '../../../../src/pages/alerts/rules/AlertRuleEditPage';

function sampleRule(): AlertRule {
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
        thresholds: {
          type: 'array',
          items: { type: 'integer', minimum: 1, maximum: 100 },
        },
      },
      required: ['thresholds'],
    },
    cooldownSec: 300,
    updatedAt: '2026-04-22T10:00:00Z',
  };
}

function renderWithRoute() {
  return renderWithRouter(
    <Routes>
      <Route path="/alerts/rules/:id" element={<AlertRuleEditPage />} />
    </Routes>,
    { route: '/alerts/rules/quota.threshold' },
  );
}

describe('AlertRuleEditPage', () => {
  beforeEach(() => {
    vi.spyOn(alertsApi, 'getRule').mockResolvedValue(sampleRule());
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('fetches the rule and seeds the editor from the fetched params', async () => {
    renderWithRoute();
    // Rule display name appears in the breadcrumb, page header, and metadata card.
    expect((await screen.findAllByText(/Quota Threshold Crossed/i)).length).toBeGreaterThan(0);
    // Editor renders the thresholds as a comma list after the fetch resolves.
    expect(await screen.findByDisplayValue('80, 95')).toBeInTheDocument();
  });

  it('calls updateRule with the current body on Save', async () => {
    const updateSpy = vi
      .spyOn(alertsApi, 'updateRule')
      .mockResolvedValue(sampleRule());
    const user = userEvent.setup();

    renderWithRoute();
    await screen.findByDisplayValue('80, 95');

    const saveBtn = screen.getByRole('button', { name: /^save$/i });
    await user.click(saveBtn);

    await waitFor(() => {
      expect(updateSpy).toHaveBeenCalledWith('quota.threshold', {
        enabled: true,
        defaultSeverity: 'high',
        cooldownSec: 300,
        requiresAck: true,
        groupIdFilter: '',
        params: { thresholds: [80, 95] },
      });
    });
  });

  it('confirms then dispatches resetRule on Reset', async () => {
    const resetSpy = vi
      .spyOn(alertsApi, 'resetRule')
      .mockResolvedValue(sampleRule());
    const user = userEvent.setup();

    renderWithRoute();
    await screen.findByDisplayValue('80, 95');

    // Click the trigger button (outside the dialog).
    const triggerBtn = screen.getByRole('button', { name: /reset to defaults/i });
    await user.click(triggerBtn);

    // The AlertDialog opens with role=alertdialog; its confirm button also
    // reads "Reset to defaults" but lives inside the alertdialog subtree.
    const dialog = await screen.findByRole('alertdialog');
    const confirmBtn = within(dialog).getByRole('button', { name: /reset to defaults/i });
    await user.click(confirmBtn);

    await waitFor(() => {
      expect(resetSpy).toHaveBeenCalledWith('quota.threshold');
    });
  });
});
