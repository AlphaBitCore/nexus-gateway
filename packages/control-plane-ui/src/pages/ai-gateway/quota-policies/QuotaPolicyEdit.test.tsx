import '@testing-library/jest-dom/vitest';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { screen, waitFor } from '@testing-library/react';

import { renderWithRouter } from '@/test/test-utils';

// Radix Tooltip / Select use ResizeObserver under the hood; jsdom does not
// ship it. Stub it before any Radix component mounts.
if (typeof globalThis.ResizeObserver === 'undefined') {
  class ResizeObserverStub {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  (globalThis as unknown as { ResizeObserver: typeof ResizeObserverStub }).ResizeObserver = ResizeObserverStub;
}

vi.mock('react-router-dom', async () => {
  const actual = await vi.importActual<typeof import('react-router-dom')>('react-router-dom');
  return { ...actual, useParams: () => ({ id: 'pol-1' }) };
});

vi.mock('@/api/services', async () => {
  const actual = await vi.importActual<typeof import('@/api/services')>('@/api/services');
  return {
    ...actual,
    quotaPolicyApi: {
      get: vi.fn(),
      update: vi.fn(),
      list: vi.fn(),
      create: vi.fn(),
      delete: vi.fn(),
    },
  };
});

vi.mock('@/components/ui', async () => {
  const actual = await vi.importActual<typeof import('@/components/ui')>('@/components/ui');
  return {
    ...actual,
    Select: ({ value, onValueChange, options, disabled, placeholder, error: _error, ...rest }: {
      value?: string;
      onValueChange: (v: string) => void;
      options: Array<{ value: string; label: string }>;
      disabled?: boolean;
      placeholder?: string;
      error?: boolean;
    }) => (
      <select
        {...rest}
        value={value ?? ''}
        onChange={(e) => onValueChange(e.target.value)}
        disabled={disabled}
      >
        {placeholder ? <option value="">{placeholder}</option> : null}
        {options.map((o) => (
          <option key={o.value} value={o.value}>{o.label}</option>
        ))}
      </select>
    ),
    OrgTreeSelect: ({ value, onChange, placeholder }: {
      value?: string;
      onChange?: (v: unknown) => void;
      placeholder?: string;
    }) => (
      <input
        data-testid="org-tree-select"
        value={value ?? ''}
        onChange={(e) => onChange?.(e.target.value)}
        placeholder={placeholder}
      />
    ),
  };
});

import { quotaPolicyApi } from '@/api/services';
import { QuotaPolicyEdit } from './QuotaPolicyEdit';

describe('QuotaPolicyEdit — self-heal on load', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('drops stale vkType when loaded scope is user, keeps organizationId', async () => {
    (quotaPolicyApi.get as ReturnType<typeof vi.fn>).mockResolvedValue({
      id: 'pol-1',
      name: 'Legacy user policy',
      scope: 'user',
      organizationId: 'org-eng',
      vkType: 'application', // illegal under new rules
      periodType: 'monthly',
      enforcementMode: 'reject',
      alertThresholds: [80, 90],
      priority: 0,
      enabled: true,
      createdAt: '2026-04-01T00:00:00Z',
      updatedAt: '2026-04-01T00:00:00Z',
    });

    renderWithRouter(<QuotaPolicyEdit />);
    await waitFor(() => {
      expect(screen.getByDisplayValue('Legacy user policy')).toBeInTheDocument();
    });
    // vkType picker must not be rendered for scope=user.
    expect(screen.queryByLabelText(/^VK type/i)).not.toBeInTheDocument();
    // OrgTreeSelect should carry the org value through.
    const orgInput = screen.getByTestId('org-tree-select') as HTMLInputElement;
    expect(orgInput.value).toBe('org-eng');
  });

  it('drops stale organizationId and vkType when loaded scope is project', async () => {
    (quotaPolicyApi.get as ReturnType<typeof vi.fn>).mockResolvedValue({
      id: 'pol-1',
      name: 'Legacy project policy',
      scope: 'project',
      organizationId: 'org-eng',
      vkType: 'personal',
      periodType: 'monthly',
      enforcementMode: 'reject',
      alertThresholds: [80, 90],
      priority: 0,
      enabled: true,
      createdAt: '2026-04-01T00:00:00Z',
      updatedAt: '2026-04-01T00:00:00Z',
    });

    renderWithRouter(<QuotaPolicyEdit />);
    await waitFor(() => {
      expect(screen.getByDisplayValue('Legacy project policy')).toBeInTheDocument();
    });
    expect(screen.queryByTestId('org-tree-select')).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/^VK type/i)).not.toBeInTheDocument();
  });

  it('drops stale organizationId when loaded scope is vk, keeps vkType', async () => {
    (quotaPolicyApi.get as ReturnType<typeof vi.fn>).mockResolvedValue({
      id: 'pol-1',
      name: 'Legacy vk policy',
      scope: 'vk',
      organizationId: 'org-eng',
      vkType: 'application',
      periodType: 'monthly',
      enforcementMode: 'reject',
      alertThresholds: [80, 90],
      priority: 0,
      enabled: true,
      createdAt: '2026-04-01T00:00:00Z',
      updatedAt: '2026-04-01T00:00:00Z',
    });

    renderWithRouter(<QuotaPolicyEdit />);
    await waitFor(() => {
      expect(screen.getByDisplayValue('Legacy vk policy')).toBeInTheDocument();
    });
    expect(screen.queryByTestId('org-tree-select')).not.toBeInTheDocument();
    const vkTypeSelect = screen.getByLabelText(/^VK type/i) as HTMLSelectElement;
    expect(vkTypeSelect.value).toBe('application');
  });
});
