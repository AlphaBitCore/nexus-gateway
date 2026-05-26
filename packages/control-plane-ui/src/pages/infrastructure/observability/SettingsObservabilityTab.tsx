import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { systemApi, type ObservabilityConfig } from '@/api/services/infrastructure/misc/system';
import { Card, Stack, Button, Skeleton, ErrorBanner, Input, FormField, Switch } from '@/components/ui';

export function SettingsObservabilityTab() {
  const { t } = useTranslation();
  const [otelEnabled, setOtelEnabled] = useState(false);
  const [samplingRate, setSamplingRate] = useState('0');
  const [traceViewerUrl, setTraceViewerUrl] = useState('');

  const { data, loading, error, refetch } = useApi<ObservabilityConfig>(
    () => systemApi.getObservabilityConfig(),
    ['admin', 'settings', 'observability'],
  );

  useEffect(() => {
    if (data) {
      setOtelEnabled(data.otelEnabled ?? false);
      setSamplingRate(String(data.samplingRate ?? 0));
      setTraceViewerUrl(data.traceViewerUrl ?? '');
    }
  }, [data]);

  const { mutate: save, loading: saving } = useMutation(
    () => systemApi.updateObservabilityConfig({
      otelEnabled,
      samplingRate: parseFloat(samplingRate) || 0,
      traceViewerUrl,
    }),
    {
      invalidateQueries: [['admin', 'settings', 'observability']],
      onSuccess: () => refetch(),
    },
  );

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!data) return null;

  return (
    <Card>
      <Stack gap="md">
        <h2>{t('pages:settingsObservability.title')}</h2>
        <p style={{ fontSize: 'var(--g-font-size-base)', color: 'var(--color-text-secondary)' }}>
          {t('pages:settingsObservability.subtitle')}
        </p>

        <Stack direction="horizontal" gap="sm" style={{ alignItems: 'center' }}>
          <Switch checked={otelEnabled} onCheckedChange={setOtelEnabled} />
          <span style={{ fontSize: 'var(--g-font-size-base)' }}>{t('pages:settingsObservability.otelEnabled')}</span>
        </Stack>

        <h3 style={{ marginTop: 'var(--g-space-4)' }}>{t('pages:settingsObservability.currentConfig')}</h3>
        <Stack gap="sm">
          <Row label={t('pages:settingsObservability.endpoint')} value={data.otelEndpoint} />
          <Row label={t('pages:settingsObservability.serviceName')} value={data.otelServiceName} />
        </Stack>

        <div style={{ maxWidth: 200 }}>
          <FormField label={t('pages:settingsObservability.samplingRate')}>
            <Input
              type="number"
              value={samplingRate}
              onChange={e => setSamplingRate(e.target.value)}
              min={0}
              max={1}
              step={0.01}
            />
          </FormField>
        </div>

        <FormField label={t('pages:settingsObservability.traceViewerUrl')}>
          <Input
            type="url"
            value={traceViewerUrl}
            onChange={e => setTraceViewerUrl(e.target.value)}
            placeholder="https://grafana.example.com/d/traces"
          />
        </FormField>

        <Stack direction="horizontal" gap="sm">
          <Button onClick={() => save(undefined)} loading={saving}>
            {t('common:save')}
          </Button>
        </Stack>
      </Stack>
    </Card>
  );
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div style={{ display: 'flex', gap: 'var(--g-space-3)' }}>
      <span style={{ minWidth: 160, fontSize: 'var(--g-font-size-base)', color: 'var(--color-text-secondary)' }}>
        {label}
      </span>
      <span style={{ fontSize: 'var(--g-font-size-base)', fontFamily: 'monospace' }}>{value}</span>
    </div>
  );
}
