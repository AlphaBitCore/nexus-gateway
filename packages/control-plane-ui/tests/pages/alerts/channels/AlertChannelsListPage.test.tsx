/**
 * AlertChannelsListPage — integration tests.
 *
 * Stubs `alertsApi.listChannels` / `updateChannel` / `deleteChannel` /
 * `testChannel` at the module boundary and asserts:
 *   - channel rows render with name, type, severities
 *   - toggling the Enabled switch fires updateChannel(id, { enabled: next })
 *   - Test button fires testChannel(id)
 *   - Delete confirmation fires deleteChannel(id)
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithRouter } from '@/test/test-utils';
import { alertsApi } from '@/api/services/alerts/alerts';
import type { AlertChannel } from '@/api/services/alerts/alerts';
import { AlertChannelsListPage } from '../../../../src/pages/alerts/channels/AlertChannelsListPage';

// The alert write affordances gate on alert.update / .create / .delete while
// the pages themselves load on alert.read. The set is mutable so a single arm
// can take one grant away and prove the affordance is really gated; a blanket
// `() => true` would leave every one of those gates untested.
const denied = new Set<string>();
vi.mock('@/hooks/usePermission', () => ({
  usePermission: (key: string) => !denied.has(key),
}));

function sampleChannel(overrides: Partial<AlertChannel> = {}): AlertChannel {
  return {
    id: 'chn-1',
    name: 'oncall-pagerduty',
    type: 'pagerduty',
    enabled: true,
    severities: ['critical', 'high'],
    sourceTypes: [],
    config: { routingKey: 'xxxx-••••-abcd' },
    ...overrides,
  };
}

describe('AlertChannelsListPage', () => {
  beforeEach(() => {
    vi.spyOn(alertsApi, 'listChannels').mockResolvedValue({
      channels: [
        sampleChannel(),
        sampleChannel({
          id: 'chn-2',
          name: 'webhook-backup',
          type: 'webhook',
          enabled: false,
          severities: ['medium'],
          sourceTypes: ['quota', 'proxy'],
          config: { url: 'https://hooks.example.com/alert', headers: {} },
        }),
      ],
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('renders a row per channel', async () => {
    renderWithRouter(<AlertChannelsListPage />);
    expect(await screen.findByText('oncall-pagerduty')).toBeInTheDocument();
    expect(screen.getByText('webhook-backup')).toBeInTheDocument();
  });

  it('toggles Enabled via updateChannel when the Switch is clicked', async () => {
    const updateSpy = vi
      .spyOn(alertsApi, 'updateChannel')
      .mockResolvedValue(sampleChannel({ enabled: false }));
    const user = userEvent.setup();

    renderWithRouter(<AlertChannelsListPage />);
    await screen.findByText('oncall-pagerduty');

    const switches = screen.getAllByRole('switch');
    expect(switches.length).toBeGreaterThan(0);
    await user.click(switches[0]);

    await waitFor(() => {
      expect(updateSpy).toHaveBeenCalledWith('chn-1', { enabled: false });
    });
  });

  it('fires testChannel when the Test button is clicked', async () => {
    const testSpy = vi
      .spyOn(alertsApi, 'testChannel')
      .mockResolvedValue({ success: true });
    const user = userEvent.setup();

    renderWithRouter(<AlertChannelsListPage />);
    await screen.findByText('oncall-pagerduty');

    const testButtons = screen.getAllByRole('button', { name: /^test$/i });
    expect(testButtons.length).toBeGreaterThan(0);
    await user.click(testButtons[0]);

    await waitFor(() => {
      expect(testSpy).toHaveBeenCalledWith('chn-1');
    });
  });

  it('confirms then fires deleteChannel on Delete', async () => {
    const deleteSpy = vi
      .spyOn(alertsApi, 'deleteChannel')
      .mockResolvedValue(undefined as unknown as void);
    const user = userEvent.setup();

    renderWithRouter(<AlertChannelsListPage />);
    await screen.findByText('oncall-pagerduty');

    const deleteButtons = screen.getAllByRole('button', { name: /^delete$/i });
    await user.click(deleteButtons[0]);

    const dialog = await screen.findByRole('alertdialog');
    const confirmBtn = within(dialog).getByRole('button', { name: /^delete$/i });
    await user.click(confirmBtn);

    await waitFor(() => {
      expect(deleteSpy).toHaveBeenCalledWith('chn-1');
    });
  });

  // The list loads on alert.read. Its three writes carry three different
  // verbs, so each affordance has to be gated on its own.
  it('leaves the Switch and Test inert without alert.update', async () => {
    denied.add('alert:update');
    try {
      const updateSpy = vi.spyOn(alertsApi, 'updateChannel');
      const testSpy = vi.spyOn(alertsApi, 'testChannel');
      const user = userEvent.setup();

      renderWithRouter(<AlertChannelsListPage />);
      await screen.findByText('oncall-pagerduty');

      const switches = screen.getAllByRole('switch');
      expect(switches[0].hasAttribute('disabled')).toBe(true);
      const testBtn = screen.getAllByRole('button', { name: /test/i })[0];
      expect(testBtn.hasAttribute('disabled')).toBe(true);

      await user.click(switches[0]);
      await user.click(testBtn);
      expect(updateSpy).not.toHaveBeenCalled();
      expect(testSpy).not.toHaveBeenCalled();
    } finally {
      denied.delete('alert:update');
    }
  });

  it('offers no Delete without alert.delete', async () => {
    denied.add('alert:delete');
    try {
      renderWithRouter(<AlertChannelsListPage />);
      await screen.findByText('oncall-pagerduty');
      expect(screen.queryAllByRole('button', { name: /^delete$/i })).toHaveLength(0);
    } finally {
      denied.delete('alert:delete');
    }
  });

  it('offers no New channel without alert.create', async () => {
    denied.add('alert:create');
    try {
      renderWithRouter(<AlertChannelsListPage />);
      await screen.findByText('oncall-pagerduty');
      expect(screen.queryByRole('button', { name: /new channel/i })).toBeNull();
    } finally {
      denied.delete('alert:create');
    }
  });
});
