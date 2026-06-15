import { describe, it, expect, vi, beforeEach } from 'vitest';
import { screen, waitFor, fireEvent } from '@testing-library/react';

import { renderWithRouter } from '@/test/test-utils';

// Radix Tooltip (used by FormField) depends on ResizeObserver, which jsdom
// does not ship. Stub it before any Radix component mounts.
if (typeof globalThis.ResizeObserver === 'undefined') {
  class ResizeObserverStub {
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  (globalThis as unknown as { ResizeObserver: typeof ResizeObserverStub }).ResizeObserver = ResizeObserverStub;
}

// Mock Radix Select → native <select> and OrgTreeSelect → native <input>
// so jsdom can drive changes without pointer-event choreography.
vi.mock('@/components/ui', async () => {
  const actual = await vi.importActual<typeof import('@/components/ui')>('@/components/ui');
  return {
    ...actual,
    // FormField clones its child and injects id + aria-*. Spread rest so the
    // id lands on the native <select>, which enables getByLabelText.
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

import { QuotaPolicyCreate } from '../../../../src/pages/ai-gateway/quota-policies/QuotaPolicyCreate';

describe('QuotaPolicyCreate — scenario-driven form', () => {
  beforeEach(() => vi.clearAllMocks());

  it('defaults to Organization budget and shows the OrgTreeSelect', async () => {
    renderWithRouter(<QuotaPolicyCreate />);
    await waitFor(() => expect(screen.getByTestId('org-tree-select')).toBeInTheDocument());
    // VK type picker should not be rendered for scope=organization.
    expect(screen.queryByLabelText(/^VK type/i)).not.toBeInTheDocument();
  });

  it('hides OrgTreeSelect and VK type for Project budget', async () => {
    renderWithRouter(<QuotaPolicyCreate />);
    const policyType = screen.getByLabelText(/Policy type/i) as HTMLSelectElement;
    fireEvent.change(policyType, { target: { value: 'project' } });
    await waitFor(() => expect(screen.queryByTestId('org-tree-select')).not.toBeInTheDocument());
    expect(screen.queryByLabelText(/^VK type/i)).not.toBeInTheDocument();
  });

  it('shows VK type select for Virtual key ceiling', async () => {
    renderWithRouter(<QuotaPolicyCreate />);
    const policyType = screen.getByLabelText(/Policy type/i) as HTMLSelectElement;
    fireEvent.change(policyType, { target: { value: 'vk' } });
    await waitFor(() => expect(screen.getByLabelText(/^VK type/i)).toBeInTheDocument());
    expect(screen.queryByTestId('org-tree-select')).not.toBeInTheDocument();
  });

  it('clears stale org value after switching to project and back', async () => {
    renderWithRouter(<QuotaPolicyCreate />);
    const orgInput = screen.getByTestId('org-tree-select') as HTMLInputElement;
    fireEvent.change(orgInput, { target: { value: 'org-123' } });
    expect((screen.getByTestId('org-tree-select') as HTMLInputElement).value).toBe('org-123');
    const policyType = screen.getByLabelText(/Policy type/i) as HTMLSelectElement;
    fireEvent.change(policyType, { target: { value: 'project' } });
    await waitFor(() => expect(screen.queryByTestId('org-tree-select')).not.toBeInTheDocument());
    fireEvent.change(policyType, { target: { value: 'organization' } });
    await waitFor(() => {
      const reshown = screen.getByTestId('org-tree-select') as HTMLInputElement;
      expect(reshown.value).toBe('');
    });
  });

  it('renders the help text for the active policy type', async () => {
    renderWithRouter(<QuotaPolicyCreate />);
    expect(screen.getByText(/Caps total spend for the selected organization/i)).toBeInTheDocument();
    const policyType = screen.getByLabelText(/Policy type/i) as HTMLSelectElement;
    fireEvent.change(policyType, { target: { value: 'vk' } });
    await waitFor(() => {
      expect(screen.getByText(/Default ceiling per VK of the selected type/i)).toBeInTheDocument();
    });
  });
});
