import { describe, it, expect, vi } from 'vitest';
import { renderHook, act, waitFor } from '@testing-library/react';
import { useForm } from 'react-hook-form';
import { useDuplicateNameCheck } from './useDuplicateNameCheck';

type Values = { name: string };

function setup(isTaken: (name: string) => Promise<boolean>) {
  return renderHook(() => {
    const form = useForm<Values>({ defaultValues: { name: '' } });
    const check = useDuplicateNameCheck({
      form,
      field: 'name',
      message: 'name already taken',
      isTaken,
    });
    return { form, check };
  });
}

describe('useDuplicateNameCheck', () => {
  it('sets the field error when the probe reports the name as taken', async () => {
    const { result } = setup(async () => true);
    act(() => result.current.form.setValue('name', 'dup'));
    await act(() => result.current.check('dup'));
    await waitFor(() => {
      expect(result.current.form.getFieldState('name').error?.message).toBe('name already taken');
    });
  });

  it('leaves the field clean when the name is available', async () => {
    const { result } = setup(async () => false);
    act(() => result.current.form.setValue('name', 'fresh'));
    await act(() => result.current.check('fresh'));
    expect(result.current.form.getFieldState('name').error).toBeUndefined();
  });

  it('skips empty and whitespace-only values without probing', async () => {
    const probe = vi.fn(async () => true);
    const { result } = setup(probe);
    await act(() => result.current.check(''));
    await act(() => result.current.check('   '));
    expect(probe).not.toHaveBeenCalled();
    expect(result.current.form.getFieldState('name').error).toBeUndefined();
  });

  it('stays silent when the probe fails (advisory only, server 409 is authoritative)', async () => {
    const { result } = setup(async () => { throw new Error('network down'); });
    act(() => result.current.form.setValue('name', 'dup'));
    await act(() => result.current.check('dup'));
    expect(result.current.form.getFieldState('name').error).toBeUndefined();
  });

  it('ignores a taken verdict if the field value changed while the probe was in flight', async () => {
    let resolveProbe!: (v: boolean) => void;
    const { result } = setup(() => new Promise<boolean>(r => { resolveProbe = r; }));
    act(() => result.current.form.setValue('name', 'old-name'));
    let pending!: Promise<void>;
    act(() => { pending = result.current.check('old-name'); });
    // User re-focused and edited before the probe answered.
    act(() => result.current.form.setValue('name', 'new-name'));
    resolveProbe(true);
    await act(() => pending);
    expect(result.current.form.getFieldState('name').error).toBeUndefined();
  });

  it('drops a stale probe response when a newer blur superseded it', async () => {
    const resolvers: Array<(v: boolean) => void> = [];
    const { result } = setup(() => new Promise<boolean>(r => { resolvers.push(r); }));
    act(() => result.current.form.setValue('name', 'first'));
    let firstCheck!: Promise<void>;
    act(() => { firstCheck = result.current.check('first'); });

    act(() => result.current.form.setValue('name', 'second'));
    let secondCheck!: Promise<void>;
    act(() => { secondCheck = result.current.check('second'); });

    // Second probe answers "available" first, then the stale first probe
    // answers "taken" — the stale verdict must not surface an error.
    resolvers[1](false);
    await act(() => secondCheck);
    resolvers[0](true);
    await act(() => firstCheck);
    expect(result.current.form.getFieldState('name').error).toBeUndefined();
  });
});
