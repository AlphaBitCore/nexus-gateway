/**
 * DryRunPanel (P-B Task 27) — fires `aiGuardApi.dryRun` and surfaces the
 * decision + latency + cache-hit metadata.
 */
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithProviders } from '@/test/test-utils';
import type { AIGuardConfig } from '@/api/services/compliance/aiguard';

vi.mock('@/api/services/compliance/aiguard', () => ({
  aiGuardApi: {
    getConfig: vi.fn(),
    saveConfig: vi.fn(),
    dryRun: vi.fn().mockResolvedValue({
      request: {
        detector_type: 'prompt_injection',
        content: 'test content',
        context: { ingress: 'AI_GATEWAY' },
      },
      response: {
        decision: 'approve',
        confidence: 0.92,
        reason: 'benign',
        labels: [],
        // Legacy `modified_content` field replaced by `redactions: []`.
        redactions: [],
        metadata: {
          judge_model: 'gpt-4o-mini',
          judge_latency_ms: 120,
          cache_hit: false,
          backend_mode: 'configured_provider',
        },
      },
    }),
  },
}));

import { DryRunPanel } from './DryRunPanel';

const BASE_CONFIG: AIGuardConfig = {
  id: 'singleton',
  backendMode: 'configured_provider',
  providerId: null,
  modelId: null,
  externalUrl: null,
  externalCredentialId: null,
  customHeaders: null,
  promptTemplate: '',
  timeoutMs: 5000,
  cacheTtlSeconds: 600,
  backendFingerprint: '',
};

beforeEach(() => {
  vi.clearAllMocks();
});

describe('DryRunPanel', () => {
  it('fires dry-run and shows response decision + latency', async () => {
    renderWithProviders(<DryRunPanel currentConfig={BASE_CONFIG} />);

    const textarea = screen.getByRole('textbox');
    await userEvent.type(textarea, 'test content');

    const runBtn = screen.getByRole('button', { name: /run|运行/i });
    await userEvent.click(runBtn);

    await waitFor(() => {
      expect(screen.getByTestId('dry-run-decision').textContent).toMatch(/approve/i);
    });
    expect(screen.getByTestId('dry-run-latency').textContent).toBe('120');
    // Request + response JSON blocks are rendered as <pre> — at least one of
    // them must echo the detector_type from the mocked response body.
    const preBlocks = document.querySelectorAll('pre');
    const anyMatch = Array.from(preBlocks).some((pre) =>
      /prompt_injection/.test(pre.textContent ?? ''),
    );
    expect(anyMatch).toBe(true);
  });
});
