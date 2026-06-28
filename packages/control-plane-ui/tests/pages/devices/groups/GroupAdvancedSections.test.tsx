import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { SmartMembershipCard, GroupBulkActionsCard } from '@/pages/devices/groups/GroupAdvancedSections';

const dg = vi.hoisted(() => ({
  deviceGroupsApi: {
    previewMembership: vi.fn(), setMembershipQuery: vi.fn(),
    bulkForceRefresh: vi.fn(), bulkRotateCert: vi.fn(),
  },
}));
vi.mock('@/api/services', () => dg);
vi.mock('@/context/ToastContext', () => ({ useToast: () => ({ addToast: vi.fn() }) }));
vi.mock('@/hooks/useMutation', () => ({
  useMutation: (fn: (a: unknown) => Promise<unknown>, opts?: { onSuccess?: (r: unknown) => void }) => ({
    mutate: async (arg: unknown) => { try { const r = await fn(arg); opts?.onSuccess?.(r); return r; } catch { /* fn set its own error state */ } },
    loading: false,
  }),
}));

function I18n({ children }: { children: React.ReactNode }) {
  return <I18nextProvider i18n={i18n}>{children}</I18nextProvider>;
}

describe('SmartMembershipCard', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    dg.deviceGroupsApi.previewMembership.mockResolvedValue({ matched: 3, sample: ['mac-1', 'mac-2'] });
    dg.deviceGroupsApi.setMembershipQuery.mockResolvedValue({});
  });

  it('static group shows the Static badge + an empty editor (no revert)', () => {
    render(<I18n><SmartMembershipCard groupId="g1" currentQuery={null} canUpdate onSaved={vi.fn()} /></I18n>);
    expect(screen.getByText(i18n.t('pages:deviceGroups.modeStatic', 'Static'))).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: i18n.t('pages:deviceGroups.revertStatic', 'Revert to static') })).not.toBeInTheDocument();
  });

  it('smart group hydrates the editor + shows the Smart badge + revert', () => {
    render(<I18n><SmartMembershipCard groupId="g1" currentQuery={{ all: [] }} canUpdate onSaved={vi.fn()} /></I18n>);
    expect(screen.getByText(i18n.t('pages:deviceGroups.modeSmart', 'Smart'))).toBeInTheDocument();
    expect(screen.getByRole('textbox')).toHaveValue(JSON.stringify({ all: [] }, null, 2));
    expect(screen.getByRole('button', { name: i18n.t('pages:deviceGroups.revertStatic', 'Revert to static') })).toBeInTheDocument();
  });

  it('renders the predicate viewer as non-interactive without update permission', () => {
    render(<I18n><SmartMembershipCard groupId="g1" currentQuery={{ all: [] }} canUpdate={false} onSaved={vi.fn()} /></I18n>);
    const viewer = screen.getByRole('textbox');

    expect(viewer).toHaveValue(JSON.stringify({ all: [] }, null, 2));
    expect(viewer).toHaveAttribute('readonly');
    expect(viewer).toHaveAttribute('aria-readonly', 'true');
    expect(viewer).toHaveAttribute('tabindex', '-1');
    expect(viewer).not.toBeDisabled();
    expect(viewer).toHaveStyle({ pointerEvents: 'none', cursor: 'default' });
  });

  it('preview with valid JSON queries previewMembership + shows the match count', async () => {
    render(<I18n><SmartMembershipCard groupId="g1" currentQuery={null} canUpdate onSaved={vi.fn()} /></I18n>);
    fireEvent.change(screen.getByRole('textbox'), { target: { value: '{"all":[]}' } });
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:deviceGroups.previewMembers', 'Preview matches') }));
    await waitFor(() => expect(dg.deviceGroupsApi.previewMembership).toHaveBeenCalledWith({ all: [] }));
    await waitFor(() => expect(screen.getByText(/3 devices match/i)).toBeInTheDocument());
  });

  it('preview with invalid JSON surfaces the parse error and skips the API', async () => {
    render(<I18n><SmartMembershipCard groupId="g1" currentQuery={null} canUpdate onSaved={vi.fn()} /></I18n>);
    fireEvent.change(screen.getByRole('textbox'), { target: { value: '{not json' } });
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:deviceGroups.previewMembers', 'Preview matches') }));
    await waitFor(() => expect(dg.deviceGroupsApi.previewMembership).not.toHaveBeenCalled());
    // the raw JSON.parse error message renders in the error slot
    await waitFor(() => expect(screen.getByText(/Expected property name|Unexpected token|not valid JSON|in JSON at position/i)).toBeInTheDocument());
  });

  it('save persists the parsed predicate then calls onSaved', async () => {
    const onSaved = vi.fn();
    render(<I18n><SmartMembershipCard groupId="g1" currentQuery={null} canUpdate onSaved={onSaved} /></I18n>);
    fireEvent.change(screen.getByRole('textbox'), { target: { value: '{"any":[]}' } });
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:deviceGroups.saveSmart', 'Save predicate') }));
    await waitFor(() => expect(dg.deviceGroupsApi.setMembershipQuery).toHaveBeenCalledWith('g1', { any: [] }));
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
  });

  it('revert clears the predicate (setMembershipQuery null) then calls onSaved', async () => {
    const onSaved = vi.fn();
    render(<I18n><SmartMembershipCard groupId="g1" currentQuery={{ all: [] }} canUpdate onSaved={onSaved} /></I18n>);
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:deviceGroups.revertStatic', 'Revert to static') }));
    await waitFor(() => expect(dg.deviceGroupsApi.setMembershipQuery).toHaveBeenCalledWith('g1', null));
    await waitFor(() => expect(onSaved).toHaveBeenCalled());
  });
});

describe('GroupBulkActionsCard', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    dg.deviceGroupsApi.bulkForceRefresh.mockResolvedValue({ total: 2, results: [] });
    dg.deviceGroupsApi.bulkRotateCert.mockResolvedValue({ total: 2, results: [] });
  });

  it('renders nothing without update permission', () => {
    const { container } = render(<I18n><GroupBulkActionsCard groupId="g1" canUpdate={false} /></I18n>);
    expect(container.firstChild).toBeNull();
  });

  it('runs the force-refresh + rotate-cert bulk actions for the group', async () => {
    render(<I18n><GroupBulkActionsCard groupId="g1" canUpdate /></I18n>);
    const buttons = screen.getAllByRole('button');
    fireEvent.click(buttons[0]);
    await waitFor(() => expect(dg.deviceGroupsApi.bulkForceRefresh).toHaveBeenCalledWith('g1'));
    fireEvent.click(buttons[1]);
    await waitFor(() => expect(dg.deviceGroupsApi.bulkRotateCert).toHaveBeenCalledWith('g1'));
  });
});
