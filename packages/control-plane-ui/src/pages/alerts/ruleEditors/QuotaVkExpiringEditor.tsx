/**
 * QuotaVkExpiringEditor — `quota.vk_expiring` rule params editor.
 *
 * Shape: `{ warnDays: number[] }` where each entry is an integer ≥ 1
 * representing "fire an alert this many days before the Virtual Key's
 * expiry". Hub schema sets no upper bound; we mirror that client-side.
 */
import { useCallback, useEffect, useMemo, useState, type ChangeEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { FormField, Input } from '@/components/ui';
import type { RuleEditorProps } from './types';

const MIN = 1;

function parseList(raw: string): number[] {
  return raw
    .split(',')
    .map((tok) => tok.trim())
    .filter((tok) => tok.length > 0)
    .map((tok) => Number(tok))
    .filter((n) => Number.isInteger(n) && n >= MIN);
}

function formatList(arr: unknown): string {
  if (!Array.isArray(arr)) return '';
  return arr.filter((n) => typeof n === 'number').join(', ');
}

export function QuotaVkExpiringEditor({ value, onChange, onValidate }: RuleEditorProps) {
  const { t } = useTranslation();
  const external = useMemo(() => formatList(value.warnDays), [value.warnDays]);
  const [raw, setRaw] = useState(external);

  // Re-sync on external change (initial fetch, Reset). Preserve the typed
  // string when it round-trips to the same canonical list to avoid clobbering
  // whitespace/ordering the user typed.
  useEffect(() => {
    const roundtripped = parseList(raw).join(', ');
    if (roundtripped !== external) setRaw(external);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [external]);

  const commit = useCallback(
    (next: string) => {
      const parsed = parseList(next);
      onChange({ ...value, warnDays: parsed });
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
      label={t('pages:alerts.ruleEditors.quotaVkExpiring.warnDaysLabel')}
      helpText={t('pages:alerts.ruleEditors.quotaVkExpiring.warnDaysHelp')}
    >
      <Input
        type="text"
        value={raw}
        onChange={onInputChange}
        onBlur={onBlur}
        placeholder={t('pages:alerts.ruleEditors.quotaVkExpiring.warnDaysPlaceholder')}
        aria-label={t('pages:alerts.ruleEditors.quotaVkExpiring.warnDaysLabel')}
      />
    </FormField>
  );
}
