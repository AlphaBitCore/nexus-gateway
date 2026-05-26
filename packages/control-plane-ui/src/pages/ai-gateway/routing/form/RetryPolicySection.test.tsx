/**
 * Unit tests — RetryPolicySection.
 *
 * Covers UX behavior (default vs custom mode, control disabling) plus the
 * pure helpers that the create + edit hooks call to serialize the wire
 * payload (per docs/users/api/openapi/admin/e34-s3-routing-retry-policy.yaml §6.3).
 */
import { useState } from 'react';
import { describe, it, expect } from 'vitest';
import { fireEvent, screen } from '@testing-library/react';

import { renderWithProviders } from '@/test/test-utils';
import type { ErrorClass } from '@/api/types';
import {
  RetryPolicySection,
  buildRetryPolicyPayload,
  deriveRetryPolicyInitialState,
  isRetryPolicyMaxAttemptsInvalid,
  type RetryPolicyMode,
} from './RetryPolicySection';

function Harness({
  initialMode = 'default',
  initialAttempts = '3',
  initialRetryOn = ['network', 'timeout', '5xx'] as ErrorClass[],
}: {
  initialMode?: RetryPolicyMode;
  initialAttempts?: string;
  initialRetryOn?: ErrorClass[];
}) {
  const [mode, setMode] = useState<RetryPolicyMode>(initialMode);
  const [attempts, setAttempts] = useState(initialAttempts);
  const [retryOn, setRetryOn] = useState<ErrorClass[]>(initialRetryOn);
  return (
    <RetryPolicySection
      mode={mode}
      onModeChange={setMode}
      maxAttempts={attempts}
      onMaxAttemptsChange={setAttempts}
      retryOn={retryOn}
      onRetryOnChange={setRetryOn}
    />
  );
}

describe('RetryPolicySection — UI', () => {
  it('defaults to "Use platform default" radio selected', () => {
    renderWithProviders(<Harness />);
    const defaultRadio = screen.getByTestId('retry-policy-mode-default') as HTMLInputElement;
    const customRadio = screen.getByTestId('retry-policy-mode-custom') as HTMLInputElement;
    expect(defaultRadio.checked).toBe(true);
    expect(customRadio.checked).toBe(false);
  });

  it('disables controls in default mode', () => {
    renderWithProviders(<Harness />);
    const input = screen.getByTestId('retry-max-attempts-input') as HTMLInputElement;
    const cb = screen.getByTestId('retry-on-network') as HTMLInputElement;
    expect(input.disabled).toBe(true);
    expect(cb.disabled).toBe(true);
  });

  it('switching to Custom enables controls', () => {
    renderWithProviders(<Harness />);
    fireEvent.click(screen.getByTestId('retry-policy-mode-custom'));
    const input = screen.getByTestId('retry-max-attempts-input') as HTMLInputElement;
    const cb = screen.getByTestId('retry-on-network') as HTMLInputElement;
    expect(input.disabled).toBe(false);
    expect(cb.disabled).toBe(false);
  });

  it('renders all four error-class options', () => {
    renderWithProviders(<Harness />);
    expect(screen.getByTestId('retry-on-network')).toBeDefined();
    expect(screen.getByTestId('retry-on-timeout')).toBeDefined();
    expect(screen.getByTestId('retry-on-429')).toBeDefined();
    expect(screen.getByTestId('retry-on-5xx')).toBeDefined();
  });
});

describe('deriveRetryPolicyInitialState', () => {
  it('null/undefined → default mode with seed values', () => {
    expect(deriveRetryPolicyInitialState(null)).toEqual({
      mode: 'default',
      maxAttempts: '3',
      retryOn: ['network', 'timeout', '5xx'],
    });
    expect(deriveRetryPolicyInitialState(undefined)).toEqual({
      mode: 'default',
      maxAttempts: '3',
      retryOn: ['network', 'timeout', '5xx'],
    });
  });

  it('persisted policy → custom mode echoing the saved values', () => {
    expect(
      deriveRetryPolicyInitialState({ maxAttemptsPerTarget: 4, retryOn: ['429', '5xx'] }),
    ).toEqual({ mode: 'custom', maxAttempts: '4', retryOn: ['429', '5xx'] });
  });

  it('persisted policy with no maxAttempts falls back to seed string', () => {
    expect(deriveRetryPolicyInitialState({ retryOn: ['timeout'] })).toEqual({
      mode: 'custom',
      maxAttempts: '3',
      retryOn: ['timeout'],
    });
  });
});

describe('buildRetryPolicyPayload', () => {
  it('default mode → ok with mode=default (no value)', () => {
    const out = buildRetryPolicyPayload('default', '3', ['network']);
    expect(out).toEqual({ ok: true, mode: 'default' });
  });

  it('custom mode with valid attempts + retryOn → ok with structured value', () => {
    const out = buildRetryPolicyPayload('custom', '4', ['5xx', 'timeout']);
    expect(out).toEqual({
      ok: true,
      mode: 'custom',
      value: { maxAttemptsPerTarget: 4, retryOn: ['5xx', 'timeout'] },
    });
  });

  it('custom mode with empty maxAttempts → omits maxAttemptsPerTarget', () => {
    const out = buildRetryPolicyPayload('custom', '', ['5xx']);
    expect(out).toEqual({ ok: true, mode: 'custom', value: { retryOn: ['5xx'] } });
  });

  it('custom mode with empty retryOn → emits empty array (spec: "retry nothing")', () => {
    const out = buildRetryPolicyPayload('custom', '2', []);
    expect(out).toEqual({
      ok: true,
      mode: 'custom',
      value: { maxAttemptsPerTarget: 2, retryOn: [] },
    });
  });

  it.each([
    ['0', 'below minimum'],
    ['6', 'above maximum'],
    ['1.5', 'non-integer'],
    ['abc', 'non-numeric'],
  ])('custom mode with maxAttempts=%s → error (%s)', (raw) => {
    const out = buildRetryPolicyPayload('custom', raw, ['network']);
    expect(out.ok).toBe(false);
  });
});

describe('isRetryPolicyMaxAttemptsInvalid', () => {
  it('default mode is never invalid', () => {
    expect(isRetryPolicyMaxAttemptsInvalid('default', 'garbage')).toBe(false);
  });

  it('custom mode with empty value is not invalid (treated as unset)', () => {
    expect(isRetryPolicyMaxAttemptsInvalid('custom', '')).toBe(false);
  });

  it.each(['1', '3', '5'])('custom mode accepts %s', (n) => {
    expect(isRetryPolicyMaxAttemptsInvalid('custom', n)).toBe(false);
  });

  it.each(['0', '6', '-1', '2.5', 'foo'])('custom mode rejects %s', (n) => {
    expect(isRetryPolicyMaxAttemptsInvalid('custom', n)).toBe(true);
  });
});
