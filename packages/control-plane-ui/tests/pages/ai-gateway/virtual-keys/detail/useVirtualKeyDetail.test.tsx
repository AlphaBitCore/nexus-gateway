import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useVirtualKeyDetail } from '@/pages/ai-gateway/virtual-keys/detail/useVirtualKeyDetail';
import { setDisplayTZ } from '@/lib/format';

const mutateCalls: unknown[] = [];
let vk: Record<string, unknown> | undefined;
const projects = [{ id: 'pr1', name: 'Billing' }];

vi.mock('react-router-dom', () => ({ useParams: () => ({ id: 'vk-1' }), useNavigate: () => vi.fn() }));
vi.mock('@/hooks/useApi', () => ({
  useApi: (_fn: unknown, key: string[]) => {
    if (key.includes('detail')) return { data: vk, loading: false, error: null, refetch: vi.fn() };
    if (key.includes('projects')) return { data: { data: projects } };
    return { data: { data: [] } };
  },
}));
vi.mock('@/hooks/useMutation', () => ({
  useMutation: () => ({ mutate: (arg: unknown) => mutateCalls.push(arg), loading: false }),
}));
vi.mock('@/api/services', () => ({ virtualKeyApi: {}, projectApi: {}, systemApi: {} }));
vi.mock('@/constants/admin-api', () => ({ ADMIN_LIST_FULL_PAGE_PARAMS: { limit: '500' } }));

describe('useVirtualKeyDetail', () => {
  beforeEach(() => {
    mutateCalls.length = 0;
    // Pin the display zone so the expected calendar dates below are exact
    // rather than dependent on the machine running the suite.
    setDisplayTZ('Asia/Shanghai');
    vk = { id: 'vk-1', projectId: 'pr1', sourceApp: 'app', enabled: true, rateLimitRpm: null, allowedModels: [], expiresAt: '2026-12-31T00:00:00Z' };
  });

  afterEach(() => setDisplayTZ(null));

  it('resolves the linked project from projectId', () => {
    const { result } = renderHook(() => useVirtualKeyDetail());
    expect(result.current.project?.name).toBe('Billing');
  });

  it('startEditing seeds edit state from the key (expiry on the local calendar, neverExpires false)', () => {
    const { result } = renderHook(() => useVirtualKeyDetail());
    act(() => result.current.startEditing());
    expect(result.current.isEditing).toBe(true);
    expect(result.current.editProjectId).toBe('pr1');
    // 2026-12-31T00:00:00Z is 08:00 on 2026-12-31 in the pinned display zone.
    expect(result.current.editExpiresAt).toBe('2026-12-31');
    expect(result.current.editNeverExpires).toBe(false);
    expect(result.current.editRateLimitRpm).toBe(''); // null → ''
  });

  it('handleSave omits an untouched expiry and maps blank rate-limit → undefined', () => {
    const { result } = renderHook(() => useVirtualKeyDetail());
    act(() => result.current.startEditing());
    act(() => result.current.handleSave());
    const call = mutateCalls[0] as { id: string; body: Record<string, unknown> };
    expect(call.id).toBe('vk-1');
    // The expiry field was never touched, so it must not appear in the PUT —
    // the stored instant is left exactly as it is.
    expect(call.body).toMatchObject({ projectId: 'pr1', enabled: true, rateLimitRpm: undefined });
    expect(call.body.expiresAt).toBeUndefined();
  });

  it('handleSave sends a numeric rate limit without disturbing the untouched expiry', () => {
    const { result } = renderHook(() => useVirtualKeyDetail());
    act(() => result.current.startEditing());
    act(() => { result.current.setEditRateLimitRpm('120'); });
    act(() => result.current.handleSave());
    const call = mutateCalls.at(-1) as { body: Record<string, unknown> };
    expect(call.body.rateLimitRpm).toBe(120);
    // Editing an unrelated field must leave the expiry column alone.
    expect(call.body.expiresAt).toBeUndefined();
  });

  it('handleSave stamps a changed expiry end-of-day in the display zone', () => {
    const { result } = renderHook(() => useVirtualKeyDetail());
    act(() => result.current.startEditing());
    act(() => { result.current.setEditExpiresAt('2027-01-15'); });
    act(() => result.current.handleSave());
    const call = mutateCalls.at(-1) as { body: Record<string, unknown> };
    // End of 2027-01-15 in the pinned +08 zone is 15:59:59.999Z the same day.
    expect(call.body.expiresAt).toBe('2027-01-15T15:59:59.999Z');
    // A key that carries an expiry must send a stamped instant, never null —
    // the backend rejects a null expiry for application keys, and a bare date
    // would land at midnight and retire the key a day early.
    expect(call.body.expiresAt).not.toBeNull();
  });

  it('copyNewKey is a no-op without a regenerated key; dismissNewKey clears it', () => {
    const { result } = renderHook(() => useVirtualKeyDetail());
    const writeText = vi.fn();
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    act(() => result.current.copyNewKey());
    expect(writeText).not.toHaveBeenCalled(); // newKey is null
    act(() => result.current.dismissNewKey());
    expect(result.current.newKey).toBeNull();
  });

  it('tab + regen-confirm setters update state', () => {
    const { result } = renderHook(() => useVirtualKeyDetail());
    act(() => result.current.setActiveTab('access-log'));
    expect(result.current.activeTab).toBe('access-log');
    act(() => result.current.setRegenConfirming(true));
    expect(result.current.regenConfirming).toBe(true);
    act(() => result.current.regenerateKey(undefined as never));
    expect(mutateCalls.length).toBeGreaterThan(0);
  });
});
