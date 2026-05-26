/**
 * ProxyHookFailureRateEditor — `proxy.hook_failure_rate` rule params editor.
 *
 * Shape: `{ thresholdPct: int[1..100]; windowSec: int≥60; minSamples: int≥1 }`.
 * Mirrors Hub schema in `builtin.go`. See ProxyHookTimeoutRateEditor for the
 * same shape bound to a different rule.
 */
import { useCallback, useMemo, type ChangeEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { FormField, Input, Stack } from '@/components/ui';
import type { RuleEditorProps } from './types';

type HookRateParams = {
  thresholdPct: number;
  windowSec: number;
  minSamples: number;
};

function toNumber(raw: unknown, fallback: number): number {
  if (typeof raw === 'number' && Number.isFinite(raw)) return raw;
  const parsed = Number(raw);
  return Number.isFinite(parsed) ? parsed : fallback;
}

function coerce(value: Record<string, unknown>): HookRateParams {
  return {
    thresholdPct: toNumber(value.thresholdPct, 0),
    windowSec: toNumber(value.windowSec, 0),
    minSamples: toNumber(value.minSamples, 0),
  };
}

function validate(p: HookRateParams): boolean {
  return (
    Number.isInteger(p.thresholdPct) && p.thresholdPct >= 1 && p.thresholdPct <= 100 &&
    Number.isInteger(p.windowSec) && p.windowSec >= 60 &&
    Number.isInteger(p.minSamples) && p.minSamples >= 1
  );
}

export function ProxyHookFailureRateEditor({ value, onChange, onValidate }: RuleEditorProps) {
  const { t } = useTranslation();
  const current = useMemo(() => coerce(value), [value]);

  const update = useCallback(
    (patch: Partial<HookRateParams>) => {
      const next = { ...current, ...patch };
      onChange({ ...value, ...next });
      onValidate?.(validate(next));
    },
    [current, onChange, onValidate, value],
  );

  const onThresholdPct = useCallback(
    (e: ChangeEvent<HTMLInputElement>) => update({ thresholdPct: Number(e.target.value) }),
    [update],
  );
  const onWindowSec = useCallback(
    (e: ChangeEvent<HTMLInputElement>) => update({ windowSec: Number(e.target.value) }),
    [update],
  );
  const onMinSamples = useCallback(
    (e: ChangeEvent<HTMLInputElement>) => update({ minSamples: Number(e.target.value) }),
    [update],
  );

  return (
    <Stack gap="md">
      <FormField
        label={t('pages:alerts.ruleEditors.hookRate.thresholdPctLabel')}
        helpText={t('pages:alerts.ruleEditors.hookRate.thresholdPctHelp')}
      >
        <Input
          type="number"
          min={1}
          max={100}
          step={1}
          value={String(current.thresholdPct)}
          onChange={onThresholdPct}
          aria-label={t('pages:alerts.ruleEditors.hookRate.thresholdPctLabel')}
        />
      </FormField>
      <FormField
        label={t('pages:alerts.ruleEditors.hookRate.windowSecLabel')}
        helpText={t('pages:alerts.ruleEditors.hookRate.windowSecHelp')}
      >
        <Input
          type="number"
          min={60}
          step={30}
          value={String(current.windowSec)}
          onChange={onWindowSec}
          aria-label={t('pages:alerts.ruleEditors.hookRate.windowSecLabel')}
        />
      </FormField>
      <FormField
        label={t('pages:alerts.ruleEditors.hookRate.minSamplesLabel')}
        helpText={t('pages:alerts.ruleEditors.hookRate.minSamplesHelp')}
      >
        <Input
          type="number"
          min={1}
          step={1}
          value={String(current.minSamples)}
          onChange={onMinSamples}
          aria-label={t('pages:alerts.ruleEditors.hookRate.minSamplesLabel')}
        />
      </FormField>
    </Stack>
  );
}
