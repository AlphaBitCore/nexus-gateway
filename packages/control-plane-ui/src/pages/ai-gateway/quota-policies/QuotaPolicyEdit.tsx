import { useEffect } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { useApi } from '@/hooks/useApi';
import { quotaPolicyApi } from '@/api/services';
import type { CreateQuotaPolicyInput, QuotaPolicy } from '@/api/services';
import { useMutation } from '@/hooks/useMutation';
import { useZodForm, FormInput, FormSelect, FormCheckbox } from '@/lib/forms';
import { useUnsavedChangesWarning } from '@/hooks/useUnsavedChangesWarning';
import {
  PageHeader, Breadcrumb, Button, Stack, Card, Skeleton, ErrorBanner,
  OrgTreeSelect,
} from '@/components/ui';
import styles from './QuotaPolicyCreate.module.css';

const schema = z.object({
  name: z.string().min(1),
  description: z.string().optional().default(''),
  scope: z.string().min(1),
  organizationId: z.string().optional().default(''),
  vkType: z.string().optional().default(''),
  periodType: z.string().min(1),
  costLimitUsd: z.string().optional().default(''),
  tokenLimit: z.string().optional().default(''),
  enforcementMode: z.string().min(1),
  priority: z.string().optional().default('0'),
  enabled: z.boolean().default(true),
  alertThresholds: z.string().optional().default('80, 90'),
});

type FormValues = z.infer<typeof schema>;

// sanitizePolicy drops scope-incompatible organizationId / vkType so the form
// opens on legal state even if the persisted row predates the strict matrix.
function sanitizePolicy(p: QuotaPolicy): { scope: string; organizationId: string; vkType: string } {
  const scope = p.scope;
  let organizationId = p.organizationId ?? '';
  let vkType = p.vkType ?? '';
  if (scope === 'project' || scope === 'vk') organizationId = '';
  if (scope !== 'vk') vkType = '';
  return { scope, organizationId, vkType };
}

export function QuotaPolicyEdit() {
  const { id } = useParams<{ id: string }>();

  const { data: policy, loading: fetching, error } = useApi<QuotaPolicy>(
    () => quotaPolicyApi.get(id!),
    ['admin', 'quota-policies', id!],
  );

  if (fetching || !policy) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} />;

  return <QuotaPolicyEditForm id={id!} policy={policy} />;
}

function QuotaPolicyEditForm({ id, policy }: { id: string; policy: QuotaPolicy }) {
  const { t } = useTranslation();
  const navigate = useNavigate();

  const { scope: initialScope, organizationId: initialOrg, vkType: initialVkType } = sanitizePolicy(policy);

  const form = useZodForm({
    schema,
    defaultValues: {
      name: policy.name,
      description: policy.description ?? '',
      scope: initialScope,
      organizationId: initialOrg,
      vkType: initialVkType,
      periodType: policy.periodType,
      costLimitUsd: policy.costLimitUsd != null ? String(policy.costLimitUsd) : '',
      tokenLimit: policy.tokenLimit != null ? String(policy.tokenLimit) : '',
      enforcementMode: policy.enforcementMode,
      priority: policy.priority != null ? String(policy.priority) : '0',
      enabled: policy.enabled,
      alertThresholds: policy.alertThresholds?.join(', ') ?? '80, 90',
    },
  });

  const scope = form.watch('scope');
  const organizationId = form.watch('organizationId');
  const vkType = form.watch('vkType');

  // On scope change (after initial render), clear the dependent fields so the
  // user cannot carry stale values across types.
  useEffect(() => {
    if (scope === initialScope) return;
    form.setValue('organizationId', '', { shouldDirty: false });
    form.setValue('vkType', '', { shouldDirty: false });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [scope]);

  useUnsavedChangesWarning(form.formState.isDirty);

  const { mutate, loading } = useMutation(
    (data: CreateQuotaPolicyInput) => quotaPolicyApi.update(id, data),
    {
      invalidateQueries: [['api', 'admin', 'quota-policies']],
      onSuccess: () => navigate('/ai-gateway/quota-policies'),
      successMessage: t('pages:quotaPolicies.policyUpdated'),
    },
  );

  const onSubmit = (values: FormValues) => {
    const payload: CreateQuotaPolicyInput = {
      name: values.name,
      description: values.description || undefined,
      scope: values.scope,
      periodType: values.periodType,
      costLimitUsd: values.costLimitUsd ? Number(values.costLimitUsd) : undefined,
      tokenLimit: values.tokenLimit ? Number(values.tokenLimit) : undefined,
      enforcementMode: values.enforcementMode,
      priority: values.priority ? Number(values.priority) : 0,
      enabled: values.enabled,
      alertThresholds: values.alertThresholds
        ? values.alertThresholds.split(',').map((s) => parseInt(s.trim(), 10)).filter((n) => !isNaN(n) && n >= 1 && n <= 100)
        : [80, 90],
    };
    if (values.scope === 'organization') payload.organizationId = values.organizationId;
    else if (values.scope === 'user' && values.organizationId) payload.organizationId = values.organizationId;
    else if (values.scope === 'vk') payload.vkType = values.vkType;
    mutate(payload);
  };

  const submitDisabled =
    loading ||
    (scope === 'organization' && !organizationId) ||
    (scope === 'vk' && !vkType);

  const helpKey =
    scope === 'organization' ? 'helpOrganization'
    : scope === 'user' ? 'helpUser'
    : scope === 'project' ? 'helpProject'
    : 'helpVk';

  return (
    <Stack gap="md">
      <Breadcrumb items={[
        { label: t('pages:quotaPolicies.title'), to: '/ai-gateway/quota-policies' },
        { label: t('pages:quotaPolicies.editPolicy') },
      ]} />

      <PageHeader
        title={t('pages:quotaPolicies.editPolicy')}
        subtitle={t('pages:quotaPolicies.editSubtitle')}
      />

      <form onSubmit={form.handleSubmit(onSubmit)}>
        <Stack gap="md">

          <Card>
            <h3 className={styles.sectionTitle}>{t('pages:quotaPolicies.basicInfo')}</h3>
            <div className={styles.formGrid}>
              <FormInput form={form} name="name" label={t('pages:quotaPolicies.name')} tooltip={t('pages:quotaPolicies.nameTooltip')} required />
              <FormInput form={form} name="description" label={t('pages:quotaPolicies.description')} tooltip={t('pages:quotaPolicies.descriptionTooltip')} placeholder={t('pages:quotaPolicies.optional')} />
            </div>
          </Card>

          <Card>
            <h3 className={styles.sectionTitle}>{t('pages:quotaPolicies.policyType')}</h3>
            <div className={styles.formGrid}>
              <FormSelect
                form={form}
                name="scope"
                label={t('pages:quotaPolicies.policyType')}
                tooltip={t('pages:quotaPolicies.policyTypeTooltip')}
                required
                options={[
                  { value: 'organization', label: t('pages:quotaPolicies.typeOrganization') },
                  { value: 'user',         label: t('pages:quotaPolicies.typeUser') },
                  { value: 'project',      label: t('pages:quotaPolicies.typeProject') },
                  { value: 'vk',           label: t('pages:quotaPolicies.typeVk') },
                ]}
              />

              <div className={styles.formGridFull}>
                <p className={styles.helpText}>{t(`pages:quotaPolicies.${helpKey}`)}</p>
              </div>

              {(scope === 'organization' || scope === 'user') && (
                <div className={styles.formGridFull}>
                  <label className={styles.fieldLabel}>
                    {scope === 'organization'
                      ? t('pages:quotaPolicies.organization')
                      : t('pages:quotaPolicies.orgFilterLabel')}
                  </label>
                  <OrgTreeSelect
                    value={organizationId}
                    onChange={(val) => form.setValue('organizationId', val as string, { shouldDirty: true, shouldValidate: true })}
                    placeholder={t('pages:quotaPolicies.selectOrganization')}
                    allowClear={scope === 'user'}
                  />
                </div>
              )}

              {scope === 'vk' && (
                <FormSelect
                  form={form}
                  name="vkType"
                  label={t('pages:quotaPolicies.vkTypeRequiredLabel')}
                  tooltip={t('pages:quotaPolicies.helpVk')}
                  required
                  options={[
                    { value: 'personal',    label: t('pages:quotaPolicies.vkTypePersonal') },
                    { value: 'application', label: t('pages:quotaPolicies.vkTypeApplication') },
                  ]}
                />
              )}
            </div>
          </Card>

          <Card>
            <h3 className={styles.sectionTitle}>{t('pages:quotaPolicies.limitsAndEnforcement')}</h3>
            <div className={styles.formGrid}>
              <FormSelect
                form={form}
                name="periodType"
                label={t('pages:quotaPolicies.periodType')}
                tooltip={t('pages:quotaPolicies.periodTypeTooltip')}
                required
                options={[
                  { value: 'daily',   label: t('pages:quotaPolicies.daily') },
                  { value: 'weekly',  label: t('pages:quotaPolicies.weekly') },
                  { value: 'monthly', label: t('pages:quotaPolicies.monthly') },
                ]}
              />
              <FormSelect
                form={form}
                name="enforcementMode"
                label={t('pages:quotaPolicies.enforcementMode')}
                tooltip={t('pages:quotaPolicies.enforcementModeTooltip')}
                required
                options={[
                  { value: 'reject',              label: t('pages:quotaPolicies.reject') },
                  { value: 'downgrade',           label: t('pages:quotaPolicies.downgrade') },
                  { value: 'notify-and-proceed',  label: t('pages:quotaPolicies.notifyAndProceed') },
                  { value: 'track-only',          label: t('pages:quotaPolicies.trackOnly') },
                ]}
              />
              <FormInput form={form} name="costLimitUsd" label={t('pages:quotaPolicies.costLimit')} tooltip={t('pages:quotaPolicies.costLimitTooltip')} type="number" placeholder={t('pages:quotaPolicies.optional')} />
              <FormInput form={form} name="tokenLimit" label={t('pages:quotaPolicies.tokenLimit')} tooltip={t('pages:quotaPolicies.tokenLimitTooltip')} type="number" placeholder={t('pages:quotaPolicies.optional')} />
              <FormInput form={form} name="alertThresholds" label={t('pages:quotaPolicies.alertThresholds')} tooltip={t('pages:quotaPolicies.alertThresholdsTooltip')} placeholder="80, 90, 95" />
            </div>
          </Card>

          <Card>
            <h3 className={styles.sectionTitle}>{t('pages:quotaPolicies.advanced')}</h3>
            <div className={styles.formGrid}>
              <FormInput form={form} name="priority" label={t('pages:quotaPolicies.priority')} tooltip={t('pages:quotaPolicies.priorityTooltip')} type="number" />
              <div>
                <FormCheckbox form={form} name="enabled" label={t('pages:quotaPolicies.enabled')} tooltip={t('pages:quotaPolicies.enabledTooltip')} />
              </div>
            </div>
          </Card>

          <Stack direction="horizontal" gap="sm" className={styles.justifyEnd}>
            <Button type="button" variant="secondary" onClick={() => navigate('/ai-gateway/quota-policies')}>{t('common:cancel')}</Button>
            <Button type="submit" disabled={submitDisabled}>
              {loading ? t('pages:quotaPolicies.saving') : t('common:save')}
            </Button>
          </Stack>
        </Stack>
      </form>
    </Stack>
  );
}
