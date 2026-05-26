import { describe, it, expect, vi } from 'vitest';
import { renderHook } from '@testing-library/react';
import { usePermission } from './usePermission';

const mockPermissions = vi.fn().mockReturnValue([]);
vi.mock('../auth/context/AuthContext', () => ({
  useAuth: () => ({ permissions: mockPermissions() }),
}));

describe('usePermission', () => {
  it('returns true when the mapped IAM action is in permissions', () => {
    mockPermissions.mockReturnValue(['admin:provider.create']);
    const { result } = renderHook(() => usePermission('provider:create'));
    expect(result.current).toBe(true);
  });

  it('returns false when the mapped IAM action is not in permissions', () => {
    mockPermissions.mockReturnValue(['admin:provider.read']);
    const { result } = renderHook(() => usePermission('provider:create'));
    expect(result.current).toBe(false);
  });

  it('returns false for unknown permission keys', () => {
    mockPermissions.mockReturnValue(['admin:provider.create']);
    const { result } = renderHook(() => usePermission('nonexistent:action'));
    expect(result.current).toBe(false);
  });

  it('returns false when permissions array is empty', () => {
    mockPermissions.mockReturnValue([]);
    const { result } = renderHook(() => usePermission('provider:create'));
    expect(result.current).toBe(false);
  });

  it('returns true for audit:export mapped to admin:audit-log.export', () => {
    mockPermissions.mockReturnValue(['admin:audit-log.export']);
    const { result } = renderHook(() => usePermission('audit:export'));
    expect(result.current).toBe(true);
  });

  it('returns true for kill-switch:toggle mapped to admin:kill-switch.toggle', () => {
    mockPermissions.mockReturnValue(['admin:kill-switch.toggle']);
    const { result } = renderHook(() => usePermission('kill-switch:toggle'));
    expect(result.current).toBe(true);
  });
});
