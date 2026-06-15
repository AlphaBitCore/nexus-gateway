import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';

import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { reliabilitySettingsApi } from '@/api/services';
import {
  Card, Stack, Button, FormField, Input, Skeleton, ErrorBanner,
} from '@/components/ui';
import type { ReliabilityConfigResponse, ReliabilityThresholds } from '@/api/types';

import styles from './SettingsCredentialReliabilityTab.module.css';

// SettingsCredentialReliabilityTab is the global reliability thresholds editor
// on the Settings page. It writes the gateway.credential_reliability.config
// row in system_metadata, which the Hub picks up on every job tick and the AI
// Gateway picks up via the thingclient "credential_reliability"
// OnConfigChanged callback.
//
// All seven fields are required and validated server-side; the form
// only surfaces obvious client-side guardrails so admins can correct
// mistakes before hitting Save.

const FIELDS: (keyof ReliabilityThresholds)[] = [
  'authFailThreshold',
  'rateLimitCooldownSeconds',
  'healthyThresholdPct',
  'degradedThresholdPct',
  'healthMinSamples',
  'healthWindowSeconds',
  'healthSustainedDegradedSeconds',
];

type FormState = Record<keyof ReliabilityThresholds, string>;

function formFromThresholds(t: ReliabilityThresholds): FormState {
  return {
    authFailThreshold: String(t.authFailThreshold ?? ''),
    rateLimitCooldownSeconds: String(t.rateLimitCooldownSeconds ?? ''),
    healthyThresholdPct: String(t.healthyThresholdPct ?? ''),
    degradedThresholdPct: String(t.degradedThresholdPct ?? ''),
    healthMinSamples: String(t.healthMinSamples ?? ''),
    healthWindowSeconds: String(t.healthWindowSeconds ?? ''),
    healthSustainedDegradedSeconds: String(t.healthSustainedDegradedSeconds ?? ''),
  };
}

function formToThresholds(f: FormState): ReliabilityThresholds {
  const out: ReliabilityThresholds = {};
  for (const k of FIELDS) {
    const n = Number(f[k]);
    if (Number.isFinite(n) && n > 0) {
      out[k] = n;
    }
  }
  return out;
}

function validate(f: FormState, t: ReturnType<typeof useTranslation>['t']): string | null {
  for (const k of FIELDS) {
    const n = Number(f[k]);
    if (!Number.isFinite(n) || n <= 0) {
      return t('pages:settings.reliability.fieldPositive', { field: t(`pages:credentials.${k}`) });
    }
  }
  const healthy = Number(f.healthyThresholdPct);
  const degraded = Number(f.degradedThresholdPct);
  if (degraded >= healthy) {
    return t('pages:settings.reliability.degradedLessThanHealthy');
  }
  if (healthy > 100) {
    return t('pages:settings.reliability.healthyAtMost100');
  }
  return null;
}

export function SettingsCredentialReliabilityTab() {
  const { t } = useTranslation();

  const { data, loading, error, refetch } = useApi<ReliabilityConfigResponse>(
    () => reliabilitySettingsApi.get(),
    ['admin', 'settings', 'credential-reliability'],
  );

  const [form, setForm] = useState<FormState | null>(null);
  const [validationErr, setValidationErr] = useState<string | null>(null);

  useEffect(() => {
    if (data) setForm(formFromThresholds(data.effective));
  }, [data]);

  const { mutate: save, loading: saving } = useMutation(
    () => {
      if (!form) return Promise.resolve(null);
      const body = formToThresholds(form);
      return reliabilitySettingsApi.update(body);
    },
    {
      successMessage: t('pages:settings.reliability.saved'),
      onSuccess: () => { void refetch(); },
    },
  );

  if (loading || !data || !form) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const onChange = (k: keyof ReliabilityThresholds) => (e: React.ChangeEvent<HTMLInputElement>) => {
    setForm({ ...form, [k]: e.target.value });
    setValidationErr(null);
  };
  const onSave = () => {
    const err = validate(form, t);
    if (err) {
      setValidationErr(err);
      return;
    }
    save(undefined as never);
  };
  const onResetDefaults = () => {
    setForm(formFromThresholds(data.defaults));
    setValidationErr(null);
  };

  return (
    <div className={styles.layout}>
      <div className={styles.header}>
        <h2 className={styles.title}>{t('pages:settings.reliability.title')}</h2>
        <p className={styles.help}>{t('pages:settings.reliability.subtitle')}</p>
      </div>

      <Card>
        <div className={styles.grid}>
          {FIELDS.map((k) => (
            <FormField
              key={k}
              label={t(`pages:credentials.${k}`)}
              helpText={t(`pages:credentials.${k}Help`)}
            >
              <Input type="number" min="1" value={form[k]} onChange={onChange(k)} />
            </FormField>
          ))}
        </div>
      </Card>

      {validationErr && <div className={styles.validationErr}>{validationErr}</div>}

      <Stack direction="horizontal" gap="sm" className={styles.actions}>
        <Button className={styles.primaryAction} onClick={onSave} loading={saving}>{t('common:save')}</Button>
        <Button className={styles.secondaryAction} variant="secondary" onClick={onResetDefaults}>
          {t('pages:settings.reliability.resetDefaults')}
        </Button>
      </Stack>

      <p className={styles.afterSaveNote}>{t('pages:settings.reliability.propagateNote')}</p>
    </div>
  );
}
