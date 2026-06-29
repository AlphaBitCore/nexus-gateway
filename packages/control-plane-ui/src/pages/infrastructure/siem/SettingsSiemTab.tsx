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
  const [collapsedServices, setCollapsedServices] = useState<Set<string>>(() => new Set());
  const [collapsedResources, setCollapsedResources] = useState<Set<string>>(() => new Set());

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

  const toggleServiceOpen = (service: string) => {
    setCollapsedServices((prev) => {
      const next = new Set(prev);
      if (next.has(service)) next.delete(service);
      else next.add(service);
      return next;
    });
  };

  const toggleResourceOpen = (service: string, resource: string) => {
    const key = `${service}|${resource}`;
    setCollapsedResources((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  return (
    <section className={styles.contentSection}>
      <h2 className={styles.sectionTitle}>{t('pages:settingsSiem.title')}</h2>
      <p className={styles.subtitle}>
        {t('pages:settingsSiem.subtitle')}
      </p>
      <Card>
        <Stack gap="md">
          <div className={styles.topGrid}>
            <div className={styles.switchBlock}>
              <span className={styles.fieldTitle}>{t('pages:settingsSiem.enabled')}</span>
              <Switch checked={form.enabled} onCheckedChange={checked => setForm({ ...form, enabled: checked })} />
            </div>

            <FormField label={t('pages:settingsSiem.url')} className={styles.formField}>
              <Input
                type="url"
                value={form.url}
                onChange={e => setForm({ ...form, url: e.target.value })}
                placeholder="https://siem.example.com/ingest"
              />
            </FormField>

            <FormField label={t('pages:settingsSiem.format')} className={styles.formField}>
            <Select
              value={form.format}
              onValueChange={value => setForm({ ...form, format: value as SiemFormat })}
              options={FORMAT_OPTIONS}
            />
          </FormField>
          </div>

          <section className={styles.formSection}>
            <div className={styles.sectionHeader}>
              <h3 className={styles.subsectionTitle}>{t('pages:settingsSiem.headers')}</h3>
              <button
                type="button"
                className={styles.addHeaderButton}
                onClick={() => setHeaderRows([...headerRows, { key: '', value: '' }])}
              >
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                  <path d="M12 5v14" />
                  <path d="M5 12h14" />
                </svg>
                {t('common:add')}
              </button>
            </div>
            <div className={styles.headerRows}>
              {headerRows.map((row, i) => (
                <div key={i} className={styles.headerRow}>
                  <Input
                    placeholder={t('pages:settingsSiem.headerNamePlaceholder')}
                    value={row.key}
                    onChange={e => {
                      const next = [...headerRows]; next[i] = { ...next[i], key: e.target.value }; setHeaderRows(next);
                    }}
                  />
                  <Input
                    placeholder={t('pages:settingsSiem.headerValuePlaceholder')}
                    value={row.value}
                    onChange={e => {
                      const next = [...headerRows]; next[i] = { ...next[i], value: e.target.value }; setHeaderRows(next);
                    }}
                  />
                  <Button
                    variant="danger"
                    className={styles.removeHeaderButton}
                    onClick={() => setHeaderRows(headerRows.filter((_, j) => j !== i))}
                  >
                    {t('common:remove')}
                  </Button>
                </div>
              ))}
            </div>
          </section>

        {/* Event Types — grouped */}
          <section className={styles.formSection}>
          <h3 className={styles.subsectionTitle}>{t('pages:settingsSiem.eventTypes')}</h3>
          <p className={styles.helpText}>
            {t('pages:settingsSiem.eventTypesHelp')}
          </p>

          {groupedEventTypes.map(({ service, resources }) => {
            const allInService = resources.flatMap(r => r.types);
            const allChecked = allInService.every(et => form.eventTypes.includes(et.type));
            const someChecked = allInService.some(et => form.eventTypes.includes(et.type));
            const serviceLabel = t(`pages:iam.services.${service}`, { defaultValue: service });
            const serviceCollapsed = collapsedServices.has(service);
            return (
              <div key={service} className={styles.eventService}>
                <div className={styles.treeHeader}>
                  <button
                    type="button"
                    className={styles.treeToggle}
                    aria-label={serviceCollapsed ? 'Expand' : 'Collapse'}
                    aria-expanded={!serviceCollapsed}
                    onClick={() => toggleServiceOpen(service)}
                  >
                    <span className={serviceCollapsed ? styles.chevronCollapsed : styles.chevronExpanded} aria-hidden="true" />
                  </button>
                  <label className={styles.serviceLabel}>
                    <Checkbox
                      checked={allChecked ? true : (someChecked ? 'indeterminate' : false)}
                      onCheckedChange={() => toggleBatch(allInService)}
                    />
                    <span>{serviceLabel}</span>
                  </label>
                </div>
                {!serviceCollapsed && (
                <div className={styles.resourceList}>
                  {resources.map(({ resource, types }) => {
                    const rAll = types.every(et => form.eventTypes.includes(et.type));
                    const rSome = types.some(et => form.eventTypes.includes(et.type));
                    const resourceKey = `${service}|${resource}`;
                    const resourceCollapsed = collapsedResources.has(resourceKey);
                    return (
                      <div key={resource} className={styles.eventResource}>
                        <div className={styles.treeHeader}>
                          <button
                            type="button"
                            className={styles.treeToggle}
                            aria-label={resourceCollapsed ? 'Expand' : 'Collapse'}
                            aria-expanded={!resourceCollapsed}
                            onClick={() => toggleResourceOpen(service, resource)}
                          >
                            <span className={resourceCollapsed ? styles.chevronCollapsed : styles.chevronExpanded} aria-hidden="true" />
                          </button>
                          <label className={styles.resourceLabel}>
                            <Checkbox
                              checked={rAll ? true : (rSome ? 'indeterminate' : false)}
                              onCheckedChange={() => toggleBatch(types)}
                            />
                            <span>{resource}</span>
                          </label>
                        </div>
                        {!resourceCollapsed && (
                        <div className={styles.typeGrid}>
                          {types.map(et => (
                            <label key={et.type} className={styles.typeLabel}>
                              <Checkbox
                                checked={form.eventTypes.includes(et.type)}
                                onCheckedChange={() => toggleEventType(et.type)}
                              />
                              <span>{et.type}</span>
                            </label>
                          ))}
                        </div>
                        )}
                      </div>
                    );
                  })}
                </div>
                )}
              </div>
            );
          })}
          </section>

        <Stack direction="horizontal" gap="sm" className={styles.footerActions}>
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
    </section>
  );
}
