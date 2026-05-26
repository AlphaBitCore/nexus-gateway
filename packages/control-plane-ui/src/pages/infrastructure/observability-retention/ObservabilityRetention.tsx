/**
 * ObservabilityRetention — admin Settings → Observability → Retention page.
 *
 * Surfaces the 11 retention layers backed by `metric_ops_retention_config`
 * (spec §5.5):
 *
 *   Operational metrics — Runtime: runtime_raw, runtime_1h, runtime_1d, runtime_1mo
 *   Operational metrics — Business: business_raw, business_1h, business_1d, business_1mo
 *   Diagnostic events: diag_warn, diag_error, diag_fatal
 *
 * The GET endpoint returns each layer's current value and `[min, max]` bounds
 * (rendered as placeholder hints next to each input). Save submits **only the
 * keys that diverged from the loaded snapshot** — minimizing the audit-log
 * footprint and avoiding accidental no-op writes that bump `updated_at`.
 *
 * Reset to defaults uses a built-in default table so the user can roll back
 * a misconfigured layer without first reading the spec; the request still
 * goes through the same PUT path so the server validates against `[min, max]`.
 */
import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { retentionApi } from '@/api/services/infrastructure/ops/retention';
import type {
  RetentionGetResponse,
  RetentionLayer,
} from '@/api/services/infrastructure/ops/retention';
import {
  PageHeader, Stack, Card, Button, Input, AlertDialog,
  LoadingSpinner, ErrorBanner, FormField,
} from '@/components/ui';
import styles from './ObservabilityRetention.module.css';

/**
 * Layer ordering + grouping. The server returns rows alphabetically, but the
 * spec calls out a deliberate UI order (runtime tier before business tier;
 * raw → 1h → 1d → 1mo within each tier; diag ordered by severity).
 */
const RUNTIME_LAYERS = ['runtime_raw', 'runtime_1h', 'runtime_1d', 'runtime_1mo'] as const;
const BUSINESS_LAYERS = ['business_raw', 'business_1h', 'business_1d', 'business_1mo'] as const;
const DIAG_LAYERS = ['diag_warn', 'diag_error', 'diag_fatal'] as const;

/** Spec §5.5 defaults — used by the "Reset to defaults" action and as the
 *  fallback when a layer is missing from the GET response. */
const DEFAULTS: Record<string, number> = {
  runtime_raw: 7,
  runtime_1h: 90,
  runtime_1d: 365,
  runtime_1mo: 1825,
  business_raw: 7,
  business_1h: 90,
  business_1d: 365,
  business_1mo: 1825,
  diag_warn: 30,
  diag_error: 180,
  diag_fatal: 365,
};

/** Spec §5.5 [min, max] bounds; mirrored client-side so the UI can disable
 *  Save before the server 400s. The server is authoritative — we never lower
 *  these bounds, only echo them. */
const BOUNDS: Record<string, { min: number; max: number }> = {
  runtime_raw: { min: 1, max: 30 },
  runtime_1h: { min: 30, max: 365 },
  runtime_1d: { min: 90, max: 1095 },
  runtime_1mo: { min: 365, max: 3650 },
  business_raw: { min: 1, max: 30 },
  business_1h: { min: 30, max: 365 },
  business_1d: { min: 90, max: 1095 },
  business_1mo: { min: 365, max: 3650 },
  diag_warn: { min: 7, max: 90 },
  diag_error: { min: 30, max: 730 },
  diag_fatal: { min: 90, max: 1825 },
};

const ALL_LAYERS = [...RUNTIME_LAYERS, ...BUSINESS_LAYERS, ...DIAG_LAYERS];

interface LayerState {
  value: string; // string so the input can hold partial / invalid edits
  initial: number;
  min: number;
  max: number;
}

/** Build the form state from the GET response. Missing layers fall back to
 *  the spec defaults so the form never renders an empty input row. */
function buildState(resp: RetentionGetResponse | null): Record<string, LayerState> {
  const out: Record<string, LayerState> = {};
  for (const key of ALL_LAYERS) {
    const layer: RetentionLayer | undefined = resp?.retention[key];
    const initial = layer?.value ?? DEFAULTS[key];
    const min = layer?.min ?? BOUNDS[key].min;
    const max = layer?.max ?? BOUNDS[key].max;
    out[key] = { value: String(initial), initial, min, max };
  }
  return out;
}

interface LayerErrors {
  [key: string]: string | undefined;
}

/** Validate one layer; returns an error string or undefined. */
function validateLayer(state: LayerState, t: (k: string, opts?: Record<string, unknown>) => string): string | undefined {
  const trimmed = state.value.trim();
  if (trimmed === '') return t('settings.observabilityRetention.errRequired');
  if (!/^\d+$/.test(trimmed)) return t('settings.observabilityRetention.errInteger');
  const n = Number(trimmed);
  if (!Number.isFinite(n) || n < state.min || n > state.max) {
    return t('settings.observabilityRetention.errRange', { min: state.min, max: state.max });
  }
  return undefined;
}

export default function ObservabilityRetention() {
  const { t } = useTranslation('pages');

  const { data, loading, error, refetch } = useApi<RetentionGetResponse>(
    () => retentionApi.get(),
    ['admin', 'observability', 'retention'],
  );

  // Form state mirrors the loaded snapshot; we re-seed it whenever the
  // backing query refreshes (after a save/reset round-trip).
  const [layers, setLayers] = useState<Record<string, LayerState>>(() => buildState(null));
  useEffect(() => {
    if (data) setLayers(buildState(data));
  }, [data]);

  const [confirmReset, setConfirmReset] = useState(false);

  // Derived: which layers diverged from the loaded value, validation errors
  // per layer, and the global "saveable?" boolean.
  const errors = useMemo<LayerErrors>(() => {
    const out: LayerErrors = {};
    for (const key of ALL_LAYERS) {
      const err = validateLayer(layers[key], t);
      if (err) out[key] = err;
    }
    return out;
  }, [layers, t]);

  const changedKeys = useMemo<string[]>(() => {
    const out: string[] = [];
    for (const key of ALL_LAYERS) {
      const s = layers[key];
      if (s.value.trim() !== String(s.initial)) out.push(key);
    }
    return out;
  }, [layers]);

  const hasErrors = Object.values(errors).some((v) => Boolean(v));
  const canSave = changedKeys.length > 0 && !hasErrors;

  // Save: PUT only diverged layers.
  const saveMutation = useMutation<Record<string, number>, unknown>(
    (body) => retentionApi.put(body),
    {
      successMessage: t('settings.observabilityRetention.saveSuccess'),
      errorMessage: t('settings.observabilityRetention.saveFailed'),
      invalidateQueries: [['admin', 'observability', 'retention']],
      onSuccess: () => refetch(),
    },
  );

  const handleSave = () => {
    if (!canSave) return;
    const body: Record<string, number> = {};
    for (const key of changedKeys) {
      body[key] = Number(layers[key].value.trim());
    }
    saveMutation.mutate(body).catch(() => undefined);
  };

  // Reset: PUT every layer back to the default. Sends the full set so the
  // server's audit log captures the operator-driven roll-back as one event.
  const resetMutation = useMutation<Record<string, number>, unknown>(
    (body) => retentionApi.put(body),
    {
      successMessage: t('settings.observabilityRetention.resetSuccess'),
      errorMessage: t('settings.observabilityRetention.resetFailed'),
      invalidateQueries: [['admin', 'observability', 'retention']],
      onSuccess: () => refetch(),
    },
  );

  const handleReset = () => {
    setConfirmReset(false);
    const body: Record<string, number> = {};
    for (const key of ALL_LAYERS) body[key] = DEFAULTS[key];
    resetMutation.mutate(body).catch(() => undefined);
  };

  // Per-layer input handler.
  const updateLayer = (key: string, val: string) => {
    setLayers((prev) => ({ ...prev, [key]: { ...prev[key], value: val } }));
  };

  if (loading && !data) return <LoadingSpinner />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('settings.observabilityRetention.title')}
        subtitle={t('settings.observabilityRetention.description')}
      />

      <Card>
        <Stack gap="md">
          <h3 className={styles.sectionTitle}>
            {t('settings.observabilityRetention.runtimeGroup')}
          </h3>
          <LayerSection keys={RUNTIME_LAYERS} layers={layers} errors={errors} onChange={updateLayer} />
        </Stack>
      </Card>

      <Card>
        <Stack gap="md">
          <h3 className={styles.sectionTitle}>
            {t('settings.observabilityRetention.businessGroup')}
          </h3>
          <LayerSection keys={BUSINESS_LAYERS} layers={layers} errors={errors} onChange={updateLayer} />
        </Stack>
      </Card>

      <Card>
        <Stack gap="md">
          <h3 className={styles.sectionTitle}>
            {t('settings.observabilityRetention.diagGroup')}
          </h3>
          <LayerSection keys={DIAG_LAYERS} layers={layers} errors={errors} onChange={updateLayer} />
        </Stack>
      </Card>

      <div className={styles.actions}>
        <Button
          type="button"
          variant="primary"
          size="md"
          disabled={!canSave}
          loading={saveMutation.loading}
          onClick={handleSave}
        >
          {t('settings.observabilityRetention.save')}
        </Button>
        <Button
          type="button"
          variant="secondary"
          size="md"
          loading={resetMutation.loading}
          onClick={() => setConfirmReset(true)}
        >
          {t('settings.observabilityRetention.reset')}
        </Button>
      </div>

      <AlertDialog
        open={confirmReset}
        onOpenChange={setConfirmReset}
        title={t('settings.observabilityRetention.resetConfirmTitle')}
        description={t('settings.observabilityRetention.resetConfirmDesc')}
        confirmLabel={t('settings.observabilityRetention.reset')}
        cancelLabel={t('settings.observabilityRetention.cancel')}
        onConfirm={handleReset}
        variant="default"
      />
    </Stack>
  );
}

/** One group of inputs (Runtime / Business / Diag). Renders each layer with
 *  its label, value input, and inline range/error hint. */
function LayerSection({
  keys,
  layers,
  errors,
  onChange,
}: {
  keys: readonly string[];
  layers: Record<string, LayerState>;
  errors: LayerErrors;
  onChange: (key: string, val: string) => void;
}) {
  const { t } = useTranslation('pages');
  return (
    <div className={styles.layerGrid}>
      {keys.map((key) => {
        const state = layers[key];
        const err = errors[key];
        const isDirty = state.value.trim() !== String(state.initial);
        return (
          <FormField
            key={key}
            label={key}
            helpText={t('settings.observabilityRetention.rangeHint', {
              min: state.min,
              max: state.max,
              default: DEFAULTS[key],
            })}
            error={err}
          >
            <Input
              type="number"
              inputMode="numeric"
              value={state.value}
              onChange={(e) => onChange(key, e.target.value)}
              min={state.min}
              max={state.max}
              data-testid={`layer-${key}`}
              aria-label={key}
              data-dirty={isDirty || undefined}
            />
          </FormField>
        );
      })}
    </div>
  );
}
