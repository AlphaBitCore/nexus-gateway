import { useState } from 'react';
import { describe, it, expect, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import '@testing-library/jest-dom/vitest';

import { renderWithProviders } from '@/test/test-utils';
import { GenericJsonEditor } from './GenericJsonEditor';

function Host({
  initial,
  schema,
  onChangeSpy,
}: {
  initial: Record<string, unknown>;
  schema: Record<string, unknown>;
  onChangeSpy?: (v: Record<string, unknown>) => void;
}) {
  const [value, setValue] = useState(initial);
  return (
    <GenericJsonEditor
      value={value}
      schema={schema}
      onChange={(next) => {
        setValue(next);
        onChangeSpy?.(next);
      }}
    />
  );
}

describe('GenericJsonEditor', () => {
  it('renders one input per schema property', () => {
    renderWithProviders(
      <Host
        initial={{ offlineAfterSec: 300, excludeKinds: ['agent'] }}
        schema={{
          type: 'object',
          properties: {
            offlineAfterSec: { type: 'integer', minimum: 60 },
            excludeKinds: { type: 'array', items: { type: 'string' } },
          },
        }}
      />,
    );
    expect(screen.getByDisplayValue('300')).toBeInTheDocument();
    expect(screen.getByDisplayValue('agent')).toBeInTheDocument();
  });

  it('emits onChange for integer fields', async () => {
    const user = userEvent.setup();
    const onChangeSpy = vi.fn();
    renderWithProviders(
      <Host
        initial={{ minDownSec: 120, recoverySec: 60 }}
        schema={{
          type: 'object',
          properties: {
            minDownSec: { type: 'integer', minimum: 30 },
            recoverySec: { type: 'integer', minimum: 30 },
          },
        }}
        onChangeSpy={onChangeSpy}
      />,
    );
    const input = screen.getByDisplayValue('120');
    await user.clear(input);
    await user.type(input, '150');
    expect(onChangeSpy).toHaveBeenLastCalledWith(
      expect.objectContaining({ minDownSec: 150, recoverySec: 60 }),
    );
  });

  it('falls back to a JSON textarea for empty-object schemas', () => {
    renderWithProviders(
      <Host initial={{ foo: 'bar' }} schema={{ type: 'object' }} />,
    );
    const textarea = screen.getByRole('textbox') as HTMLTextAreaElement;
    expect(textarea.value).toContain('"foo"');
    expect(textarea.value).toContain('"bar"');
  });

  it('JSON fallback parses valid JSON on blur and calls onChange', async () => {
    const user = userEvent.setup();
    const onChangeSpy = vi.fn();
    renderWithProviders(
      <Host initial={{}} schema={{ type: 'object' }} onChangeSpy={onChangeSpy} />,
    );
    const textarea = screen.getByRole('textbox');
    await user.clear(textarea);
    await user.type(textarea, '{{"a":1}');
    textarea.blur();
    expect(onChangeSpy).toHaveBeenLastCalledWith({ a: 1 });
  });
});
