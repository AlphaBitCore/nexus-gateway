/**
 * DeviceAuthSettingsPage — device enrolment auth mode.
 *
 * The page reads GET /api/admin/settings/device-auth (settings.read) and
 * writes PUT on the same path (settings.update). Guarding the route
 * on settings.update hides a readable page from a settings auditor, so
 * it is guarded on settings.read and Save carries the write grant itself.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithRouter } from '@/test/test-utils';
import { fleetApi } from '@/api/services';
import { DeviceAuthSettingsPage } from '@/pages/devices/auth/DeviceAuthSettingsPage';

// Mutable so one arm can take settings:update away and prove Save is gated;
// a blanket `() => true` would leave the gate untested.
const denied = new Set<string>();
vi.mock('@/hooks/usePermission', () => ({
  usePermission: (key: string) => !denied.has(key),
}));

const SETTINGS = {
  mode: 'mtls-only',
  ssoConfigured: true,
  ssoProviders: [{ id: 'idp-1', type: 'oidc', name: 'Okta' }],
  localLoginAvailable: true,
};

describe('DeviceAuthSettingsPage', () => {
  beforeEach(() => {
    vi.spyOn(fleetApi, 'getDeviceAuthSettings').mockResolvedValue(SETTINGS);
  });

  afterEach(() => {
    vi.restoreAllMocks();
    denied.clear();
  });

  it('seeds the mode from the fetched settings', async () => {
    renderWithRouter(<DeviceAuthSettingsPage />);
    await waitFor(() => expect(fleetApi.getDeviceAuthSettings).toHaveBeenCalled());
    const chosen = await screen.findByRole('radio', { checked: true });
    expect(chosen).toBeInTheDocument();
  });

  it('PUTs the selected mode on Save', async () => {
    const updateSpy = vi
      .spyOn(fleetApi, 'updateDeviceAuthSettings')
      .mockResolvedValue({ ...SETTINGS, mode: 'sso-required' });
    const user = userEvent.setup();

    renderWithRouter(<DeviceAuthSettingsPage />);
    await waitFor(() => expect(fleetApi.getDeviceAuthSettings).toHaveBeenCalled());

    // Pick a mode other than the one that came back, so Save is enabled.
    const radios = await screen.findAllByRole('radio');
    const other = radios.find((r) => !(r as HTMLInputElement).checked);
    expect(other).toBeDefined();
    await user.click(other!);

    await user.click(screen.getByRole('button', { name: /^save$/i }));
    await waitFor(() => expect(updateSpy).toHaveBeenCalled());
  });

  it('leaves Save inert without settings.update', async () => {
    denied.add('settings:update');
    const updateSpy = vi.spyOn(fleetApi, 'updateDeviceAuthSettings');
    const user = userEvent.setup();

    renderWithRouter(<DeviceAuthSettingsPage />);
    await waitFor(() => expect(fleetApi.getDeviceAuthSettings).toHaveBeenCalled());

    // Make a real change first — Save is also disabled when nothing is
    // pending, and the assertion below would otherwise hold for that reason.
    const radios = await screen.findAllByRole('radio');
    const other = radios.find((r) => !(r as HTMLInputElement).checked);
    expect(other).toBeDefined();
    await user.click(other!);

    const save = screen.getByRole('button', { name: /^save$/i });
    expect(save.hasAttribute('disabled')).toBe(true);

    await user.click(save);
    expect(updateSpy).not.toHaveBeenCalled();
  });
});
