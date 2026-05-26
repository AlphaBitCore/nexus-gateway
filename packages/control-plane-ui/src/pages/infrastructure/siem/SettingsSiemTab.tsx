import { useState, useEffect, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { systemApi, type SiemConfig, type SiemFormat, type SiemEventTypeInfo } from '@/api/services/infrastructure/misc/system';
import { Card, Stack, Button, Skeleton, ErrorBanner, Input, Select, FormField, Switch, Checkbox } from '@/components/ui';
import styles from './SettingsSiemTab.module.css';

const FORMAT_OPTIONS = [
  { value: 'json', label: 'JSON' },
  { value: 'cef', label: 'CEF' },
  { value: 'syslog', label: 'Syslog' },
];

export function SettingsSiemTab() {
  const { t } = useTranslation();

  const { data, loading, error, refetch } = useApi<SiemConfig>(
    () => systemApi.getSiemConfig(),
    ['admin', 'settings', 'siem'],
  );

  const { data: eventTypesData } = useApi<{ eventTypes: SiemEventTypeInfo[] }>(
    () => systemApi.listSiemEventTypes(),
    ['admin', 'settings', 'siem', 'event-types'],
  );

  const [form, setForm] = useState<SiemConfig | null>(null);
  const [headerRows, setHeaderRows] = useState<Array<{ key: string; value: string }>>([]);
  const [testResult, setTestResult] = useState<{ ok: boolean; error?: string } | null>(null);

  useEffect(() => {
    if (data) {
      setForm({ ...data, headers: data.headers ?? {}, eventTypes: data.eventTypes ?? [] });
      setHeaderRows(Object.entries(data.headers ?? {}).map(([key, value]) => ({ key, value })));
    }
  }, [data]);

  const { mutate: save, loading: saving } = useMutation(
    () => {
      if (!form) throw new Error('no form state');
      const headers: Record<string, string> = {};
      for (const r of headerRows) {
        if (r.key.trim()) headers[r.key.trim()] = r.value;
      }
      return systemApi.updateSiemConfig({ ...form, headers });
    },
    { invalidateQueries: [['admin', 'settings', 'siem']], onSuccess: () => refetch() },
  );

  const { mutate: sendTest, loading: testing } = useMutation(
    () => systemApi.sendSiemTestEvent(),
    { onSuccess: (result) => setTestResult(result) },
  );

  // Three-level grouping for the SIEM filter picker: service → resource →
  // event-type. Mirrors the IAM CatalogPicker hierarchy so operators see one
  // consistent tree shape across IAM + SIEM screens.
  const SIEM_SERVICE_ORDER = ['gateway', 'compliance', 'agent', 'platform', 'iam'] as const;
  const groupedEventTypes = useMemo(() => {
    if (!eventTypesData?.eventTypes) return [];
    // Bucket by service first.
    const byService = new Map<string, Map<string, SiemEventTypeInfo[]>>();
    for (const et of eventTypesData.eventTypes) {
      const svc = et.service || 'unknown';
      if (!byService.has(svc)) byService.set(svc, new Map());
      const byResource = byService.get(svc)!;
      const list = byResource.get(et.resource) ?? [];
      list.push(et);
      byResource.set(et.resource, list);
    }
    // Walk SIEM_SERVICE_ORDER for canonical service order; surface any
    // unknown services at the tail.
    const result: Array<{
      service: string;
      resources: Array<{ resource: string; types: SiemEventTypeInfo[] }>;
    }> = [];
    const seen = new Set<string>();
    const emit = (svc: string) => {
      const byResource = byService.get(svc);
      if (!byResource) return;
      const resources = [...byResource.entries()]
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([resource, types]) => ({ resource, types }));
      result.push({ service: svc, resources });
      seen.add(svc);
    };
    for (const s of SIEM_SERVICE_ORDER) emit(s);
    for (const s of byService.keys()) if (!seen.has(s)) emit(s);
    return result;
  }, [eventTypesData]);

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!form) return null;

  const toggleEventType = (type: string) => {
    const next = form.eventTypes.includes(type)
      ? form.eventTypes.filter(t => t !== type)
      : [...form.eventTypes, type];
    setForm({ ...form, eventTypes: next });
  };

  const toggleBatch = (types: SiemEventTypeInfo[]) => {
    const typeNames = types.map(et => et.type);
    const allChecked = typeNames.every(t => form.eventTypes.includes(t));
    if (allChecked) {
      setForm({ ...form, eventTypes: form.eventTypes.filter(t => !typeNames.includes(t)) });
    } else {
      const merged = new Set([...form.eventTypes, ...typeNames]);
      setForm({ ...form, eventTypes: [...merged] });
    }
  };

  return (
    <Card>
      <Stack gap="md">
        <h2>{t('pages:settingsSiem.title')}</h2>
        <p className={styles.subtitle}>
          {t('pages:settingsSiem.subtitle')}
        </p>

        <Stack direction="horizontal" gap="sm" style={{ alignItems: 'center' }}>
          <Switch checked={form.enabled} onCheckedChange={checked => setForm({ ...form, enabled: checked })} />
          <span style={{ fontSize: 'var(--g-font-size-base)' }}>{t('pages:settingsSiem.enabled')}</span>
        </Stack>

        <FormField label={t('pages:settingsSiem.url')}>
          <Input
            type="url"
            value={form.url}
            onChange={e => setForm({ ...form, url: e.target.value })}
            placeholder="https://siem.example.com/ingest"
          />
        </FormField>

        <div style={{ maxWidth: 100 }}>
          <FormField label={t('pages:settingsSiem.format')}>
            <Select
              value={form.format}
              onValueChange={value => setForm({ ...form, format: value as SiemFormat })}
              options={FORMAT_OPTIONS}
            />
          </FormField>
        </div>

        <div>
          <h3 style={{ marginTop: 'var(--g-space-4)', fontSize: 'var(--g-font-size-md)' }}>{t('pages:settingsSiem.headers')}</h3>
          {headerRows.map((row, i) => (
            <Stack key={i} direction="horizontal" gap="sm" style={{ alignItems: 'center' }}>
              <Input
                placeholder={t('pages:settingsSiem.headerNamePlaceholder')}
                value={row.key}
                onChange={e => {
                  const next = [...headerRows]; next[i] = { ...next[i], key: e.target.value }; setHeaderRows(next);
                }}
                style={{ flex: 1 }}
              />
              <Input
                placeholder={t('pages:settingsSiem.headerValuePlaceholder')}
                value={row.value}
                onChange={e => {
                  const next = [...headerRows]; next[i] = { ...next[i], value: e.target.value }; setHeaderRows(next);
                }}
                style={{ flex: 2 }}
              />
              <Button variant="danger" onClick={() => setHeaderRows(headerRows.filter((_, j) => j !== i))}>
                {t('common:remove')}
              </Button>
            </Stack>
          ))}
          <Button variant="secondary" onClick={() => setHeaderRows([...headerRows, { key: '', value: '' }])}>
            {t('common:add')}
          </Button>
        </div>

        {/* Event Types — grouped */}
        <div>
          <h3 style={{ marginTop: 'var(--g-space-4)', fontSize: 'var(--g-font-size-md)' }}>{t('pages:settingsSiem.eventTypes')}</h3>
          <p className={styles.helpText}>
            {t('pages:settingsSiem.eventTypesHelp')}
          </p>

          {groupedEventTypes.map(({ service, resources }) => {
            const allInService = resources.flatMap(r => r.types);
            const allChecked = allInService.every(et => form.eventTypes.includes(et.type));
            const someChecked = allInService.some(et => form.eventTypes.includes(et.type));
            const serviceLabel = t(`pages:iam.services.${service}`, { defaultValue: service });
            return (
              <div key={service} style={{ marginBottom: 'var(--g-space-4)', borderLeft: '3px solid var(--color-border-subtle)', paddingLeft: 'var(--g-space-3)' }}>
                <label style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-2)', fontSize: 'var(--g-font-size-base)', fontWeight: 'var(--g-font-weight-bold)', textTransform: 'uppercase', letterSpacing: '0.04em', marginBottom: 'var(--g-space-1)', cursor: 'pointer' }}>
                  <Checkbox
                    checked={allChecked ? true : (someChecked ? 'indeterminate' : false)}
                    onCheckedChange={() => toggleBatch(allInService)}
                  />
                  <span>{serviceLabel}</span>
                </label>
                <div style={{ paddingLeft: 'var(--g-space-4)' }}>
                  {resources.map(({ resource, types }) => {
                    const rAll = types.every(et => form.eventTypes.includes(et.type));
                    const rSome = types.some(et => form.eventTypes.includes(et.type));
                    return (
                      <div key={resource} style={{ marginBottom: 'var(--g-space-2)' }}>
                        <label style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-2)', fontSize: 'var(--g-font-size-sm)', fontWeight: 'var(--g-font-weight-semibold)', marginBottom: 'var(--g-space-0-5)', cursor: 'pointer' }}>
                          <Checkbox
                            checked={rAll ? true : (rSome ? 'indeterminate' : false)}
                            onCheckedChange={() => toggleBatch(types)}
                          />
                          <span style={{ fontFamily: 'monospace' }}>{resource}</span>
                        </label>
                        <div style={{ paddingLeft: 'var(--g-space-6)', display: 'grid', gridTemplateColumns: 'repeat(2, 1fr)', gap: 'var(--g-space-0-5)' }}>
                          {types.map(et => (
                            <label key={et.type} style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-2)', fontSize: 'var(--g-font-size-sm)', cursor: 'pointer' }}>
                              <Checkbox
                                checked={form.eventTypes.includes(et.type)}
                                onCheckedChange={() => toggleEventType(et.type)}
                              />
                              <span style={{ fontFamily: 'monospace' }}>{et.type}</span>
                            </label>
                          ))}
                        </div>
                      </div>
                    );
                  })}
                </div>
              </div>
            );
          })}
        </div>

        <Stack direction="horizontal" gap="sm">
          <Button onClick={() => save(undefined)} loading={saving}>{t('common:save')}</Button>
          <Button variant="secondary" onClick={() => sendTest(undefined)} loading={testing}>
            {t('pages:settingsSiem.testButton')}
          </Button>
        </Stack>

        {testResult && (
          <div className={testResult.ok ? styles.testResultOk : styles.testResultError}>
            {testResult.ok ? t('pages:settingsSiem.testSuccess') : `${t('pages:settingsSiem.testFailure')}: ${testResult.error}`}
          </div>
        )}
      </Stack>
    </Card>
  );
}
