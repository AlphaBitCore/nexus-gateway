/**
 * AlertChannelEditPage — integration tests.
 *
 * Covers:
 *   - loading an existing channel seeds the form
 *   - saving fires updateChannel with the current body
 *   - masked secrets PUT verbatim (Hub's mergeMaskedSecrets handles restore)
 *     when the user does not click "Change"
 *   - clicking "Change" on a masked field clears it and lets the user type
 *     a fresh value, which is PUT as-is
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { Routes, Route } from 'react-router-dom';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithRouter } from '@/test/test-utils';
import { alertsApi } from '@/api/services/alerts/alerts';
import type { AlertChannel } from '@/api/services/alerts/alerts';
import { AlertChannelEditPage, MASK_PREFIX } from '../../../../src/pages/alerts/channels/AlertChannelEditPage';

function pagerdutyChannel(): AlertChannel {
  return {
    id: 'chn-1',
    name: 'oncall-pagerduty',
    type: 'pagerduty',
    enabled: true,
    severities: ['critical', 'high'],
    sourceTypes: [],
    config: { routingKey: `${MASK_PREFIX}abcd` },
  };
}

function renderEdit(route = '/alerts/channels/chn-1') {
  return renderWithRouter(
    <Routes>
      <Route path="/alerts/channels/:id" element={<AlertChannelEditPage />} />
    </Routes>,
    { route },
  );
}

describe('AlertChannelEditPage', () => {
  beforeEach(() => {
    vi.spyOn(alertsApi, 'getChannel').mockResolvedValue(pagerdutyChannel());
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('seeds the form from the fetched channel', async () => {
    renderEdit();
    expect(await screen.findByDisplayValue('oncall-pagerduty')).toBeInTheDocument();
  });

  it('PUTs the masked routing key verbatim when the user does not click Change', async () => {
    const updateSpy = vi
      .spyOn(alertsApi, 'updateChannel')
      .mockResolvedValue(pagerdutyChannel());
    const user = userEvent.setup();

    renderEdit();
    await screen.findByDisplayValue('oncall-pagerduty');

    const saveBtn = screen.getByRole('button', { name: /^save$/i });
    await user.click(saveBtn);

    await waitFor(() => {
      expect(updateSpy).toHaveBeenCalledWith(
        'chn-1',
        expect.objectContaining({
          name: 'oncall-pagerduty',
          type: 'pagerduty',
          enabled: true,
          severities: ['critical', 'high'],
          sourceTypes: [],
          config: { routingKey: `${MASK_PREFIX}abcd` },
        }),
      );
    });
  });

  it('clicking Change clears the masked field so the user can enter a new secret', async () => {
    const updateSpy = vi
      .spyOn(alertsApi, 'updateChannel')
      .mockResolvedValue(pagerdutyChannel());
    const user = userEvent.setup();

    renderEdit();
    await screen.findByDisplayValue('oncall-pagerduty');

    // Masked value is shown in a read-only input alongside a "Change" button.
    const changeBtn = screen.getByRole('button', { name: /change/i });
    await user.click(changeBtn);

    // After clicking Change, the routing-key input is cleared and editable.
    // The password-type input doesn't expose role=textbox, so query by
    // placeholder / label via the surrounding FormField label.
    const label = screen.getByText(/routing key/i);
    // The Input is a sibling via the FormField wrapper; find all inputs and
    // pick the one that is now empty and editable.
    const inputs = document.querySelectorAll('input');
    const target = Array.from(inputs).find(
      (el) => !el.readOnly && el.getAttribute('type') === 'password',
    );
    expect(target).toBeTruthy();
    expect(label).toBeInTheDocument();

    await user.type(target!, 'brand-new-key');

    const saveBtn = screen.getByRole('button', { name: /^save$/i });
    await user.click(saveBtn);

    await waitFor(() => {
      expect(updateSpy).toHaveBeenCalledWith(
        'chn-1',
        expect.objectContaining({
          config: { routingKey: 'brand-new-key' },
        }),
      );
    });
  });
});
