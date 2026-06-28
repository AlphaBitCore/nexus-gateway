import { useState, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { iamApi, virtualKeyApi } from '@/api/services';
import { useApi } from '../../../hooks/useApi';
import { useMutation } from '../../../hooks/useMutation';
import {
  PageHeader, Card, Button, FormField, Select, Tooltip, Stack,
} from '@/components/ui';
import type { IamSimulationResponse, AdminApiKey, AdminUser, VirtualKey } from '../../../api/types';
import type { ActionCatalogResponse } from '@/api/services';
import { ADMIN_LIST_FULL_PAGE_PARAMS } from '../../../constants/admin-api';
import styles from './IamSimulator.module.css';

/* ── Resource & Action definitions ────────────────────────────────────── */
// Dropdowns fetch the canonical taxonomy from /api/admin/iam/action-catalog
// so they stay in sync with the shared/iam.Catalog the engine evaluates
// against. The single static row — the `admin:*` wildcard — is appended
// client-side because it has no concrete resource entry in the catalog.

const WILDCARD_OPTION = {
  type: 'all (wildcard)',
  service: '*',
  nrn: 'nrn:nexus:*:*:*/*',
  actions: ['admin:*'],
};

// Service display order — must match CatalogPicker + catalog_data.go.
const SERVICE_ORDER = ['gateway', 'compliance', 'agent', 'platform', 'iam'] as const;

/* ── Component ────────────────────────────────────────────────────────── */

export function IamSimulator() {
  const { t } = useTranslation();
  const [principalType, setPrincipalType] = useState('api_key');
  const [principalId, setPrincipalId] = useState('');
  const [service, setService] = useState('');
  const [resource, setResource] = useState('');
  const [action, setAction] = useState('');
  const [result, setResult] = useState<IamSimulationResponse | null>(null);
  const [simulationError, setSimulationError] = useState<string | null>(null);

  // Fetch principals for dropdown
  const { data: apiKeysData } = useApi<{ data: AdminApiKey[] }>(
    () => iamApi.listApiKeys(),
    ['admin', 'iam', 'api-keys', 'list', 'simulator'],
  );
  const { data: vkData } = useApi<{ data: VirtualKey[] }>(
    () => virtualKeyApi.list({ ...ADMIN_LIST_FULL_PAGE_PARAMS }),
    ['admin', 'virtual-keys', 'list', 'simulator'],
  );
  const { data: usersData } = useApi<{ data: AdminUser[]; total: number }>(
    () => iamApi.listUsers({ ...ADMIN_LIST_FULL_PAGE_PARAMS }),
    ['admin', 'iam', 'users', 'list', 'simulator'],
  );

  // Fetch the canonical resource × verb catalog from CP. The dropdowns
  // below render exclusively from this — no hardcoded resource or action
  // lists in this file. Append the admin:* wildcard locally; it has no
  // first-class catalog row but is a useful test target.
  const { data: catalogResp } = useApi<ActionCatalogResponse>(
    () => iamApi.getActionCatalog(),
    ['admin', 'iam', 'action-catalog', 'simulator'],
  );
  const resourceDefs = useMemo(() => {
    const fromCatalog = (catalogResp?.resources ?? []).map(r => ({
      type: r.type,
      service: r.service,
      nrn: r.nrn,
      actions: r.actions.map(a => a.name),
    }));
    return [...fromCatalog, WILDCARD_OPTION];
  }, [catalogResp]);

  // Distinct services present in the catalog, ordered canonically.
  // The wildcard option ('*') always trails so the user can pick it as
  // a cross-service evaluation target.
  const serviceOptions = useMemo(() => {
    const present = new Set(resourceDefs.map(r => r.service));
    const ordered: string[] = [];
    for (const s of SERVICE_ORDER) if (present.has(s)) ordered.push(s);
    if (present.has('*')) ordered.push('*');
    return ordered;
  }, [resourceDefs]);

  // Resources visible in the second dropdown — filtered to the selected
  // service. When no service is picked yet, the dropdown stays empty.
  const resourceOptions = useMemo(() => {
    if (!service) return [];
    return resourceDefs.filter(r => r.service === service);
  }, [resourceDefs, service]);

  // Build principal options based on selected type
  const principalOptions = useMemo(() => {
    if (principalType === 'api_key') {
      return (apiKeysData?.data ?? []).map(k => ({ value: k.id, label: `${k.name} (${k.keyPrefix}...)` }));
    }
    if (principalType === 'virtual_key') {
      return (vkData?.data ?? []).map(vk => ({ value: vk.id, label: vk.name }));
    }
    if (principalType === 'nexus_user') {
      return (usersData?.data ?? []).map(u => ({ value: u.id, label: `${u.displayName}${u.roles?.length ? ` (${u.roles.join(', ')})` : ''}` }));
    }
    return [];
  }, [principalType, apiKeysData, vkData, usersData]);

  // Actions filtered by selected resource
  const selectedResource = resourceDefs.find(r => r.nrn === resource);
  const actionOptions = useMemo(() => {
    if (!selectedResource) return [];
    return selectedResource.actions.map(a => ({ value: a, label: a }));
  }, [selectedResource]);

  const { mutate: simulate, loading } = useMutation(
    () =>
      iamApi.simulate({
        principal: { type: principalType, id: principalId },
        action,
        resource,
      }) as Promise<IamSimulationResponse>,
    {
      onSuccess: (data) => { setResult(data); setSimulationError(null); },
      successMessage: t('pages:iam.simulationCompleted'),
      errorMessage: t('pages:iam.simulationFailed'),
    },
  );

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setResult(null);
    setSimulationError(null);
    try {
      await simulate(undefined as never);
    } catch (err) {
      setResult(null);
      setSimulationError(err instanceof Error ? err.message : t('pages:iam.simulationFailed'));
    }
  };

  const canSubmit = principalType && principalId && action && resource;

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:iam.simulator')}
        subtitle={t('pages:iam.simulatorSubtitle')}
        subtitleClassName={styles.pageSubtitle}
      />
      <div className={styles.grid}>
        {/* Form */}
        <Card>
          <h3 className={styles.widgetTitle}>{t('pages:iam.simulationInput')}</h3>
          <form onSubmit={handleSubmit} className={styles.form}>
            {/* Principal Type */}
            <FormField label={t('pages:iam.principalTypeLabel')} required>
              <Select
                value={principalType}
                onValueChange={(v) => { setPrincipalType(v); setPrincipalId(''); }}
                options={[
                  { value: 'api_key', label: t('pages:iam.adminApiKey') },
                  { value: 'virtual_key', label: t('pages:iam.virtualKeyType') },
                  { value: 'nexus_user', label: t('pages:iam.userType') },
                ]}
              />
            </FormField>

            {/* Principal ID — dropdown */}
            <div>
              <div className={styles.labelRow}>
                <label className={styles.fieldLabel} htmlFor="sim-principal-id">{t('pages:iam.principalIdLabel')}</label>
                <Tooltip content={t('pages:iam.principalIdTooltip')}>
                  <button type="button" aria-label={t('pages:iam.ariaHelpPrincipalId')} className={styles.helpIconBtn}>&#9432;</button>
                </Tooltip>
              </div>
              <select id="sim-principal-id" value={principalId} onChange={e => setPrincipalId(e.target.value)} className={styles.filterSelect} required>
                <option value="">{principalType === 'api_key' ? t('pages:iam.selectAdminApiKey') : principalType === 'nexus_user' ? t('pages:iam.selectAUser') : t('pages:iam.selectVirtualKey')}</option>
                {principalOptions.map(o => (
                  <option key={o.value} value={o.value}>{o.label}</option>
                ))}
              </select>
            </div>

            {/* Service — drill level 1 of the catalog three-level hierarchy */}
            <div>
              <div className={styles.labelRow}>
                <label className={styles.fieldLabel} htmlFor="sim-service">{t('pages:iam.serviceLabel')}</label>
                <Tooltip content={t('pages:iam.serviceTooltip')}>
                  <button type="button" aria-label={t('pages:iam.ariaHelpService')} className={styles.helpIconBtn}>&#9432;</button>
                </Tooltip>
              </div>
              <select
                id="sim-service"
                value={service}
                onChange={e => { setService(e.target.value); setResource(''); setAction(''); }}
                className={styles.filterSelect}
                required
              >
                <option value="">{t('pages:iam.selectService')}</option>
                {serviceOptions.map(s => (
                  <option key={s} value={s}>
                    {s === '*'
                      ? t('pages:iam.serviceWildcard', { defaultValue: 'Any (wildcard)' })
                      : t(`pages:iam.services.${s}`, { defaultValue: s })}
                  </option>
                ))}
              </select>
            </div>

            {/* Resource — drill level 2, filtered by service */}
            <div>
              <div className={styles.labelRow}>
                <label className={styles.fieldLabel} htmlFor="sim-resource">{t('pages:iam.resourceLabel')}</label>
                <Tooltip content={t('pages:iam.resourceTooltip')}>
                  <button type="button" aria-label={t('pages:iam.ariaHelpResource')} className={styles.helpIconBtn}>&#9432;</button>
                </Tooltip>
              </div>
              <select
                id="sim-resource"
                value={resource}
                onChange={e => { setResource(e.target.value); setAction(''); }}
                className={styles.filterSelect}
                disabled={!service}
                required
              >
                <option value="">{service ? t('pages:iam.selectResourceType') : t('pages:iam.selectServiceFirst')}</option>
                {resourceOptions.map(r => (
                  <option key={r.nrn} value={r.nrn}>{r.type} — {r.nrn}</option>
                ))}
              </select>
            </div>

            {/* Action — filtered by resource */}
            <div>
              <div className={styles.labelRow}>
                <label className={styles.fieldLabel} htmlFor="sim-action">{t('pages:iam.actionLabel')}</label>
                <Tooltip content={t('pages:iam.actionTooltip')}>
                  <button type="button" aria-label={t('pages:iam.ariaHelpAction')} className={styles.helpIconBtn}>&#9432;</button>
                </Tooltip>
              </div>
              <select
                id="sim-action"
                value={action}
                onChange={e => setAction(e.target.value)}
                className={styles.filterSelect}
                disabled={!resource}
                required
              >
                <option value="">{resource ? t('pages:iam.selectAction') : t('pages:iam.selectResourceFirst')}</option>
                {actionOptions.map(o => (
                  <option key={o.value} value={o.value}>{o.label}</option>
                ))}
              </select>
            </div>

            <Button
              type="submit"
              loading={loading}
              disabled={!canSubmit}
              aria-label={t('pages:iam.ariaRunSimulation')}
            >
              {t('pages:iam.simulate')}
            </Button>
          </form>
        </Card>

        {/* Result */}
        <Card className={styles.resultCard} aria-label={t('pages:iam.ariaSimulationResult')} aria-live="polite">
          <div className={styles.resultHeader}>
            <div className={styles.resultHeaderTitleWrap}>
              <h3 className={styles.resultHeaderTitle}>{t('pages:iam.result')}</h3>
              <Tooltip content={t('pages:iam.resultTooltip')}>
                <button type="button" aria-label={t('pages:iam.ariaHelpSimulationResult')} className={styles.helpIconBtn}>&#9432;</button>
              </Tooltip>
            </div>
            {result && (
              <span className={result.decision === 'Allow' ? styles.decisionAllow : styles.decisionDeny}>
                {result.decision}
              </span>
            )}
          </div>

          {simulationError && (
            <div className={styles.errorBox}>{simulationError}</div>
          )}

          {!result && !simulationError ? (
            <div className={styles.emptyResult}>{t('pages:iam.runSimulation')}</div>
          ) : result ? (
            <div className={styles.resultColumn}>
              <p className={styles.reason}>{result.reason}</p>

              {/* Backend returns `null` for matchedStatements when no
                  policy matched (Deny by absence). Guard with ?? [] so
                  the page renders the verdict cleanly without trying to
                  enumerate a null list. */}
              {(result.matchedStatements ?? []).length > 0 && (
                <div>
                  <h4 className={styles.matchedTitle}>{t('pages:iam.matchedStatements')}</h4>
                  <div className={styles.matchedTable}>
                    <table className={styles.matchedTableInner}>
                      <thead>
                        <tr>
                          <th className={styles.th}>{t('pages:iam.colPolicy')}</th>
                          <th className={styles.th}>{t('pages:iam.colSid')}</th>
                          <th className={styles.th}>{t('pages:iam.colEffect')}</th>
                          <th className={styles.th}>{t('pages:iam.colSource')}</th>
                        </tr>
                      </thead>
                      <tbody>
                        {(result.matchedStatements ?? []).map((s, i) => (
                          <tr key={i} className={styles.simTableRow}>
                            <td className={styles.td}>{s.policyName}</td>
                            <td className={styles.tdMono}>{s.sid ?? '-'}</td>
                            <td className={styles.td}>
                              <span className={s.effect === 'Allow' ? styles.effectAllow : styles.effectDeny}>
                                {s.effect}
                              </span>
                            </td>
                            <td className={`${styles.td} ${styles.simSourceCol}`}>
                              {s.source === 'direct' ? t('pages:iam.sourceDirect') : t('pages:iam.sourceGroup', { name: s.groupName ?? t('pages:iam.unknown') })}
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                </div>
              )}
            </div>
          ) : null}
        </Card>
      </div>
    </Stack>
  );
}
