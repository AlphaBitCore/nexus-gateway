import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { FleetDeviceDetailPage } from '@/pages/devices/FleetDeviceDetailPage';
import { devicesApi } from '@/api/services';

const navigate = vi.fn();
vi.mock('react-router-dom', async (orig) => ({
  ...(await orig<typeof import('react-router-dom')>()),
  useParams: () => ({ id: 'dev-1' }),
  useNavigate: () => navigate,
}));
vi.mock('@/hooks/usePermission', () => ({ usePermission: () => true }));
// run the real mutation fn + onSuccess so the device-action API calls fire
vi.mock('@/hooks/useMutation', () => ({
  useMutation: (fn: () => Promise<unknown>, opts?: { onSuccess?: () => void }) => ({
    mutate: async () => { await fn(); opts?.onSuccess?.(); },
    loading: false,
  }),
}));
const deviceApiMock = vi.hoisted(() => ({
  forceRefresh: vi.fn(), unenroll: vi.fn(),
}));
vi.mock('@/api/services', async (orig) => ({
  ...(await orig<typeof import('@/api/services')>()),
  devicesApi: deviceApiMock,
  diagModeApi: { enable: vi.fn().mockResolvedValue({}), disable: vi.fn().mockResolvedValue({}) },
  fleetApi: {},
}));

const device = { id: 'dev-1', hostname: 'mac-1', os: 'darwin', osVersion: '15.3', agentVersion: '1.2.0', status: 'online' };
const deviceState = vi.hoisted(() => ({ value: { data: undefined as unknown, loading: false, error: null as Error | null } }));
// device detail returns the fixture; every other useApi (events/timeline/audit/
// config/diag + sub-component stats) returns undefined → safe null branches.
vi.mock('@/hooks/useApi', () => ({
  useApi: (_fn: unknown, key: unknown[]) =>
    key.includes('detail') && key.includes('devices')
      ? deviceState.value
      : { data: undefined, loading: false, error: null, refetch: vi.fn() },
}));

function wrap() {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter><FleetDeviceDetailPage /></MemoryRouter></I18nextProvider>);
}

describe('FleetDeviceDetailPage', () => {
  beforeEach(() => { deviceState.value = { data: device, loading: false, error: null, refetch: vi.fn() } as never; });

  it('renders the device header (hostname, status, OS)', () => {
    wrap();
    expect(screen.getAllByText('mac-1').length).toBeGreaterThan(0);
    expect(screen.getByText('online')).toBeInTheDocument();
    expect(screen.getAllByText(/macOS 15\.3/).length).toBeGreaterThan(0);
  });

  it('renders the loading skeleton', () => {
    deviceState.value = { data: undefined, loading: true, error: null, refetch: vi.fn() } as never;
    const { container } = wrap();
    expect(container.firstChild).toBeTruthy();
  });

  it('renders the error banner on failure', () => {
    deviceState.value = { data: undefined, loading: false, error: new Error('device load failed'), refetch: vi.fn() } as never;
    wrap();
    expect(screen.getByText('device load failed')).toBeInTheDocument();
  });

  it('switches across all detail tabs without crashing', () => {
    wrap();
    for (const label of [
      i18n.t('pages:fleet.tabCompliance'),
      i18n.t('pages:fleet.tabConfiguration'),
      i18n.t('pages:fleet.tabSystem'),
      i18n.t('pages:fleet.tabActivity'),
      i18n.t('pages:fleet.tabTraffic'),
    ]) {
      fireEvent.click(screen.getByText(label));
      // header survives every tab switch
      expect(screen.getAllByText('mac-1').length).toBeGreaterThan(0);
    }
  });

  it('Force Refresh triggers the devices API for this device', async () => {
    deviceApiMock.forceRefresh.mockResolvedValue({});
    wrap();
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:devices.forceRefresh') }));
    await waitFor(() => expect(deviceApiMock.forceRefresh).toHaveBeenCalledWith('dev-1'));
  });


  it('Revoke device confirms then unenrolls + navigates to the fleet list', async () => {
    deviceApiMock.unenroll.mockResolvedValue({});
    const user = userEvent.setup();
    wrap();
    // the device-actions dropdown trigger is the one carrying the ▾ caret
    const triggers = screen.getAllByRole('button', { name: /actions/i });
    await user.click(triggers.find((b) => b.textContent?.includes('▾')) ?? triggers[0]);
    await user.click(await screen.findByText(i18n.t('pages:fleet.revokeDevice')));
    const confirm = screen.getAllByRole('button', { name: i18n.t('pages:fleet.revokeDevice') });
    await user.click(confirm[confirm.length - 1]);
    await waitFor(() => expect(deviceApiMock.unenroll).toHaveBeenCalledWith('dev-1'));
    expect(navigate).toHaveBeenCalledWith('/devices');
  });
});
