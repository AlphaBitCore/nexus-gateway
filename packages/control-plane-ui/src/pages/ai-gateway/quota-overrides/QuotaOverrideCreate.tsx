import { useState, useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import {
  quotaOverrideApi, projectApi, virtualKeyApi, iamApi,
} from '@/api/services';
import type { CreateQuotaOverrideInput } from '@/api/services';
import { useMutation } from '@/hooks/useMutation';
import { useZodForm, FormInput, FormSelect, FormTextarea } from '@/lib/forms';
import { useUnsavedChangesWarning } from '@/hooks/useUnsavedChangesWarning';
import {
  PageHeader, Breadcrumb, Button, Stack, Card, SearchableCombobox, OrgTreeSelect,
} from '@/components/ui';
import type { ComboboxOption } from '@/components/ui';
import styles from './QuotaOverrideCreate.module.css';

/* -- Schema ---------------------------------------------------------------- */

const schema = z.object({
  targetType: z.string().min(1, 'Target type is required'),
  targetId: z.string().min(1, 'Target ID is required'),
  reason: z.string().optional().default(''),
  costLimitUsd: z.string().optional().default(''),
  tokenLimit: z.string().optional().default(''),
  enforcementMode: z.string().optional().default(''),
  periodType: z.string().optional().default(''),
});

type FormValues = z.infer<typeof schema>;

/* -- Target searchable select ---------------------------------------------- */

async function fetchTargetOptions(targetType: string, query: string): Promise<ComboboxOption[]> {
  switch (targetType) {
    case 'user': {
      const params: Record<string, string> = { limit: '100' };
      if (query.trim()) params.q = query.trim();
      const res = await iamApi.listUsers(params);
      const rows = (res as { data: Array<{ id: string; displayName?: string; email?: string }> }).data ?? [];
      return rows.map((u) => ({
        id: u.id,
        label: u.displayName ? `${u.displayName} (${u.email ?? u.id})` : u.email ?? u.id,
      }));
    }
    case 'vk': {
      const params: Record<string, string> = { limit: '100' };
      if (query.trim()) params.q = query.trim();
      const res = await virtualKeyApi.list(params);
      const rows = res.data ?? [];
      return rows.map((k) => ({ id: k.id, label: k.name }));
    }
    case 'project': {
      const params: Record<string, string> = { limit: '100' };
      if (query.trim()) params.q = query.trim();
      const res = await projectApi.list(params);
      const rows = res.data ?? [];
      return rows.map((p) => ({ id: p.id, label: `${p.name} (${p.code})` }));
    }
    default:
      return [];
  }
}

/* -- Component ------------------------------------------------------------- */

export function QuotaOverrideCreate() {
  const { t } = useTranslation();
  const navigate = useNavigate();

  const [targetLabel, setTargetLabel] = useState('');

  const form = useZodForm({
    schema,
    defaultValues: {
      targetType: 'user',
      targetId: '',
      reason: '',
      costLimitUsd: '',
      tokenLimit: '',
      enforcementMode: '_inherit',
      periodType: '_inherit',
    },
  });

  const targetType = form.watch('targetType');

  // Clear target when type changes
  useEffect(() => {
    form.setValue('targetId', '', { shouldDirty: false });
    setTargetLabel('');
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [targetType]);

  useUnsavedChangesWarning(form.formState.isDirty);

  const { mutate, loading } = useMutation(
    (data: CreateQuotaOverrideInput) => quotaOverrideApi.create(data),
    {
      invalidateQueries: [['api', 'admin', 'quota-overrides']],
      onSuccess: () => navigate('/ai-gateway/quota-overrides'),
      successMessage: t('pages:quotaOverrides.overrideCreated'),
    },
  );

  const onSubmit = (values: FormValues) => {
    mutate({
      targetType: values.targetType,
      targetId: values.targetId,
      reason: values.reason || undefined,
      costLimitUsd: values.costLimitUsd ? Number(values.costLimitUsd) : undefined,
      tokenLimit: values.tokenLimit ? Number(values.tokenLimit) : undefined,
      enforcementMode: values.enforcementMode === '_inherit' ? undefined : (values.enforcementMode || undefined),
      periodType: values.periodType === '_inherit' ? undefined : (values.periodType || undefined),
    });
  };

  const fetchTargets = (q: string) => fetchTargetOptions(targetType, q);

  const inheritLabel = t('pages:quotaOverrides.inheritFromPolicy');

  return (
    <Stack gap="md">
      <Breadcrumb items={[
        { label: t('pages:quotaOverrides.title'), to: '/ai-gateway/quota-overrides' },
        { label: t('pages:quotaOverrides.createOverride') },
      ]} />

      <PageHeader
        title={t('pages:quotaOverrides.createOverride')}
        subtitle={t('pages:quotaOverrides.createSubtitle')}
      />

      <form onSubmit={form.handleSubmit(onSubmit)}>
        <Stack gap="md">
          {/* Section 1: Target */}
          <Card>
            <h3 className={styles.sectionTitle}>{t('pages:quotaOverrides.targetSection')}</h3>
            <div className={styles.formGrid}>
              <FormSelect
                form={form}
                name="targetType"
                label={t('pages:quotaOverrides.targetType')}
                tooltip={t('pages:quotaOverrides.targetTypeTooltip')}
                required
                options={[
                  { value: 'user', label: t('pages:quotaOverrides.scopeUser') },
                  { value: 'vk', label: t('pages:quotaOverrides.scopeVk') },
                  { value: 'project', label: t('pages:quotaOverrides.scopeProject') },
                  { value: 'organization', label: t('pages:quotaOverrides.scopeOrganization') },
                ]}
              />
              <div>
                <label className={styles.fieldLabel}>{t('pages:quotaOverrides.target')}</label>
                {targetType === 'organization' ? (
                  <OrgTreeSelect
                    value={form.watch('targetId')}
                    onChange={(val) => {
                      form.setValue('targetId', val as string, { shouldDirty: true, shouldValidate: true });
                    }}
                    placeholder={t('pages:quotaOverrides.selectTarget')}
                    allowClear
                  />
                ) : (
                  <SearchableCombobox
                    ariaLabel={t('pages:quotaOverrides.target')}
                    placeholder={t('pages:quotaOverrides.selectTarget')}
                    valueId={form.watch('targetId')}
                    valueLabel={targetLabel}
                    allowEmptyQueryFetch
                    fetchOptions={fetchTargets}
                    onSelect={(opt) => {
                      form.setValue('targetId', opt?.id ?? '', { shouldDirty: true, shouldValidate: true });
                      setTargetLabel(opt?.label ?? '');
                    }}
                  />
                )}
              </div>
            </div>
          </Card>

          {/* Section 2: Override Settings */}
          <Card>
            <h3 className={styles.sectionTitle}>{t('pages:quotaOverrides.targetSettings')}</h3>
            <div className={styles.formGrid}>
              <FormInput form={form} name="costLimitUsd" label={t('pages:quotaOverrides.costLimit')} tooltip={t('pages:quotaOverrides.costLimitTooltip')} type="number" placeholder={t('pages:quotaOverrides.optional')} />
              <FormInput form={form} name="tokenLimit" label={t('pages:quotaOverrides.tokenLimit')} tooltip={t('pages:quotaOverrides.tokenLimitTooltip')} type="number" placeholder={t('pages:quotaOverrides.optional')} />
              <FormSelect
                form={form}
                name="enforcementMode"
                label={t('pages:quotaOverrides.enforcementMode')}
                tooltip={t('pages:quotaOverrides.enforcementModeTooltip')}
                options={[
                  { value: '_inherit', label: inheritLabel },
                  { value: 'reject', label: t('pages:quotaOverrides.reject') },
                  { value: 'downgrade', label: t('pages:quotaOverrides.downgrade') },
                  { value: 'notify-and-proceed', label: t('pages:quotaOverrides.notifyAndProceed') },
                  { value: 'track-only', label: t('pages:quotaOverrides.trackOnly') },
                ]}
              />
              <FormSelect
                form={form}
                name="periodType"
                label={t('pages:quotaOverrides.periodType')}
                tooltip={t('pages:quotaOverrides.periodTypeTooltip')}
                options={[
                  { value: '_inherit', label: inheritLabel },
                  { value: 'daily', label: t('pages:quotaOverrides.daily') },
                  { value: 'weekly', label: t('pages:quotaOverrides.weekly') },
                  { value: 'monthly', label: t('pages:quotaOverrides.monthly') },
                ]}
              />
              <div className={styles.formGridFull}>
                <FormTextarea form={form} name="reason" label={t('pages:quotaOverrides.reason')} tooltip={t('pages:quotaOverrides.reasonTooltip')} placeholder={t('pages:quotaOverrides.optional')} />
              </div>
            </div>
          </Card>

          <Stack direction="horizontal" gap="sm" className={styles.justifyEnd}>
            <Button type="button" variant="secondary" onClick={() => navigate('/ai-gateway/quota-overrides')}>{t('common:cancel')}</Button>
            <Button type="submit" disabled={loading || !form.formState.isValid}>
              {loading ? t('pages:quotaOverrides.saving') : t('pages:quotaOverrides.createOverride')}
            </Button>
          </Stack>
        </Stack>
      </form>
    </Stack>
  );
}
