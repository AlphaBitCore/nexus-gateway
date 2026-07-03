/**
 * useDuplicateNameCheck — advisory blur-time duplicate-name probe for create
 * forms whose names must be unique.
 *
 * Returns a `checkName(value)` callback to run when the user leaves the name
 * field. It asks the caller-supplied `isTaken` probe and, on a hit, sets an
 * inline field error so the user learns about the collision before filling in
 * the rest of the form. The check is advisory only: probe failures are
 * swallowed and never block input or submission — the server's 409 on create
 * remains the authoritative (race-proof) duplicate rejection.
 *
 * The error is set via react-hook-form's `setError`, so it clears as soon as
 * the user edits the field again (reValidateMode: 'onChange').
 */
import { useCallback, useRef } from 'react';
import type { FieldValues, Path, UseFormReturn } from 'react-hook-form';

export interface DuplicateNameCheckOptions<T extends FieldValues> {
  form: UseFormReturn<T>;
  field: Path<T>;
  /** Inline error message shown when the name is already taken. */
  message: string;
  /** Resolves true when `name` already exists in the relevant scope. */
  isTaken: (name: string) => Promise<boolean>;
}

export function useDuplicateNameCheck<T extends FieldValues>({
  form,
  field,
  message,
  isTaken,
}: DuplicateNameCheckOptions<T>) {
  // Monotonic sequence: a slow probe for an old value must not clobber the
  // state produced by a newer blur.
  const seq = useRef(0);

  return useCallback(
    async (raw: string) => {
      const name = raw.trim();
      if (!name) return;
      const mySeq = ++seq.current;
      try {
        const taken = await isTaken(name);
        if (seq.current !== mySeq) return; // stale response — a newer blur ran
        // The user may have re-focused and edited while the probe was in
        // flight; only flag the value the field still holds.
        const current = String(form.getValues(field) ?? '').trim();
        if (current !== name) return;
        if (taken) {
          form.setError(field, { type: 'duplicate', message });
        }
      } catch {
        // Advisory only — on probe failure stay silent; the create call's
        // 409 still rejects duplicates.
      }
    },
    [form, field, message, isTaken],
  );
}
