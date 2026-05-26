import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { projectApi } from '@/api/services';
import type { CreateProjectInput } from '@/api/services';
import { useMutation } from '../../../hooks/useMutation';
import { useZodForm, FormInput } from '@/lib/forms';
import { useUnsavedChangesWarning } from '@/hooks/useUnsavedChangesWarning';
import {
  PageHeader, Breadcrumb, Button, Stack, Card, OrgTreeSelect,
} from '@/components/ui';
import styles from './ProjectCreate.module.css';

/* ── Schema ─────────────────────────────────────────────────────────────── */

const schema = z.object({
  name: z.string().min(1, 'Name is required'),
  code: z.string().min(1, 'Code is required'),
  organizationId: z.string().optional().default(''),
  description: z.string().optional().default(''),
  contactName: z.string().optional().default(''),
  contactEmail: z.string().optional().default(''),
});

type FormValues = z.infer<typeof schema>;

/* ── Component ──────────────────────────────────────────────────────────── */

export function ProjectCreate() {
  const { t } = useTranslation();
  const navigate = useNavigate();

  const form = useZodForm({
    schema,
    defaultValues: {
      name: '',
      code: '',
      organizationId: '',
      description: '',
      contactName: '',
      contactEmail: '',
    },
  });

  useUnsavedChangesWarning(form.formState.isDirty);

  const { mutate, loading } = useMutation(
    (data: CreateProjectInput) => projectApi.create(data),
    {
      invalidateQueries: [['api', 'admin', 'projects']],
      onSuccess: () => navigate('/iam/projects'),
      successMessage: t('pages:projects.projectCreated'),
    },
  );

  const onSubmit = (values: FormValues) => {
    mutate({
      name: values.name,
      code: values.code,
      organizationId: values.organizationId || undefined,
      description: values.description || undefined,
      contactName: values.contactName || undefined,
      contactEmail: values.contactEmail || undefined,
    });
  };

  return (
    <Stack gap="md">
      <Breadcrumb items={[
        { label: t('pages:projects.title'), to: '/iam/projects' },
        { label: t('pages:projects.createProject') },
      ]} />

      <PageHeader title={t('pages:projects.createProject')} subtitle={t('pages:projects.createSubtitle')} />

      <Card>
        <form onSubmit={form.handleSubmit(onSubmit)}>
          <Stack gap="md">
            <FormInput form={form} name="name" label={t('pages:projects.name')} required placeholder={t('pages:projects.placeholderName')} />
            <FormInput form={form} name="code" label={t('pages:projects.code')} required helpText={t('pages:projects.codeCreateHelpText')} placeholder={t('pages:projects.placeholderCode')} />
            <div>
              <label>{t('pages:projects.organization')} *</label>
              <OrgTreeSelect
                value={form.watch('organizationId')}
                onChange={(val) => form.setValue('organizationId', val as string, { shouldDirty: true })}
                placeholder={t('pages:projects.selectOrganization')}
              />
            </div>
            <FormInput form={form} name="description" label={t('pages:projects.description')} placeholder={t('pages:projects.placeholderDescription')} />
            <FormInput form={form} name="contactName" label={t('pages:projects.contactName')} placeholder={t('pages:projects.placeholderContactName')} />
            <FormInput form={form} name="contactEmail" label={t('pages:projects.contactEmail')} type="email" placeholder={t('pages:projects.placeholderContactEmail')} />

            <Stack direction="horizontal" gap="sm" className={styles.justifyEnd}>
              <Button type="button" variant="secondary" onClick={() => navigate('/iam/projects')}>{t('common:cancel')}</Button>
              <Button
                type="submit"
                disabled={loading || !form.formState.isValid}
              >
                {loading ? t('pages:projects.creating') : t('pages:projects.createProject')}
              </Button>
            </Stack>
          </Stack>
        </form>
      </Card>
    </Stack>
  );
}
