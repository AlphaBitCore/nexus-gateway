import { useState } from 'react';
import { describe, it, expect, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import '@testing-library/jest-dom/vitest';

import { renderWithProviders } from '@/test/test-utils';
import { ProxyHookTimeoutRateEditor } from './ProxyHookTimeoutRateEditor';

function Host({
  initial,
  onChangeSpy,
}: {
  initial: Record<string, unknown>;
  onChangeSpy?: (v: Record<string, unknown>) => void;
}) {
  const [value, setValue] = useState(initial);
  return (
    <ProxyHookTimeoutRateEditor
      value={value}
      schema={{}}
      onChange={(next) => {
        setValue(next);
        onChangeSpy?.(next);
      }}
    />
  );
}

describe('ProxyHookTimeoutRateEditor', () => {
  it('renders initial values for each field', () => {
    renderWithProviders(
      <Host initial={{ thresholdPct: 15, windowSec: 300, minSamples: 25 }} />,
    );
    expect(screen.getByDisplayValue('15')).toBeInTheDocument();
    expect(screen.getByDisplayValue('300')).toBeInTheDocument();
    expect(screen.getByDisplayValue('25')).toBeInTheDocument();
  });

  it('calls onChange when windowSec changes', async () => {
    const user = userEvent.setup();
    const onChangeSpy = vi.fn();
    renderWithProviders(
      <Host
        initial={{ thresholdPct: 10, windowSec: 300, minSamples: 50 }}
        onChangeSpy={onChangeSpy}
      />,
    );
    const input = screen.getByDisplayValue('300');
    await user.clear(input);
    await user.type(input, '600');
    expect(onChangeSpy).toHaveBeenLastCalledWith(
      expect.objectContaining({ windowSec: 600, thresholdPct: 10, minSamples: 50 }),
    );
  });
});
