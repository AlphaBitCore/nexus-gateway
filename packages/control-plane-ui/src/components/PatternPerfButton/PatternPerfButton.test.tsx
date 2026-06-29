import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { I18nextProvider } from 'react-i18next';
import { describe, it, expect, vi } from 'vitest';

import i18n from '@/i18n';
import { rulePacksApi } from '@/api/services';

import { PatternPerfButton } from './PatternPerfButton';

vi.mock('@/api/services', async (orig) => {
  const actual = await orig<typeof import('@/api/services')>();
  return { ...actual, rulePacksApi: { ...actual.rulePacksApi, patternPerfTest: vi.fn() } };
});

function renderBtn(pattern: string, flags?: string) {
  return render(
    <I18nextProvider i18n={i18n}>
      <PatternPerfButton pattern={pattern} flags={flags} />
    </I18nextProvider>,
  );
}

describe('PatternPerfButton', () => {
  it('forwards the pattern+flags and surfaces the verdict, metrics, and suggestions', async () => {
    vi.mocked(rulePacksApi.patternPerfTest).mockResolvedValue({
      compiles: true,
      findings: [],
      cleanScanUs: 20,
      adversarialScanUs: 812,
      verdict: 'slow',
      suggestions: ['Add a required literal substring so the prefilter can skip benign input.'],
    });
    renderBtn('[A-Za-z0-9]+', 'i');
    fireEvent.click(screen.getByRole('button'));
    await waitFor(() =>
      expect(screen.getByText(/Add a required literal substring/)).toBeInTheDocument(),
    );
    expect(rulePacksApi.patternPerfTest).toHaveBeenCalledWith('[A-Za-z0-9]+', 'i');
    // metrics show the measured µs
    expect(screen.getByText(/812µs/)).toBeInTheDocument();
  });

  it('disables the button when the pattern is blank', () => {
    renderBtn('   ');
    expect(screen.getByRole('button')).toBeDisabled();
  });

  it('shows an error when the gateway is unreachable', async () => {
    vi.mocked(rulePacksApi.patternPerfTest).mockResolvedValue({
      compiles: false,
      findings: [],
      cleanScanUs: 0,
      adversarialScanUs: 0,
      verdict: 'invalid',
      suggestions: [],
      success: false,
      error: 'AI Gateway unreachable: dial tcp',
    });
    renderBtn('secret');
    fireEvent.click(screen.getByRole('button'));
    await waitFor(() => expect(screen.getByText(/unreachable/i)).toBeInTheDocument());
  });
});
