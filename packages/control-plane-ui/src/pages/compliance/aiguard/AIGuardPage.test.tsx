/**
 * AIGuardPage (P-B Task 26) — renders the singleton config, flips the
 * external-URL warning banner when the radio changes, and POSTs the
 * draft on Save.
 *
 * Mocks the aiGuardApi service directly (per the task spec) so the test
 * stays focused on UI behaviour rather than HTTP wire details.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithProviders } from '@/test/test-utils';

vi.mock('@/api/services/compliance/aiguard', () => ({
  aiGuardApi: {
    getConfig: vi.fn().mockResolvedValue({
      id: 'singleton',
      backendMode: 'configured_provider',
      providerId: null,
      modelId: null,
      externalUrl: null,
      externalCredentialId: null,
      customHeaders: null,
      promptTemplate: 'default template',
      timeoutMs: 5000,
      cacheTtlSeconds: 600,
      backendFingerprint: 'fp-1',
    }),
    saveConfig: vi.fn().mockResolvedValue({
      id: 'singleton',
      backendMode: 'configured_provider',
      promptTemplate: 'default template',
      timeoutMs: 5000,
      cacheTtlSeconds: 600,
      backendFingerprint: 'fp-1',
    }),
    dryRun: vi.fn(),
  },
}));

// Import AFTER vi.mock so the page picks up the mocked module.
import { AIGuardPage } from './AIGuardPage';

beforeEach(() => {
  vi.clearAllMocks();
  Object.assign(navigator, {
    clipboard: {
      writeText: vi.fn().mockResolvedValue(undefined),
    },
  });
});

describe('AIGuardPage', () => {
  it('renders the current singleton config', async () => {
    renderWithProviders(<AIGuardPage />);

    await waitFor(() => {
      expect(screen.getByDisplayValue('5000')).toBeDefined();
    });
    expect(screen.getByDisplayValue('600')).toBeDefined();
    expect(screen.getByDisplayValue('default template')).toBeDefined();
  });

  it('shows the external-URL warning banner only when that mode is selected', async () => {
    renderWithProviders(<AIGuardPage />);

    await waitFor(() => {
      expect(screen.getByDisplayValue('5000')).toBeDefined();
    });

    // Initially the banner is hidden because backendMode = configured_provider.
    expect(screen.queryByRole('alert')).toBeNull();

    const externalRadio = screen.getByRole('radio', { name: /external url|外部/i });
    await userEvent.click(externalRadio);

    await waitFor(() => {
      expect(screen.getByRole('alert')).toBeDefined();
    });
  });

  it('calls saveConfig on submit', async () => {
    const { aiGuardApi } = await import('@/api/services/compliance/aiguard');
    renderWithProviders(<AIGuardPage />);

    await waitFor(() => {
      expect(screen.getByDisplayValue('5000')).toBeDefined();
    });

    const saveBtn = screen.getByRole('button', { name: /save|保存/i });
    await userEvent.click(saveBtn);

    await waitFor(() => {
      expect(aiGuardApi.saveConfig).toHaveBeenCalledTimes(1);
    });
    const firstCall = (aiGuardApi.saveConfig as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(firstCall.backendMode).toBe('configured_provider');
    expect(firstCall.timeoutMs).toBe(5000);
    expect(firstCall.cacheTtlSeconds).toBe(600);
  });

  it('shows and copies the compliance webhook URL', async () => {
    renderWithProviders(<AIGuardPage />);
    await waitFor(() => {
      expect(screen.getByDisplayValue(/\/v1\/ai-guard\/compliance-webhook$/i)).toBeDefined();
    });

    const copyBtn = screen.getByRole('button', { name: /copy url/i });
    await userEvent.click(copyBtn);
    await waitFor(() => {
      expect(navigator.clipboard.writeText).toHaveBeenCalledTimes(1);
    });
  });
});
