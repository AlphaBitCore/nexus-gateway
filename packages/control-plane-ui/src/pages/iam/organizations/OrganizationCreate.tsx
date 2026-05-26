import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { organizationApi } from '@/api/services';
import type { CreateOrganizationInput } from '@/api/services';
import { useMutation } from '../../../hooks/useMutation';
import { useZodForm, FormInput, FormSwitch } from '@/lib/forms';
import { useUnsavedChangesWarning } from '@/hooks/useUnsavedChangesWarning';
import {
  PageHeader, Breadcrumb, Button, Stack, Card, OrgTreeSelect,
} from '@/components/ui';
import styles from './OrganizationCreate.module.css';

/* ── Schema ─────────────────────────────────────────────────────────────── */

const schema = z.object({
  name: z.string().min(1, 'Name is required'),
  code: z.string().min(1, 'Code is required'),
  parentId: z.string().optional().default(''),
  description: z.string().optional().default(''),
  contactName: z.string().optional().default(''),
  contactEmail: z.string().optional().default(''),
  contactPhone: z.string().optional().default(''),
  enabled: z.boolean().default(true),
  timezone: z.string().optional().default(''),
});

type FormValues = z.infer<typeof schema>;

/* ── Component ──────────────────────────────────────────────────────────── */

export function OrganizationCreate() {
  const { t } = useTranslation();
  const navigate = useNavigate();

  const form = useZodForm({
    schema,
    defaultValues: {
      name: '',
      code: '',
      parentId: '',
      description: '',
      contactName: '',
      contactEmail: '',
      contactPhone: '',
      enabled: true,
      timezone: '',
    },
  });

  useUnsavedChangesWarning(form.formState.isDirty);

  const { mutate, loading } = useMutation(
    (data: CreateOrganizationInput) => organizationApi.create(data),
    {
      invalidateQueries: [['api', 'admin', 'organizations']],
      onSuccess: () => navigate('/iam/organizations'),
      successMessage: t('pages:organizations.organizationCreated'),
    },
  );

  const onSubmit = (values: FormValues) => {
    mutate({
      name: values.name,
      code: values.code,
      parentId: values.parentId || undefined,
      description: values.description || undefined,
      contactName: values.contactName || undefined,
      contactEmail: values.contactEmail || undefined,
      contactPhone: values.contactPhone || undefined,
      enabled: values.enabled,
      timezone: values.timezone || undefined,
    });
  };

  return (
    <Stack gap="md">
      <Breadcrumb items={[
        { label: t('pages:organizations.title'), to: '/iam/organizations' },
        { label: t('pages:organizations.createOrganization') },
      ]} />

      <PageHeader title={t('pages:organizations.createOrganization')} subtitle={t('pages:organizations.createSubtitle')} />

      <Card>
        <form onSubmit={form.handleSubmit(onSubmit)}>
          <Stack gap="md">
            <FormInput form={form} name="name" label={t('pages:organizations.name')} required placeholder={t('pages:organizations.placeholderName')} />
            <FormInput form={form} name="code" label={t('pages:organizations.code')} required helpText={t('pages:organizations.codeCreateHelpText')} placeholder={t('pages:organizations.placeholderCode')} />
            <div>
              <label>{t('pages:organizations.parentOrganization')}</label>
              <OrgTreeSelect
                value={form.watch('parentId')}
                onChange={(val) => form.setValue('parentId', val as string, { shouldDirty: true })}
                placeholder={t('pages:organizations.noneRoot')}
                allowClear
              />
            </div>
            <FormInput form={form} name="description" label={t('pages:organizations.description')} placeholder={t('pages:organizations.placeholderDescription')} />
            <FormInput form={form} name="contactName" label={t('pages:organizations.contactName')} placeholder={t('pages:organizations.placeholderContactName')} />
            <FormInput form={form} name="contactEmail" label={t('pages:organizations.contactEmail')} type="email" placeholder={t('pages:organizations.placeholderContactEmail')} />
            <FormInput form={form} name="contactPhone" label={t('pages:organizations.contactPhone')} placeholder={t('pages:organizations.placeholderContactPhone')} />
            <FormInput form={form} name="timezone" label={t('pages:organizations.timezone')} helpText={t('pages:organizations.timezoneHelpText')} placeholder={t('pages:organizations.timezonePlaceholder')} />
            <FormSwitch form={form} name="enabled" label={t('pages:organizations.enabled')} helpText={t('pages:organizations.enabledHelpText')} />

            <Stack direction="horizontal" gap="sm" className={styles.justifyEnd}>
              <Button type="button" variant="secondary" onClick={() => navigate('/iam/organizations')}>{t('common:cancel')}</Button>
              <Button
                type="submit"
                disabled={loading || !form.formState.isValid}
              >
                {loading ? t('pages:organizations.creating') : t('pages:organizations.createOrganization')}
              </Button>
            </Stack>
          </Stack>
        </form>
      </Card>
    </Stack>
  );
}
