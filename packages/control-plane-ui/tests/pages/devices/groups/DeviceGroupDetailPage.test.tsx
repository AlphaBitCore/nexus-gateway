import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { DeviceGroupDetailPage } from '@/pages/devices/groups/DeviceGroupDetailPage';

const svc = vi.hoisted(() => ({
  deviceGroupsApi: {
    get: vi.fn(), addMember: vi.fn(), removeMember: vi.fn(), delete: vi.fn(),
    previewMembership: vi.fn(), setMembershipQuery: vi.fn(), bulkForceRefresh: vi.fn(),
  },
  hubApi: { listNodes: vi.fn().mockResolvedValue({ nodes: [] }) },
}));
vi.mock('@/api/services', () => svc);
vi.mock('@/hooks/usePermission', () => ({ usePermission: () => true }));

const navigate = vi.fn();
vi.mock('react-router-dom', async (orig) => ({
  ...(await orig<typeof import('react-router-dom')>()),
  useParams: () => ({ id: 'g1' }),
  useNavigate: () => navigate,
}));
const mutateCalls = vi.hoisted(() => ({ list: [] as unknown[] }));
vi.mock('@/hooks/useMutation', () => ({
  useMutation: (fn: (a: unknown) => Promise<unknown>, opts?: { onSuccess?: (r: unknown) => void }) => ({
    mutate: async (arg: unknown) => { mutateCalls.list.push(arg); const r = await fn(arg); opts?.onSuccess?.(r); return r; },
    loading: false,
  }),
}));
const apiState = vi.hoisted(() => ({ value: { data: undefined as unknown, loading: false, error: null as Error | null, refetch: vi.fn() } }));
vi.mock('@/hooks/useApi', () => ({ useApi: () => apiState.value }));

const group = { id: 'g1', name: 'Macs', description: 'all macs', membershipQuery: null, memberships: [{ device: { id: 'd1', hostname: 'mac-1', os: 'darwin' } }] };
function ok(d: unknown) { return { data: d, loading: false, error: null, refetch: vi.fn() }; }
function wrap() {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter><DeviceGroupDetailPage /></MemoryRouter></I18nextProvider>);
}

describe('DeviceGroupDetailPage', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mutateCalls.list = [];
    apiState.value = ok(group);
    svc.deviceGroupsApi.delete.mockResolvedValue({});
  });

  it('renders the group name + its members', () => {
    wrap();
    expect(screen.getAllByText('Macs').length).toBeGreaterThan(0);
    expect(screen.getByText('mac-1')).toBeInTheDocument();
  });

  it('renders the error branch', () => {
    apiState.value = { data: undefined, loading: false, error: new Error('group load failed'), refetch: vi.fn() };
    wrap();
    expect(screen.getByText('group load failed')).toBeInTheDocument();
  });

  it('toggles the add-member form', () => {
    wrap();
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:deviceGroups.addMember') }));
    // toggling flips the button label to Cancel
    expect(screen.getByRole('button', { name: i18n.t('common:cancel') })).toBeInTheDocument();
  });

  it('delete group confirms then deletes + navigates', async () => {
    wrap();
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:deviceGroups.deleteGroup') }));
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:delete') }));
    await waitFor(() => expect(svc.deviceGroupsApi.delete).toHaveBeenCalledWith('g1'));
    await waitFor(() => expect(navigate).toHaveBeenCalled());
  });
});
