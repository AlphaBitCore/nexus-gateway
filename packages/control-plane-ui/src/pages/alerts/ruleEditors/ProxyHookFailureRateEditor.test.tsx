import { useState } from 'react';
import { describe, it, expect, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import '@testing-library/jest-dom/vitest';

import { renderWithProviders } from '@/test/test-utils';
import { ProxyHookFailureRateEditor } from './ProxyHookFailureRateEditor';

/** Host wrapper that threads the editor's onChange back into its value so the
 *  controlled Input reflects each keystroke. Mirrors how AlertRuleEditPage
 *  hosts the editor via `setParams` in production. */
function Host({
  initial,
  onChangeSpy,
}: {
  initial: Record<string, unknown>;
  onChangeSpy?: (v: Record<string, unknown>) => void;
}) {
  const [value, setValue] = useState(initial);
  return (
    <ProxyHookFailureRateEditor
      value={value}
      schema={{}}
      onChange={(next) => {
        setValue(next);
        onChangeSpy?.(next);
      }}
    />
  );
}

describe('ProxyHookFailureRateEditor', () => {
  it('renders initial values', () => {
    renderWithProviders(
      <Host initial={{ thresholdPct: 20, windowSec: 300, minSamples: 10 }} />,
    );
    expect(screen.getByDisplayValue('20')).toBeInTheDocument();
    expect(screen.getByDisplayValue('300')).toBeInTheDocument();
    expect(screen.getByDisplayValue('10')).toBeInTheDocument();
  });

  it('calls onChange with merged params when thresholdPct changes', async () => {
    const user = userEvent.setup();
    const onChangeSpy = vi.fn();
    renderWithProviders(
      <Host
        initial={{ thresholdPct: 20, windowSec: 300, minSamples: 10 }}
        onChangeSpy={onChangeSpy}
      />,
    );
    const input = screen.getByDisplayValue('20');
    await user.clear(input);
    await user.type(input, '50');
    expect(onChangeSpy).toHaveBeenLastCalledWith(
      expect.objectContaining({ thresholdPct: 50, windowSec: 300, minSamples: 10 }),
    );
  });
});
