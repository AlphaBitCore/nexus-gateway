import { useState } from 'react';
import { useNavigate, useSearchParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { iamApi } from '@/api/services';
import type { CreateAdminUserInput } from '@/api/services';
import { useMutation } from '../../../hooks/useMutation';
import { useZodForm, FormInput } from '@/lib/forms';
import { useUnsavedChangesWarning } from '@/hooks/useUnsavedChangesWarning';
import {
  PageHeader, Breadcrumb, Button, Stack, Card, FormField, Switch,
} from '@/components/ui';
import { OrgTreeSelect } from '@/components/ui/OrgTreeSelect';
import styles from './IamUserCreate.module.css';

/* ── Schema ─────────────────────────────────────────────────────────────── */

const schema = z.object({
  username: z.string().min(1, 'Display name is required'),
  email: z.string().optional().default(''),
  password: z.string().min(1, 'Password is required'),
});

type FormValues = z.infer<typeof schema>;

/* ── Component ──────────────────────────────────────────────────────────── */

export function IamUserCreate() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const [organizationId, setOrganizationId] = useState<string>(searchParams.get('orgId') ?? '');
  const [canAccessControlPlane, setCanAccessControlPlane] = useState(true);

  const form = useZodForm({
    schema,
    defaultValues: {
      username: '',
      email: '',
      password: '',
    },
  });

  useUnsavedChangesWarning(form.formState.isDirty);

  const { mutate, loading } = useMutation(
    (data: CreateAdminUserInput) => iamApi.createUser(data),
    {
      onSuccess: (result) => {
        navigate(`/iam/users/${result.id}`);
      },
      successMessage: t('pages:iam.userCreated'),
    },
  );

  const onSubmit = (values: FormValues) => {
    mutate({
      username: values.username.trim(),
      email: values.email?.trim() || undefined,
      password: values.password,
      organizationId: organizationId || undefined,
      canAccessControlPlane,
    });
  };

  return (
    <Stack gap="lg">
      <Breadcrumb items={[
        { label: t('pages:iam.users'), to: '/iam/users' },
        { label: t('pages:iam.createUser') },
      ]} />

      <PageHeader title={t('pages:iam.createUser')} subtitle={t('pages:iam.createUserSubtitle')} />

      <Card>
        <form onSubmit={form.handleSubmit(onSubmit)}>
          <Stack gap="md">
            <FormInput form={form} name="username" label={t('pages:iam.displayName')} required placeholder={t('pages:iam.placeholderUsername')} />
            <FormInput form={form} name="email" label={t('pages:iam.email')} type="email" placeholder={t('pages:iam.placeholderEmail')} />
            <FormInput form={form} name="password" label={t('pages:iam.password')} required type="password" placeholder={t('pages:iam.placeholderPassword')} />
            <FormField label={t('pages:iam.organization')}>
              <OrgTreeSelect
                mode="single"
                allowClear
                value={organizationId}
                onChange={(v) => setOrganizationId(v as string)}
                placeholder={t('pages:iam.selectOrg')}
              />
            </FormField>
            <FormField
              label={t('pages:iam.canAccessControlPlane')}
              helpText={t('pages:iam.canAccessControlPlaneHelpText')}
            >
              <Switch checked={canAccessControlPlane} onCheckedChange={setCanAccessControlPlane} />
            </FormField>

            <Stack direction="horizontal" gap="sm" className={styles.formActions}>
              <Button type="button" variant="secondary" onClick={() => navigate('/iam/users')}>{t('common:cancel')}</Button>
              <Button
                type="submit"
                disabled={!form.formState.isValid || loading}
              >
                {loading ? t('pages:iam.creating') : t('pages:iam.createUser')}
              </Button>
            </Stack>
          </Stack>
        </form>
      </Card>
    </Stack>
  );
}
