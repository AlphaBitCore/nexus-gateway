import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { cacheApi, type CacheAdapterConfig, familyOf } from '@/api/services/system/cache';
import { Button, Card, ErrorBanner, FormField, Input, Skeleton, Stack, Switch } from '@/components/ui';
import styles from './SettingsCacheTab.module.css';

/** Adapter tabs for the Tier-2 knob panel. Only families with cache-specific knobs. */
const ADAPTER_TABS: { type: string; labelKey: string }[] = [
  { type: 'anthropic', labelKey: 'pages:settings.promptCache.adapterAnthropic' },
  { type: 'bedrock', labelKey: 'pages:settings.promptCache.adapterBedrock' },
  { type: 'gemini', labelKey: 'pages:settings.promptCache.adapterGemini' },
  { type: 'vertex', labelKey: 'pages:settings.promptCache.adapterVertex' },
];

export function AdapterPanel() {
  const { t } = useTranslation();
  const [activeAdapter, setActiveAdapter] = useState<string>('anthropic');

  return (
    <Card>
      <Stack gap="md">
        <h2 style={{ margin: 'var(--g-space-0)' }}>{t('pages:settings.promptCache.adapterTitle')}</h2>
        <p className={styles.subtitle}>{t('pages:settings.promptCache.adapterSubtitle')}</p>

        <Stack direction="horizontal" gap="sm">
          {ADAPTER_TABS.map(tab => (
            <Button
              key={tab.type}
              onClick={() => setActiveAdapter(tab.type)}
              variant={activeAdapter === tab.type ? 'primary' : 'secondary'}
            >
              {t(tab.labelKey)}
            </Button>
          ))}
        </Stack>

        <AdapterConfigCard adapterType={activeAdapter} />
      </Stack>
    </Card>
  );
}

function AdapterConfigCard({ adapterType }: { adapterType: string }) {
  const { t } = useTranslation();
  const family = familyOf(adapterType);

  const { data, loading, error, refetch } = useApi<CacheAdapterConfig>(
    () => cacheApi.getAdapter(adapterType),
    ['admin', 'cache', 'adapter', adapterType],
  );

  // Local draft of changes the user has made since the last save. NULL means
  // "no edits — display values come from `data`". This pattern avoids the
  // race where useState({}) + useEffect left a single render
  // where cfg was empty before data populated, and a fast Save click during
  // that gap wiped the row.
  const [draft, setDraft] = useState<Partial<CacheAdapterConfig> | null>(null);
  useEffect(() => { setDraft(null); }, [adapterType]);

  const cfg: CacheAdapterConfig = { ...(data ?? {}), ...(draft ?? {}) };

  const set = <K extends keyof CacheAdapterConfig>(key: K, value: CacheAdapterConfig[K]) =>
    setDraft(prev => ({ ...(prev ?? {}), [key]: value }));

  const { mutate: save, loading: saving } = useMutation(
    // Always send the merged body (data + draft); preserve any rules row
    // owned by NormalisationRulesPanel so saving knobs here does not wipe
    // them.
    () => cacheApi.putAdapter(adapterType, { ...cfg, rules: data?.rules }),
    {
      invalidateQueries: [['admin', 'cache', 'adapter', adapterType], ['admin', 'cache', 'adapters', 'list']],
      onSuccess: () => { setDraft(null); refetch(); },
      successMessage: t('pages:settings.promptCache.saved'),
      errorMessage: t('pages:settings.promptCache.saveError'),
    },
  );

  // Block rendering until data has loaded, so the Save button is never active
  // against an empty body. Without this, a fast user click during the data
  // fetch could PUT an empty config and wipe the adapter row.
  if (loading || !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  return (
    <Card>
      <Stack gap="md">
        <h3 style={{ margin: 'var(--g-space-0)' }}>{t('pages:settings.promptCache.adapterDefaultsTitle', { adapter: adapterType })}</h3>

        {family === 'anthropic' && (
          <>
            <FormField label={t('pages:providers.cacheInjectEnabled')} helpText={t('pages:providers.cacheInjectEnabledHint')}>
              <Switch checked={!!cfg.marker_inject_enabled} onCheckedChange={v => set('marker_inject_enabled', v)} />
            </FormField>
            <FormField label={t('pages:providers.cacheBoundary3Enabled')} helpText={t('pages:providers.cacheBoundary3EnabledHint')}>
              <Switch
                checked={!!cfg.marker_boundary3_enabled}
                onCheckedChange={v => set('marker_boundary3_enabled', v)}
                disabled={!cfg.marker_inject_enabled}
              />
            </FormField>
          </>
        )}

        {family === 'gemini' && (
          <>
            <FormField label={t('pages:providers.cacheGeminiEnabled')} helpText={t('pages:providers.cacheGeminiEnabledHint')}>
              <Switch checked={!!cfg.cache_enabled} onCheckedChange={v => set('cache_enabled', v)} />
            </FormField>
            <NumberField
              label={t('pages:providers.cacheGeminiMinSystemChars')}
              helpText={t('pages:providers.cacheGeminiMinSystemCharsHint')}
              value={cfg.min_system_chars ?? 4096}
              onChange={v => set('min_system_chars', v)}
              disabled={!cfg.cache_enabled}
            />
            <NumberField
              label={t('pages:providers.cacheGeminiTtlSeconds')}
              helpText={t('pages:providers.cacheGeminiTtlSecondsHint')}
              value={cfg.ttl_seconds ?? 3600}
              onChange={v => set('ttl_seconds', v)}
              disabled={!cfg.cache_enabled}
            />
            <NumberField
              label={t('pages:providers.cacheGeminiCircuitThreshold')}
              helpText={t('pages:providers.cacheGeminiCircuitThresholdHint')}
              value={cfg.circuit_breaker_threshold ?? 5}
              onChange={v => set('circuit_breaker_threshold', v)}
              disabled={!cfg.cache_enabled}
            />
            <NumberField
              label={t('pages:providers.cacheGeminiCircuitOpenSecs')}
              helpText={t('pages:providers.cacheGeminiCircuitOpenSecsHint')}
              value={cfg.circuit_breaker_open_secs ?? 300}
              onChange={v => set('circuit_breaker_open_secs', v)}
              disabled={!cfg.cache_enabled}
            />
          </>
        )}

        <Stack direction="horizontal" gap="sm">
          <Button onClick={() => save(undefined)} disabled={saving}>
            {saving ? t('common:saving') : t('common:save')}
          </Button>
        </Stack>
      </Stack>
    </Card>
  );
}

function NumberField({
  label, helpText, value, onChange, disabled,
}: {
  label: string;
  helpText: string;
  value: number;
  onChange: (v: number) => void;
  disabled?: boolean;
}) {
  return (
    <FormField label={label} helpText={helpText}>
      <Input
        type="number"
        min={0}
        value={value}
        disabled={disabled}
        onChange={e => onChange(Number(e.target.value))}
      />
    </FormField>
  );
}
