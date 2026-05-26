import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { personalVKApi } from '@/api/services/ai-gateway/personalVirtualKeys';
import type { CreatePersonalVKInput } from '@/api/services/ai-gateway/personalVirtualKeys';
import { useMutation } from '@/hooks/useMutation';
import { useZodForm, FormInput } from '@/lib/forms';
import { useUnsavedChangesWarning } from '@/hooks/useUnsavedChangesWarning';
import {
  PageHeader, Breadcrumb, Button, Stack, Card,
} from '@/components/ui';
import styles from './PersonalVKCreate.module.css';

/* -- Schema ---------------------------------------------------------------- */

const schema = z.object({
  name: z.string().min(1, 'Name is required'),
  sourceApp: z.string().optional().default(''),
  rateLimitRpm: z.string().optional().default(''),
});

type FormValues = z.infer<typeof schema>;

/* -- Component ------------------------------------------------------------- */

export function PersonalVKCreate() {
  const { t } = useTranslation();
  const navigate = useNavigate();

  const form = useZodForm({
    schema,
    defaultValues: {
      name: '',
      sourceApp: '',
      rateLimitRpm: '',
    },
  });

  useUnsavedChangesWarning(form.formState.isDirty);

  const { mutate, loading } = useMutation(
    (data: CreatePersonalVKInput) => personalVKApi.create(data),
    {
      invalidateQueries: [['api', 'user', 'virtual-keys']],
      onSuccess: () => navigate('/account?tab=virtualKeys'),
      successMessage: t('pages:personalVks.created'),
    },
  );

  const onSubmit = (values: FormValues) => {
    mutate({
      name: values.name,
      sourceApp: values.sourceApp || undefined,
      rateLimitRpm: values.rateLimitRpm ? Number(values.rateLimitRpm) : undefined,
    });
  };

  return (
    <Stack gap="md">
      <Breadcrumb items={[
        { label: t('pages:personalVks.title'), to: '/account?tab=virtualKeys' },
        { label: t('pages:personalVks.createVk') },
      ]} />

      <PageHeader
        title={t('pages:personalVks.createVk')}
        subtitle={t('pages:personalVks.createSubtitle')}
      />

      <Card>
        <form onSubmit={form.handleSubmit(onSubmit)}>
          <Stack gap="md">
            <FormInput form={form} name="name" label={t('pages:personalVks.name')} required placeholder={t('pages:personalVks.namePlaceholder')} />
            <FormInput form={form} name="sourceApp" label={t('pages:personalVks.sourceApp')} placeholder={t('pages:personalVks.optional')} />
            <FormInput form={form} name="rateLimitRpm" label={t('pages:personalVks.rpm')} type="number" placeholder={t('pages:personalVks.optional')} />

            <Stack direction="horizontal" gap="sm" className={styles.justifyEnd}>
              <Button type="button" variant="secondary" onClick={() => navigate('/account?tab=virtualKeys')}>{t('common:cancel')}</Button>
              <Button type="submit" disabled={loading || !form.formState.isValid}>
                {loading ? t('pages:personalVks.creating') : t('pages:personalVks.createVk')}
              </Button>
            </Stack>
          </Stack>
        </form>
      </Card>
    </Stack>
  );
}
