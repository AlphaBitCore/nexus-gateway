/**
 * ExemptionsPage unit tests — unified list with status filter, create dialog,
 * delete + approve flows.
 *
 * Select is mocked to a native <select> so jsdom can drive option changes
 * without Radix portal/pointer-event flakiness.
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithRouter } from '@/test/test-utils';
import { complianceApi } from '@/api/services/compliance/compliance';
import type { UnifiedExemptionRow } from '@/api/services/compliance/compliance';
import { ExemptionsPage } from '../../../../src/pages/compliance/exemptions/ExemptionsPage';

vi.mock('@/components/ui', async () => {
  const actual = await vi.importActual<typeof import('@/components/ui')>('@/components/ui');
  return {
    ...actual,
    Select: ({
      value,
      onValueChange,
      options,
      placeholder,
    }: {
      value?: string;
      onValueChange: (v: string) => void;
      options: Array<{ value: string; label: string; disabled?: boolean }>;
      placeholder?: string;
    }) => (
      <select
        data-testid="mock-select"
        value={value ?? ''}
        onChange={(e) => onValueChange(e.target.value)}
      >
        {placeholder ? <option value="">{placeholder}</option> : null}
        {options.map((o) => (
          <option key={o.value} value={o.value} disabled={o.disabled}>
            {o.label}
          </option>
        ))}
      </select>
    ),
  };
});

const futureTs = new Date(Date.now() + 24 * 60 * 60 * 1000).toISOString();
const pastEff = new Date(Date.now() - 60 * 60 * 1000).toISOString();

const SAMPLE_GRANT: UnifiedExemptionRow = {
  kind: 'grant',
  status: 'effective',
  id: 'exempt-1a2b3c4d5e6f',
  sourceIp: '10.0.0.5',
  targetHost: 'api.openai.com',
  reason: 'false positive investigation',
  durationMinutes: 1440,
  createdAt: pastEff,
  effectiveFrom: pastEff,
  expiresAt: futureTs,
  approvedBy: 'alice@nexus.ai',
  inactive: false,
  activatedAt: null,
  transactionId: null,
  requestedBy: null,
};

const SAMPLE_PENDING: UnifiedExemptionRow = {
  kind: 'pending',
  status: 'pending',
  id: 'fba61740-f996-4703-987a-3e66e12bbbe0',
  sourceIp: '10.0.1.15',
  targetHost: 'api.mistral.ai',
  reason: 'PoC',
  durationMinutes: 1440,
  createdAt: '2026-04-22T03:10:06.363Z',
  effectiveFrom: null,
  expiresAt: null,
  approvedBy: null,
  inactive: null,
  activatedAt: null,
  transactionId: 'pending-exempt-tx-1',
  requestedBy: 'nexus-user-agent-jdoe',
};

describe('ExemptionsPage', () => {
  beforeEach(() => {
    vi.spyOn(complianceApi, 'listExemptions').mockResolvedValue({
      rows: [SAMPLE_GRANT, SAMPLE_PENDING],
      total: 2,
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('defaults to All and renders both grant and pending rows', async () => {
    renderWithRouter(<ExemptionsPage />);

    await waitFor(() => {
      expect(screen.getByText('10.0.0.5')).toBeInTheDocument();
    });
    expect(screen.getByText('api.openai.com')).toBeInTheDocument();
    expect(screen.getByText('alice@nexus.ai')).toBeInTheDocument();
    expect(screen.getByText('10.0.1.15')).toBeInTheDocument();
    expect(screen.getByText('api.mistral.ai')).toBeInTheDocument();

    expect(complianceApi.listExemptions).toHaveBeenCalledWith(
      expect.objectContaining({ tab: 'all', limit: 10, offset: 0 }),
    );
  });

  it('switches to the pending tab via the Status filter', async () => {
    const spy = vi
      .spyOn(complianceApi, 'listExemptions')
      .mockResolvedValueOnce({ rows: [SAMPLE_GRANT, SAMPLE_PENDING], total: 2 })
      .mockResolvedValueOnce({ rows: [SAMPLE_PENDING], total: 1 });

    const user = userEvent.setup();
    renderWithRouter(<ExemptionsPage />);

    const statusSelect = await screen.findByTestId('mock-select');
    await user.selectOptions(statusSelect, 'pending');

    await waitFor(() => {
      expect(spy).toHaveBeenCalledWith(expect.objectContaining({ tab: 'pending' }));
    });
  });

  it('opens the create dialog when Create exemption is clicked', async () => {
    const user = userEvent.setup();
    renderWithRouter(<ExemptionsPage />);

    await waitFor(() => {
      expect(screen.getByText('10.0.0.5')).toBeInTheDocument();
    });

    const createButton = screen.getByRole('button', { name: /create exemption/i });
    await user.click(createButton);

    await waitFor(() => {
      expect(
        screen.getByPlaceholderText(/10\.0\.0\.0\/24 or 10\.0\.0\.5/),
      ).toBeInTheDocument();
    });
  });

  it('invokes deleteExemptionGrant after confirming delete on a pre-activation grant', async () => {
    const deleteSpy = vi.spyOn(complianceApi, 'deleteExemptionGrant').mockResolvedValue(undefined);

    const user = userEvent.setup();
    renderWithRouter(<ExemptionsPage />);

    await waitFor(() => {
      expect(screen.getByText('10.0.0.5')).toBeInTheDocument();
    });

    const deleteButtons = screen.getAllByRole('button', { name: /^delete$/i });
    await user.click(deleteButtons[0]);

    await waitFor(() => {
      expect(screen.getByText(/delete exemption\?/i)).toBeInTheDocument();
    });

    const confirmButtons = screen.getAllByRole('button', { name: /^delete$/i });
    await user.click(confirmButtons[confirmButtons.length - 1]);

    await waitFor(() => {
      expect(deleteSpy).toHaveBeenCalledWith('exempt-1a2b3c4d5e6f');
    });
  });

  it('invokes approveExemption after confirming on a pending row', async () => {
    const approveSpy = vi
      .spyOn(complianceApi, 'approveExemption')
      .mockResolvedValue({ id: SAMPLE_PENDING.id, status: 'APPROVED' });

    const user = userEvent.setup();
    renderWithRouter(<ExemptionsPage />);

    await waitFor(() => {
      expect(screen.getByText('10.0.1.15')).toBeInTheDocument();
    });

    const approveButton = screen.getByRole('button', { name: /^approve$/i });
    await user.click(approveButton);

    const confirmButtons = await screen.findAllByRole('button', { name: /^approve$/i });
    await user.click(confirmButtons[confirmButtons.length - 1]);

    await waitFor(() => {
      expect(approveSpy).toHaveBeenCalledWith(SAMPLE_PENDING.id);
    });
  });

  it('creates a grant via createExemptionGrant from the filled form', async () => {
    const createSpy = vi.spyOn(complianceApi, 'createExemptionGrant').mockResolvedValue({ id: 'new-grant' } as never);
    const user = userEvent.setup();
    renderWithRouter(<ExemptionsPage />);
    await user.click(await screen.findByRole('button', { name: /create exemption/i }));

    await user.type(screen.getByPlaceholderText(/10\.0\.0\.0\/24 or 10\.0\.0\.5/), '10.0.0.9');
    await user.type(screen.getByPlaceholderText(/api\.openai\.com or \*\.openai\.com/), 'api.anthropic.com');
    await user.type(screen.getByPlaceholderText(/false positive investigation/), 'investigating a false positive');

    await user.click(screen.getByRole('button', { name: /^create$/i }));
    await waitFor(() => {
      expect(createSpy).toHaveBeenCalledWith(expect.objectContaining({
        sourceIp: '10.0.0.9', targetHost: 'api.anthropic.com', durationMinutes: 1440, reason: 'investigating a false positive',
      }));
    });
  });

  it('blocks creation with a too-short reason (no API call)', async () => {
    const createSpy = vi.spyOn(complianceApi, 'createExemptionGrant').mockResolvedValue({ id: 'x' } as never);
    const user = userEvent.setup();
    renderWithRouter(<ExemptionsPage />);
    await user.click(await screen.findByRole('button', { name: /create exemption/i }));

    await user.type(screen.getByPlaceholderText(/10\.0\.0\.0\/24 or 10\.0\.0\.5/), '10.0.0.9');
    await user.type(screen.getByPlaceholderText(/api\.openai\.com or \*\.openai\.com/), 'api.anthropic.com');
    await user.type(screen.getByPlaceholderText(/false positive investigation/), 'no'); // < 4 chars

    await user.click(screen.getByRole('button', { name: /^create$/i }));
    // validation short-circuits before the API; give the microtask queue a tick
    await new Promise((r) => setTimeout(r, 0));
    expect(createSpy).not.toHaveBeenCalled();
  });

  it('rejects a pending row via rejectExemption with the review note', async () => {
    const rejectSpy = vi.spyOn(complianceApi, 'rejectExemption').mockResolvedValue({ id: SAMPLE_PENDING.id, status: 'REJECTED' } as never);
    const user = userEvent.setup();
    renderWithRouter(<ExemptionsPage />);
    await waitFor(() => expect(screen.getByText('10.0.1.15')).toBeInTheDocument());

    await user.click(screen.getByRole('button', { name: /^reject$/i }));
    await user.type(await screen.findByPlaceholderText(/does not meet policy/), 'fails policy');
    const rejectButtons = screen.getAllByRole('button', { name: /^reject$/i });
    await user.click(rejectButtons[rejectButtons.length - 1]);

    await waitFor(() => expect(rejectSpy).toHaveBeenCalledWith(SAMPLE_PENDING.id, 'fails policy'));
  });
});
