import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { systemApi, type ObservabilityConfig } from '@/api/services/infrastructure/misc/system';
import { Card, Stack, Button, Skeleton, ErrorBanner, Input, FormField, Switch } from '@/components/ui';
import styles from './SettingsObservabilityTab.module.css';

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
    <section className={styles.contentSection}>
      <h2 className={styles.sectionTitle}>{t('pages:settingsObservability.title')}</h2>
      <p className={styles.subtitle}>
        {t('pages:settingsObservability.subtitle')}
      </p>
      <Card>
        <Stack gap="md">
          <div className={styles.configGrid}>
            <div className={styles.configItem}>
              <span className={styles.itemTitle}>{t('pages:settingsObservability.otelEnabled')}</span>
              <Switch checked={otelEnabled} onCheckedChange={setOtelEnabled} />
            </div>
            <ConfigItem label={t('pages:settingsObservability.endpoint')} value={data.otelEndpoint} />
            <ConfigItem label={t('pages:settingsObservability.serviceName')} value={data.otelServiceName} />
          </div>

          <div className={styles.formGrid}>
            <FormField label={t('pages:settingsObservability.samplingRate')} className={styles.formField}>
              <Input
                type="number"
                value={samplingRate}
                onChange={e => setSamplingRate(e.target.value)}
                min={0}
                max={1}
                step={0.01}
              />
            </FormField>

            <FormField label={t('pages:settingsObservability.traceViewerUrl')} className={styles.formField}>
              <Input
                type="url"
                value={traceViewerUrl}
                onChange={e => setTraceViewerUrl(e.target.value)}
                placeholder="https://grafana.example.com/d/traces"
              />
            </FormField>
          </div>

          <Stack direction="horizontal" gap="sm">
            <Button className={styles.saveButton} onClick={() => save(undefined)} loading={saving}>
              {t('common:save')}
            </Button>
          </Stack>
        </Stack>
      </Card>
    </section>
  );
}

function ConfigItem({ label, value }: { label: string; value: string }) {
  return (
    <div className={styles.configItem}>
      <span className={styles.itemTitle}>{label}</span>
      <span className={styles.itemValue}>{value || '—'}</span>
    </div>
  );
}
