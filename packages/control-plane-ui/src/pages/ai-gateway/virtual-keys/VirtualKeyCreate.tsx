import { useState, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { z } from 'zod';
import { useApi } from '@/hooks/useApi';
import { virtualKeyApi, projectApi, systemApi } from '@/api/services';
import type { CreateVirtualKeyInput } from '@/api/services';
import { useMutation } from '@/hooks/useMutation';
import { useZodForm, FormInput, FormSwitch } from '@/lib/forms';
import { useUnsavedChangesWarning } from '@/hooks/useUnsavedChangesWarning';
import {
  PageHeader, Breadcrumb, Button, Stack, Card, Tooltip, Input,
} from '@/components/ui';
import type { AdminModelsByProvider, Project, VirtualKeyAllowedModelRef } from '@/api/types';
import { ADMIN_LIST_FULL_PAGE_PARAMS } from '@/constants/admin-api';
import styles from './VirtualKeyCreate.module.css';

/* ── Grouped Model Selector ───────────────────────────────────────────── */

function isRefSelected(selected: VirtualKeyAllowedModelRef[], providerId: string, modelId: string) {
  return selected.some(s => s.providerId === providerId && s.modelId === modelId);
}

function GroupedModelSelect({
  groups,
  selected,
  onChange,
}: {
  groups: AdminModelsByProvider[];
  selected: VirtualKeyAllowedModelRef[];
  onChange: (refs: VirtualKeyAllowedModelRef[]) => void;
}) {
  const { t } = useTranslation();
  const [modelSearch, setModelSearch] = useState('');
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>(() => {
    if (groups.length > 5) {
      const map: Record<string, boolean> = {};
      for (const g of groups) map[g.provider?.id] = true;
      return map;
    }
    return {};
  });

  const allRefs = useMemo(
    () => groups.flatMap(g => g?.models?.map(m => ({ providerId: g.provider?.id, modelId: m.id }))),
    [groups],
  );
  const q = modelSearch.toLowerCase();

  const filteredGroups = useMemo(() => {
    if (!q) return groups;
    return groups
      .map(g => ({
        ...g,
        models: g?.models?.filter(m => m.name.toLowerCase().includes(q) || m.code.toLowerCase().includes(q)),
      }))
      .filter(g => g?.models?.length > 0);
  }, [groups, q]);

  const handleSelectAll = () => onChange([...allRefs]);
  const handleDeselectAll = () => onChange([]);
  const toggleCollapse = (providerId: string) =>
    setCollapsed(prev => ({ ...prev, [providerId]: !prev[providerId] }));

  return (
    <div className={styles.modelAccessWrapper}>
      <label className={styles.modelAccessLabel}>
        {t('pages:virtualKeys.modelAccess')}
        <Tooltip content={t('pages:virtualKeys.modelAccessTooltip')}>
          <span role="presentation">&#9432;</span>
        </Tooltip>
      </label>
      <Stack direction="horizontal" gap="xs" className={styles.modelSearchRow}>
        <Input
          placeholder={t('pages:virtualKeys.searchModels')}
          value={modelSearch}
          onChange={e => setModelSearch(e.target.value)}
          className={styles.modelSearchInput}
        />
        <button type="button" onClick={handleSelectAll} className={styles.modelSelectAllBtn}>
          {t('pages:virtualKeys.selectAll')}
        </button>
        <button type="button" onClick={handleDeselectAll} className={styles.modelSelectAllBtn}>
          {t('pages:virtualKeys.deselectAll')}
        </button>
      </Stack>
      <div className={styles.modelListContainer}>
        {filteredGroups.length === 0 ? (
          <div className={styles.emptyModelHint}>
            {groups.length === 0 ? t('pages:virtualKeys.noModelsAvailable') : t('pages:virtualKeys.noMatchingModels')}
          </div>
        ) : (
          filteredGroups.map(group => {
            const isCollapsed = collapsed[group.provider?.id] && !q;
            return (
              <div key={group.provider?.id} className={styles.providerGroupWrapper}>
                <div
                  onClick={() => toggleCollapse(group.provider?.id)}
                  className={styles.providerHeader}
                >
                  <span className={isCollapsed ? styles.collapseArrowClosed : styles.collapseArrowOpen}>&#9660;</span>
                  {group.provider?.displayName || group.provider?.name}
                  <span className={styles.providerCounter}>
                    ({group?.models?.filter(m => isRefSelected(selected, group.provider?.id, m.id)).length}/{group?.models?.length})
                  </span>
                </div>
                {!isCollapsed && group?.models?.map(m => (
                  <label key={m.id} className={styles.modelLabel}>
                    <input
                      type="checkbox"
                      checked={isRefSelected(selected, group.provider?.id, m.id)}
                      onChange={e => {
                        if (e.target.checked) onChange([...selected, { providerId: group.provider?.id, modelId: m.id }]);
                        else onChange(selected.filter(s => !(s.providerId === group.provider?.id && s.modelId === m.id)));
                      }}
                    />
                    {m.name}
                    <span className={styles.modelIdHint}>({m.code})</span>
                  </label>
                ))}
              </div>
            );
          })
        )}
      </div>
      <div className={styles.modelAccessSummary}>
        {selected.length === 0 ? t('pages:virtualKeys.allModelsAllowed') : t('pages:virtualKeys.modelsSelected', { count: selected.length })}
      </div>
    </div>
  );
}

/* ── Schema ─────────────────────────────────────────────────────────────── */

const schema = z.object({
  name: z.string().min(1, 'Name is required'),
  projectId: z.string().optional().default(''),
  sourceApp: z.string().optional().default(''),
  enabled: z.boolean(),
  rateLimitRpm: z.string().optional().default(''),
  expiresAt: z.string().optional().default(''),
  neverExpires: z.boolean(),
});

type FormValues = z.infer<typeof schema>;

/* ── Component ──────────────────────────────────────────────────────────── */

export function VirtualKeyCreate() {
  const { t } = useTranslation();
  const navigate = useNavigate();

  const form = useZodForm({
    schema,
    defaultValues: {
      name: '',
      projectId: '',
      sourceApp: '',
      enabled: true,
      rateLimitRpm: '',
      expiresAt: '',
      neverExpires: true,
    },
  });

  useUnsavedChangesWarning(form.formState.isDirty);

  const [selectedModels, setSelectedModels] = useState<VirtualKeyAllowedModelRef[]>([]);

  // After creation state
  const [createdKey, setCreatedKey] = useState<string | null>(null);
  const [createdId, setCreatedId] = useState<string | null>(null);
  const [keyCopied, setKeyCopied] = useState(false);

  const { data: modelsData } = useApi<{ data: AdminModelsByProvider[] }>(
    () => systemApi.listModels(),
    ['admin', 'models', 'by-provider'],
  );
  const { data: projectsData } = useApi<{ data: Project[] }>(
    () => projectApi.list({ ...ADMIN_LIST_FULL_PAGE_PARAMS }),
    ['admin', 'projects', 'list', 'vk-create'],
  );

  const { mutate, loading } = useMutation(
    (data: CreateVirtualKeyInput) => virtualKeyApi.create(data) as Promise<{ key?: string; id?: string }>,
    {
      invalidateQueries: [['api', 'admin', 'virtual-keys']],
      onSuccess: (result: { key?: string; id?: string }) => {
        setCreatedKey(result.key ?? '');
        setCreatedId(result.id ?? '');
      },
      successMessage: t('pages:virtualKeys.virtualKeyCreatedMsg'),
    },
  );

  const neverExpires = form.watch('neverExpires');

  const onSubmit = (values: FormValues) => {
    // <Input type="date"> produces "YYYY-MM-DD" while the backend
    // unmarshals into time.Time and demands RFC3339. Stamp end-of-day
    // UTC so "expires on May 2" remains usable through that calendar
    // day; without this the bind fails with "Invalid request body".
    const expiresAt = values.neverExpires || !values.expiresAt
      ? undefined
      : `${values.expiresAt}T23:59:59Z`;

    mutate({
      name: values.name,
      projectId: values.projectId || undefined,
      sourceApp: values.sourceApp || undefined,
      enabled: values.enabled,
      rateLimitRpm: values.rateLimitRpm ? Number(values.rateLimitRpm) : undefined,
      allowedModels: selectedModels.length > 0 ? selectedModels : undefined,
      expiresAt,
      // /access/virtual-keys is the application-VK admin surface;
      // personal VKs live under /account?tab=virtualKeys. Without this
      // explicit type the server defaults to 'personal' and the new
      // row vanishes from the list (which filters vkType='application').
      vkType: 'application',
    });
  };

  // After creation -- show the generated key
  if (createdKey !== null) {
    return (
      <Stack gap="lg">
        <PageHeader title={t('pages:virtualKeys.virtualKeyCreated')} subtitle={t('pages:virtualKeys.saveSecretKeyBelow')} />
        <Card className={styles.successCard}>
          <Stack gap="md">
            <div className={styles.successTitle}>
              {t('pages:virtualKeys.virtualKeyCreatedSuccess')}
            </div>
            <div className={styles.secretKeyLabel}>{t('pages:virtualKeys.secretKey')}</div>
            <div className={styles.secretKeyContainer}>
              <span>{createdKey}</span>
              <button
                onClick={() => { navigator.clipboard.writeText(createdKey); setKeyCopied(true); setTimeout(() => setKeyCopied(false), 2000); }}
                className={styles.outlineBtn}
              >
                {keyCopied ? t('pages:virtualKeys.copied') : t('pages:virtualKeys.copy')}
              </button>
            </div>
            <div className={styles.keyWarning}>
              {t('pages:virtualKeys.saveKeyWarning')}
            </div>
            <Stack direction="horizontal" gap="sm">
              <Button type="button" variant="secondary" onClick={() => navigate('/ai-gateway/virtual-keys')}>
                {t('pages:virtualKeys.backToList')}
              </Button>
              {createdId && (
                <Button type="button" onClick={() => navigate(`/ai-gateway/virtual-keys/${createdId}`)}>
                  {t('pages:virtualKeys.goToDetail')}
                </Button>
              )}
            </Stack>
          </Stack>
        </Card>
      </Stack>
    );
  }

  return (
    <Stack gap="lg">
      <Breadcrumb items={[
        { label: t('pages:virtualKeys.title'), to: '/ai-gateway/virtual-keys' },
        { label: t('pages:virtualKeys.createVirtualKey') },
      ]} />

      <PageHeader title={t('pages:virtualKeys.createVirtualKey')} subtitle={t('pages:virtualKeys.createSubtitle')} />

      <Card>
        <form onSubmit={form.handleSubmit(onSubmit)}>
          <Stack gap="md">
            <FormInput form={form} name="name" label={t('pages:virtualKeys.name')} required helpText={t('pages:virtualKeys.nameHelpText')} placeholder={t('pages:virtualKeys.namePlaceholder')} />

            <div>
              <label className={styles.tooltipLabel}>
                {t('pages:virtualKeys.project')}
                <Tooltip content={t('pages:virtualKeys.projectTooltip')}>
                  <span role="presentation">&#9432;</span>
                </Tooltip>
              </label>
              <select value={form.watch('projectId')} onChange={e => form.setValue('projectId', e.target.value)} className={`${styles.filterSelect} ${styles.projectSelect}`}>
                <option value="">{t('pages:virtualKeys.none')}</option>
                {(projectsData?.data ?? []).map(p => <option key={p.id} value={p.id}>{p.name}{p.organization ? ` (${p.organization.name})` : ''}</option>)}
              </select>
            </div>

            <FormInput form={form} name="sourceApp" label={t('pages:virtualKeys.sourceApp')} placeholder={t('pages:virtualKeys.placeholderSourceApp')} />

            <GroupedModelSelect groups={modelsData?.data ?? []} selected={selectedModels} onChange={setSelectedModels} />

            <FormInput form={form} name="rateLimitRpm" label={t('pages:virtualKeys.rateLimitRpm')} helpText={t('pages:virtualKeys.rateLimitHelpText')} type="number" placeholder={t('pages:virtualKeys.placeholderRpm')} />

            <div>
              <label className={styles.tooltipLabel}>
                {t('pages:virtualKeys.expiration')}
                <Tooltip content={t('pages:virtualKeys.expirationTooltip')}>
                  <span role="presentation">&#9432;</span>
                </Tooltip>
              </label>
              <Stack direction="horizontal" gap="xs" className={styles.enabledRow}>
                <Input type="date" value={form.watch('expiresAt')} onChange={e => form.setValue('expiresAt', e.target.value)} disabled={neverExpires}
                  className={styles.expirationDateInput} />
                <label className={styles.neverExpiresLabel}>
                  <input type="checkbox" checked={neverExpires} onChange={e => { form.setValue('neverExpires', e.target.checked); if (e.target.checked) form.setValue('expiresAt', ''); }} />
                  {t('pages:virtualKeys.neverExpires')}
                </label>
              </Stack>
            </div>

            <FormSwitch form={form} name="enabled" label={t('common:enabled')} />

            <Stack direction="horizontal" gap="sm" className={styles.formActions}>
              <Button type="button" variant="secondary" onClick={() => navigate('/ai-gateway/virtual-keys')}>{t('common:cancel')}</Button>
              <Button
                type="submit"
                disabled={loading || !form.formState.isValid}
              >
                {loading ? t('pages:virtualKeys.creating') : t('pages:virtualKeys.createVirtualKey')}
              </Button>
            </Stack>
          </Stack>
        </form>
      </Card>
    </Stack>
  );
}
