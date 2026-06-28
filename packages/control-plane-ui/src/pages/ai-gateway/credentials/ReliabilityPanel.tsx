import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import clsx from 'clsx';

import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { credentialApi } from '@/api/services';
import { Card, Stack, Button, FormField, Input } from '@/components/ui';
import type {
  Credential,
  CredentialProbeResult,
  ReliabilityThresholds,
} from '@/api/types';

import styles from './ReliabilityPanel.module.css';

// ReliabilityPanel is the "Reliability" tab body in the credential detail view. Owns:
//   * 8 s polling of GET /api/admin/credentials/:id so the live auth_fails
//     counter, circuit countdown, and health classification stay fresh
//     without a full page reload.
//   * Test Credential button — calls POST /credentials/:id/probe and
//     surfaces the result inline.
//   * Per-credential threshold overrides editor — writes to
//     PUT /credentials/:id/reliability-overrides.
//   * Circuit Reset shortcut, mirroring the existing button on the info tab.
//
// Polling stops automatically when the tab is unmounted.

const POLL_INTERVAL_MS = 8000;

type OverrideForm = Required<{
  [K in keyof ReliabilityThresholds]: string;
}>;

const THRESHOLD_FIELDS: { key: keyof OverrideForm; helper: string }[] = [
  { key: 'authFailThreshold', helper: 'authFailThresholdHelp' },
  { key: 'rateLimitCooldownSeconds', helper: 'rateLimitCooldownSecondsHelp' },
  { key: 'healthyThresholdPct', helper: 'healthyThresholdPctHelp' },
  { key: 'degradedThresholdPct', helper: 'degradedThresholdPctHelp' },
  { key: 'healthMinSamples', helper: 'healthMinSamplesHelp' },
  { key: 'healthWindowSeconds', helper: 'healthWindowSecondsHelp' },
  { key: 'healthSustainedDegradedSeconds', helper: 'healthSustainedDegradedSecondsHelp' },
];

function emptyForm(): OverrideForm {
  return {
    authFailThreshold: '',
    rateLimitCooldownSeconds: '',
    healthyThresholdPct: '',
    degradedThresholdPct: '',
    healthMinSamples: '',
    healthWindowSeconds: '',
    healthSustainedDegradedSeconds: '',
  };
}

function formFromOverride(o: ReliabilityThresholds | null | undefined): OverrideForm {
  const f = emptyForm();
  if (!o) return f;
  for (const key of Object.keys(f) as (keyof OverrideForm)[]) {
    const v = o[key];
    if (typeof v === 'number' && v > 0) {
      f[key] = String(v);
    }
  }
  return f;
}

function formToOverride(f: OverrideForm): ReliabilityThresholds {
  const out: ReliabilityThresholds = {};
  for (const key of Object.keys(f) as (keyof OverrideForm)[]) {
    const v = f[key].trim();
    if (v === '') continue;
    const n = Number(v);
    if (Number.isFinite(n) && n > 0) {
      out[key] = n;
    }
  }
  return out;
}

function isFormEmpty(f: OverrideForm): boolean {
  return Object.values(f).every((v) => v.trim() === '');
}

export interface ReliabilityPanelProps {
  credentialId: string;
  /** When false, edit + reset + probe buttons are hidden. */
  canEdit: boolean;
  /** Server-rendered seed; replaced by polling once mounted. */
  seed: Credential;
}

export function ReliabilityPanel({ credentialId, canEdit, seed }: ReliabilityPanelProps) {
  const { t } = useTranslation();

  const { data: live, refetch } = useApi<Credential>(
    () => credentialApi.get(credentialId),
    ['admin', 'credentials', 'detail', 'reliability', credentialId],
    { refetchInterval: POLL_INTERVAL_MS },
  );
  const c = live ?? seed;

  const [form, setForm] = useState<OverrideForm>(() => formFromOverride(c.reliabilityOverrides));
  const [editing, setEditing] = useState(false);

  const { mutate: saveOverrides, loading: savingOverrides } = useMutation(
    () => {
      const o = formToOverride(form);
      const payload = isFormEmpty(form) ? null : o;
      return credentialApi.updateReliabilityOverrides(credentialId, payload);
    },
    {
      invalidateQueries: [['api', 'admin', 'credentials']],
      successMessage: t('pages:credentials.overridesSaved'),
      onSuccess: () => { setEditing(false); void refetch(); },
    },
  );

  const [probe, setProbe] = useState<CredentialProbeResult | null>(null);
  const { mutate: runProbe, loading: probing } = useMutation(
    () => credentialApi.probe(credentialId),
    {
      onSuccess: (res: CredentialProbeResult) => setProbe(res),
      // Probe failures are surfaced inline (we still render res), not as toast.
      onError: () => setProbe(null),
    },
  );

  const { mutate: circuitReset, loading: resetting } = useMutation(
    () => credentialApi.circuitReset(credentialId),
    {
      invalidateQueries: [['api', 'admin', 'credentials']],
      successMessage: t('pages:credentials.circuitResetSuccess'),
      onSuccess: () => { void refetch(); },
    },
  );

  return (
    <Stack gap="lg">
      <div className={styles.sectionHeaderRow}>
        <h2 className={styles.widgetTitle}>{t('pages:credentials.reliabilityCurrent')}</h2>
        <span className={styles.refreshNote}>{t('pages:credentials.autoRefreshes8s')}</span>
      </div>
      <Card>
        <ReliabilitySummary c={c} />

        {canEdit && (
          <Stack direction="horizontal" gap="sm" className={`${styles.actionRow} ${styles.probeActionRow}`}>
            <Button variant="secondary" size="sm" onClick={() => runProbe(undefined as never)} loading={probing}>
              {t('pages:credentials.testCredential')}
            </Button>
            {(c.circuitState === 'open' || c.circuitState === 'half_open') && (
              <Button variant="secondary" size="sm" onClick={() => circuitReset(undefined as never)} loading={resetting}>
                {t('pages:credentials.circuitReset')}
              </Button>
            )}
          </Stack>
        )}

        {probe && <ProbeResultPanel result={probe} />}
      </Card>

      <div className={styles.sectionHeaderRow}>
        <h2 className={styles.widgetTitle}>{t('pages:credentials.thresholdOverridesTitle')}</h2>
        {canEdit && !editing && (
          <Button onClick={() => setEditing(true)}>
            {t('common:edit')}
          </Button>
        )}
      </div>
      <Card>
        <p className={styles.helpText}>{t('pages:credentials.thresholdOverridesHelp')}</p>

        {editing ? (
          <ThresholdEditor form={form} setForm={setForm} />
        ) : (
          <ThresholdDisplay overrides={c.reliabilityOverrides ?? null} t={t} />
        )}

        {editing && canEdit && (
          <Stack direction="horizontal" gap="sm" className={styles.actionRow}>
            <Button onClick={() => saveOverrides(undefined as never)} loading={savingOverrides}>
              {t('common:save')}
            </Button>
            <Button variant="secondary" onClick={() => { setForm(formFromOverride(c.reliabilityOverrides)); setEditing(false); }}>
              {t('common:cancel')}
            </Button>
            <Button
              variant="ghost"
              onClick={() => { setForm(emptyForm()); }}
              title={t('pages:credentials.clearOverrides')}
            >
              {t('pages:credentials.clearOverrides')}
            </Button>
          </Stack>
        )}
      </Card>
    </Stack>
  );
}

// Subcomponents below are kept in the same file (split if any are reused elsewhere).

function ReliabilitySummary({ c }: { c: Credential }) {
  const { t, i18n } = useTranslation();
  const rate5m = c.healthSuccessRate5m;
  const rate1h = c.healthSuccessRate1h;
  const samples = c.healthSamplesObserved ?? 0;
  return (
    <div className={styles.summaryGrid}>
      <div className={styles.summaryItem}>
        <div className={styles.summaryLabel}>{t('pages:credentials.health')}</div>
        <div className={styles.summaryValue}>
          <span className={clsx(styles.statusBadge, toneClass(c))}>
            {t(`pages:credentials.health_${c.healthStatus ?? 'unknown'}`, { defaultValue: c.healthStatus ?? 'unknown' })}
          </span>
          {c.healthTrend && (
            <span className={styles.trendInline}>
              {' '}
              ({t(`pages:credentials.trend_${c.healthTrend}`, { defaultValue: c.healthTrend })})
            </span>
          )}
        </div>
      </div>

      <div className={styles.summaryItem}>
        <div className={styles.summaryLabel}>{t('pages:credentials.rate5m')}</div>
        <div className={styles.summaryValue}>
          {rate5m != null
            ? `${(rate5m * 100).toFixed(1)}%`
            : rate1h != null
              ? <span className={styles.mutedText}>{t('pages:credentials.rate5mIdle', { defaultValue: 'idle · no traffic in last 5 min' })}</span>
              : '—'}
        </div>
      </div>

      <div className={styles.summaryItem}>
        <div className={styles.summaryLabel}>{t('pages:credentials.rate1h')}</div>
        <div className={styles.summaryValue}>{rate1h != null ? `${(rate1h * 100).toFixed(1)}%` : '—'}</div>
      </div>

      <div className={styles.summaryItem}>
        <div className={styles.summaryLabel}>{t('pages:credentials.samples')}</div>
        <div className={styles.summaryValue}>
          {samples > 0
            ? <>{samples}{samples < 5 && <> · {t('pages:credentials.collectingProgress', { observed: samples, target: 5 })}</>}</>
            : rate1h != null
              ? <span className={styles.mutedText}>{t('pages:credentials.samplesIdle', { defaultValue: '0 in last 5 min · see 1h rate above' })}</span>
              : '—'}
        </div>
      </div>

      {c.healthDominantError && c.healthDominantError !== 'none' && (
        <div className={styles.summaryItem}>
          <div className={styles.summaryLabel}>{t('pages:credentials.dominantError')}</div>
          <div className={styles.summaryValue}>{t(`pages:credentials.dominantError_${c.healthDominantError}`, { defaultValue: c.healthDominantError })}</div>
        </div>
      )}

      <div className={styles.summaryItem}>
        <div className={styles.summaryLabel}>{t('pages:credentials.circuit')}</div>
        <div className={styles.summaryValue}>
          <span className={clsx(styles.statusBadge, toneClass(c))}>
            {t(`pages:credentials.circuit_${c.circuitState ?? 'closed'}`, { defaultValue: c.circuitState ?? 'closed' })}
          </span>
          {c.circuitReason && (
            <span className={styles.mutedText}>
              {' '}· {t(`pages:credentials.circuitReason_${c.circuitReason}`, { defaultValue: c.circuitReason })}
            </span>
          )}
        </div>
      </div>

      {c.circuitOpenedAt && (
        <div className={styles.summaryItem}>
          <div className={styles.summaryLabel}>{t('pages:credentials.openedAt')}</div>
          <div className={styles.summaryValue}>{new Date(c.circuitOpenedAt).toLocaleString(i18n.language)}</div>
        </div>
      )}

      {c.circuitNextProbeAt && c.circuitReason === 'rate_limit' && (
        <div className={styles.summaryItem}>
          <div className={styles.summaryLabel}>{t('pages:credentials.nextProbeAt')}</div>
          <div className={styles.summaryValue}>{new Date(c.circuitNextProbeAt).toLocaleString(i18n.language)}</div>
        </div>
      )}

      {c.liveCircuit && c.liveCircuit.authFailsCurrent > 0 && (
        <div className={styles.summaryItem}>
          <div className={styles.summaryLabel}>{t('pages:credentials.liveAuthFails')}</div>
          <div className={styles.summaryValue}>{c.liveCircuit.authFailsCurrent}</div>
        </div>
      )}

      {c.healthCheckedAt && (
        <div className={styles.summaryItem}>
          <div className={styles.summaryLabel}>{t('pages:credentials.checkedAt')}</div>
          <div className={styles.summaryValue}>{new Date(c.healthCheckedAt).toLocaleString(i18n.language)}</div>
        </div>
      )}
    </div>
  );
}

function toneClass(c: Credential): string {
  if (c.circuitState === 'open' || c.healthStatus === 'unavailable') return styles.toneBad;
  if (c.circuitState === 'half_open' || c.healthStatus === 'degraded') return styles.toneWarn;
  if (!c.healthStatus || c.healthStatus === 'unknown' || c.healthStatus === 'collecting') return styles.toneIdle;
  return styles.toneGood;
}

function ProbeResultPanel({ result }: { result: CredentialProbeResult }) {
  const { t } = useTranslation();
  return (
    <div className={clsx(styles.probeBox, result.ok ? styles.probeOk : styles.probeFail)}>
      <div className={styles.probeHeader}>
        {result.ok ? t('pages:credentials.probeOk') : t('pages:credentials.probeFail')}
        <span className={styles.mutedText}> · {result.latencyMs} ms</span>
      </div>
      {result.detail && <div className={styles.probeDetail}>{result.detail}</div>}
      {result.error && <div className={styles.probeError}>{result.error}</div>}
      <div className={styles.probeMeta}>
        {result.providerName && <>{result.providerName} · </>}
        {result.adapterType}
      </div>
    </div>
  );
}

function ThresholdEditor({ form, setForm }: { form: OverrideForm; setForm: (f: OverrideForm) => void }) {
  const { t } = useTranslation();
  const update = (key: keyof OverrideForm) => (e: React.ChangeEvent<HTMLInputElement>) =>
    setForm({ ...form, [key]: e.target.value });

  return (
    <div className={styles.editGrid}>
      {THRESHOLD_FIELDS.map((f) => (
        <FormField key={f.key} label={t(`pages:credentials.${f.key}`)} helpText={t(`pages:credentials.${f.helper}`)}>
          <Input type="number" min="0" value={form[f.key]} onChange={update(f.key)} placeholder={t('pages:credentials.useGlobal').replace(/[（）()]/g, '')} />
        </FormField>
      ))}
    </div>
  );
}

function ThresholdDisplay({ overrides, t }: { overrides: ReliabilityThresholds | null; t: ReturnType<typeof useTranslation>['t'] }) {
  return (
    <div className={styles.thresholdGrid}>
      {THRESHOLD_FIELDS.map(({ key }) => {
        const value = overrides?.[key];
        const hasOverride = typeof value === 'number' && value > 0;
        return (
          <div className={styles.thresholdItem} key={key}>
            <div className={styles.thresholdLabel}>{t(`pages:credentials.${key}`)}</div>
            <div className={hasOverride ? styles.thresholdValue : styles.thresholdInherited}>
              {hasOverride ? value : t('pages:credentials.useGlobal').replace(/[（）()]/g, '')}
            </div>
          </div>
        );
      })}
    </div>
  );
}
