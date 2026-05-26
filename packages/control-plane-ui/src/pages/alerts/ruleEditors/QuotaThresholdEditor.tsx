/**
 * QuotaThresholdEditor — `quota.threshold` rule params editor.
 *
 * Shape: `{ thresholds: number[] }` where each entry is an integer in [1,100]
 * representing a percent-of-limit crossing that should fire the alert.
 *
 * The schema's validity bounds are hardcoded client-side to match Hub's
 * builtin registry (`packages/nexus-hub/internal/alerting/rules/builtin.go`):
 * integer, min 1, max 100, at least one item. We deliberately do not try to
 * re-derive these from the passed-in JSON Schema because the Hub side is the
 * source of truth on reject/accept — this editor's job is to nudge the user
 * toward a valid input, not to replicate server-side validation.
 */
import { useCallback, useEffect, useMemo, useState, type ChangeEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { FormField, Input } from '@/components/ui';
import type { RuleEditorProps } from './types';

const MIN = 1;
const MAX = 100;

function parseList(raw: string): number[] {
  return raw
    .split(',')
    .map((tok) => tok.trim())
    .filter((tok) => tok.length > 0)
    .map((tok) => Number(tok))
    .filter((n) => Number.isInteger(n) && n >= MIN && n <= MAX);
}

function formatList(arr: unknown): string {
  if (!Array.isArray(arr)) return '';
  return arr.filter((n) => typeof n === 'number').join(', ');
}

export function QuotaThresholdEditor({ value, onChange, onValidate }: RuleEditorProps) {
  const { t } = useTranslation();
  const external = useMemo(() => formatList(value.thresholds), [value.thresholds]);
  // Local buffer so the user sees every keystroke (including not-yet-valid
  // tokens) between onChange/onBlur cycles. We sync to the external formatted
  // list whenever it changes in a way that doesn't match our parsed view —
  // this covers initial fetch, Reset, and any outside mutation of params.
  const [raw, setRaw] = useState(external);

  useEffect(() => {
    // If the external list round-trips to the same string as what the user
    // typed, keep the typed string (preserves formatting differences like
    // trailing commas). Otherwise replace with the authoritative external
    // value.
    const roundtripped = parseList(raw).join(', ');
    if (roundtripped !== external) {
      setRaw(external);
    }
    // Only depend on `external` — we intentionally read `raw` through closure
    // and don't want re-syncs when the user types.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [external]);

  const commit = useCallback(
    (next: string) => {
      const parsed = parseList(next);
      onChange({ ...value, thresholds: parsed });
      onValidate?.(parsed.length >= 1);
    },
    [onChange, onValidate, value],
  );

  const onInputChange = useCallback((e: ChangeEvent<HTMLInputElement>) => {
    setRaw(e.target.value);
  }, []);

  const onBlur = useCallback(() => {
    commit(raw);
  }, [commit, raw]);

  return (
    <FormField
      label={t('pages:alerts.ruleEditors.quotaThreshold.thresholdsLabel')}
      helpText={t('pages:alerts.ruleEditors.quotaThreshold.thresholdsHelp')}
    >
      <Input
        type="text"
        value={raw}
        onChange={onInputChange}
        onBlur={onBlur}
        placeholder={t('pages:alerts.ruleEditors.quotaThreshold.thresholdsPlaceholder')}
        aria-label={t('pages:alerts.ruleEditors.quotaThreshold.thresholdsLabel')}
      />
    </FormField>
  );
}
