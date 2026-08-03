/**
 * Pins that the expiry edit form actually reaches the PUT body. The rule logic
 * lives in deriveUpdateExpiry (unit-tested in ../expiryBounds.test.ts), but a
 * correct helper is worthless if the hook never calls it — these tests cover
 * the wiring itself.
 *
 * Named failure modes:
 *   - a chosen date never reaches the request body (expiry silently unchanged)
 *   - saving an unrelated field re-sends a re-derived expiry, moving a stored
 *     instant the admin never touched
 *   - the picker is seeded with a different calendar day than the one the Info
 *     tab renders for the same instant
 *   - a personal VK cannot clear its expiry (null never sent)
 *   - an application VK is sent null and rejected by requireApplicationExpiry
 *   - startEditing seeds never-expires from a state it would then overwrite
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import type { VirtualKey } from '@/api/types';
import { formatDate, setDisplayTZ } from '@/lib/format';
import { useVirtualKeyDetail } from './useVirtualKeyDetail';

const updateMock = vi.fn();
let currentVk: VirtualKey;

vi.mock('react-router-dom', () => ({
  useParams: () => ({ id: 'vk-1' }),
  useNavigate: () => vi.fn(),
}));

vi.mock('@/hooks/useApi', () => ({
  useApi: (_fn: unknown, key: unknown[]) => ({
    // The hook issues three useApi calls; only the VK detail one feeds the
    // edit form. The list-shaped ones must return {data: []} not a VK.
    data: key.includes('detail') ? currentVk : { data: [] },
    loading: false,
    error: null,
    refetch: vi.fn(),
  }),
}));

vi.mock('@/hooks/useMutation', () => ({
  useMutation: (fn: (arg: unknown) => unknown) => ({
    mutate: (arg: unknown) => {
      // Only the update mutation carries a body; regenerate takes no arg.
      if (arg && typeof arg === 'object' && 'body' in (arg as object)) updateMock(arg);
      return fn(arg);
    },
    loading: false,
  }),
}));

vi.mock('@/api/services', () => ({
  virtualKeyApi: { get: vi.fn(), update: vi.fn(), regenerate: vi.fn() },
  projectApi: { list: vi.fn() },
  systemApi: { listModels: vi.fn(), listTrafficEvents: vi.fn() },
}));

function makeVk(over: Partial<VirtualKey>): VirtualKey {
  return {
    id: 'vk-1',
    name: 'k',
    enabled: true,
    createdAt: '2026-01-01T00:00:00Z',
    vkType: 'application',
    ...over,
  };
}

function bodyOf(): Record<string, unknown> {
  return (updateMock.mock.calls.at(-1)![0] as { body: Record<string, unknown> }).body;
}

beforeEach(() => {
  updateMock.mockClear();
  // Pin the display zone east of UTC so "the calendar day in the admin's zone"
  // and "the UTC day" are observably different for the instants below.
  setDisplayTZ('Asia/Shanghai');
});

afterEach(() => setDisplayTZ(null));

describe('useVirtualKeyDetail — expiry edit wiring', () => {
  it('sends the chosen date, stamped end-of-day in the display zone', () => {
    currentVk = makeVk({ vkType: 'application', expiresAt: '2026-08-01T23:59:59Z' });
    const { result } = renderHook(() => useVirtualKeyDetail());

    act(() => result.current.startEditing());
    act(() => result.current.setEditExpiresAt('2029-09-01'));
    act(() => result.current.handleSave());

    // End of 2029-09-01 in the pinned +08 zone is 15:59:59.999Z the same day.
    expect(bodyOf().expiresAt).toBe('2029-09-01T15:59:59.999Z');
  });

  it('prefills the edit date from the VK current expiry', () => {
    currentVk = makeVk({ vkType: 'application', expiresAt: '2027-03-04T23:59:59Z' });
    const { result } = renderHook(() => useVirtualKeyDetail());

    act(() => result.current.startEditing());

    // 23:59:59Z on Mar 4 is 07:59:59 on Mar 5 in the pinned +08 zone.
    expect(result.current.editExpiresAt).toBe('2027-03-05');
  });

  it('seeds the picker with the same calendar day the Info tab renders', () => {
    // The Info tab renders the expiry with formatDate (display TZ) while the
    // picker takes a bare calendar day. If the picker were seeded from the UTC
    // day, the page would show one date and offer another for the same key.
    currentVk = makeVk({ vkType: 'application', expiresAt: '2026-09-01T23:59:59Z' });
    const { result } = renderHook(() => useVirtualKeyDetail());

    act(() => result.current.startEditing());

    expect(formatDate(currentVk.expiresAt)).toContain('Sep 2, 2026');
    expect(result.current.editExpiresAt).toBe('2026-09-02');
  });

  it('leaves a stored expiry untouched when only an unrelated field is edited', () => {
    // A stored expiry carries a real time component (any API-created key does).
    // The picker round-trips only a calendar day, so re-deriving a value on save
    // would restamp this instant to end-of-day and push the expiry later.
    currentVk = makeVk({ vkType: 'application', expiresAt: '2026-09-01T05:00:00Z' });
    const { result } = renderHook(() => useVirtualKeyDetail());

    act(() => result.current.startEditing());
    act(() => result.current.setEditSourceApp('renamed-service'));
    act(() => result.current.handleSave());

    expect(bodyOf().sourceApp).toBe('renamed-service');
    // Absent field → the PUT leaves the column exactly as stored.
    expect(bodyOf().expiresAt).toBeUndefined();
  });

  it('leaves an untouched never-expiring personal VK unchanged', () => {
    currentVk = makeVk({ vkType: 'personal', expiresAt: undefined });
    const { result } = renderHook(() => useVirtualKeyDetail());

    act(() => result.current.startEditing());
    act(() => result.current.setEditSourceApp('renamed-service'));
    act(() => result.current.handleSave());

    expect(bodyOf().expiresAt).toBeUndefined();
  });

  it('personal VK: never-expires clears the expiry to null', () => {
    currentVk = makeVk({ vkType: 'personal', expiresAt: '2026-08-01T23:59:59Z' });
    const { result } = renderHook(() => useVirtualKeyDetail());

    act(() => result.current.startEditing());
    act(() => result.current.setEditNeverExpires(true));
    act(() => result.current.setEditExpiresAt(''));
    act(() => result.current.handleSave());

    expect(bodyOf().expiresAt).toBeNull();
  });

  it('personal VK already never-expiring: startEditing seeds the toggle on', () => {
    currentVk = makeVk({ vkType: 'personal', expiresAt: undefined });
    const { result } = renderHook(() => useVirtualKeyDetail());

    act(() => result.current.startEditing());

    expect(result.current.editNeverExpires).toBe(true);
  });

  it('application VK: never sends null even if the date is blanked', () => {
    // requireApplicationExpiry rejects null, so a blank field must omit the
    // key (leave unchanged) rather than send a value the server 400s on.
    currentVk = makeVk({ vkType: 'application', expiresAt: '2026-08-01T23:59:59Z' });
    const { result } = renderHook(() => useVirtualKeyDetail());

    act(() => result.current.startEditing());
    act(() => result.current.setEditExpiresAt(''));
    act(() => result.current.handleSave());

    expect(bodyOf().expiresAt).toBeUndefined();
  });
});
