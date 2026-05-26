import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { z } from 'zod';
import { useApi } from '@/hooks/useApi';
import { credentialApi, providerApi } from '@/api/services';
import type { CreateCredentialInput } from '@/api/services';
import { useMutation } from '@/hooks/useMutation';
import { PageHeader, Breadcrumb, Button, Stack, Card, FormField, Switch } from '@/components/ui';
import { useZodForm, FormInput, FormSelect } from '@/lib/forms';
import { useUnsavedChangesWarning } from '@/hooks/useUnsavedChangesWarning';
import type { Provider } from '@/api/types';
import { ADMIN_LIST_FULL_PAGE_PARAMS } from '@/constants/admin-api';

const credentialSchema = z.object({
  name: z.string().min(1, 'Name is required'),
  providerId: z.string().min(1, 'Provider is required'),
  apiKey: z.string().min(1, 'API key is required'),
  enabled: z.boolean(),
  selectionWeight: z.coerce.number().int().min(1).max(1000),
  expiresAt: z.string().optional(),
});

export function CredentialCreate() {
  const { t } = useTranslation();
  const navigate = useNavigate();

  const form = useZodForm({
    schema: credentialSchema,
    defaultValues: { name: '', providerId: '', apiKey: '', enabled: true, selectionWeight: 100, expiresAt: '' },
  });

  useUnsavedChangesWarning(form.formState.isDirty);

  const { data: providersData } = useApi<{ data: Provider[] }>(
    () => providerApi.list({ ...ADMIN_LIST_FULL_PAGE_PARAMS }),
    ['admin', 'providers', 'list', 'credential-create'],
  );

  const providerOptions = (providersData?.data ?? []).map((p) => ({
    value: p.id,
    label: p.displayName || p.name,
  }));

  const { mutate, loading } = useMutation(
    (data: CreateCredentialInput) => credentialApi.create(data),
    {
      invalidateQueries: [['api', 'admin', 'credentials']],
      onSuccess: () => navigate('/ai-gateway/credentials'),
      successMessage: t('pages:credentials.credentialCreated'),
    },
  );

  const onSubmit = form.handleSubmit((data) => {
    const payload: CreateCredentialInput = {
      name: data.name,
      providerId: data.providerId,
      apiKey: data.apiKey,
      enabled: data.enabled,
      selectionWeight: data.selectionWeight,
    };
    if (data.expiresAt) payload.expiresAt = `${data.expiresAt}T00:00:00Z`;
    void mutate(payload);
  });

  return (
    <Stack gap="md">
      <Breadcrumb items={[
        { label: t('pages:credentials.title'), to: '/ai-gateway/credentials' },
        { label: t('pages:credentials.addCredential') },
      ]} />

      <PageHeader title={t('pages:credentials.addCredential')} subtitle={t('pages:credentials.addCredentialSubtitle')} />

      <Card>
        <form onSubmit={onSubmit}>
          <Stack gap="md">
            <FormInput
              form={form}
              name="name"
              label={t('pages:credentials.name')}
              required
              helpText={t('pages:credentials.nameHelpTextCreate')}
            />
            <FormSelect
              form={form}
              name="providerId"
              label={t('pages:credentials.provider')}
              required
              helpText={t('pages:credentials.providerHelpTextCreate')}
              options={providerOptions}
              placeholder={t('pages:credentials.placeholderSelect')}
            />
            <FormInput
              form={form}
              name="apiKey"
              label={t('pages:providers.apiKeyLabel')}
              required
              helpText={t('pages:credentials.apiKeyHelpTextCreate')}
              type="password"
              placeholder={t('pages:credentials.placeholderApiKeyHint')}
            />
            <FormInput
              form={form}
              name="selectionWeight"
              label={t('pages:credentials.selectionWeightLabel')}
              helpText={t('pages:credentials.selectionWeightHelp')}
              type="number"
            />
            <FormInput
              form={form}
              name="expiresAt"
              label={t('pages:providers.credExpiresAtLabel')}
              helpText={t('pages:providers.credExpiresAtHelp')}
              type="date"
            />
            <FormField label={t('pages:credentials.enabledLabel')}>
              <Stack direction="horizontal" gap="sm" align="center">
                <Switch
                  checked={form.watch('enabled')}
                  onCheckedChange={(v) => form.setValue('enabled', v, { shouldDirty: true })}
                />
                <span>{form.watch('enabled') ? t('common:enabled') : t('common:disabled')}</span>
              </Stack>
            </FormField>

            <Stack direction="horizontal" gap="sm" justify="end">
              <Button type="button" variant="secondary" onClick={() => navigate('/ai-gateway/credentials')}>
                {t('common:cancel')}
              </Button>
              <Button
                type="submit"
                disabled={loading || !form.formState.isValid}
                loading={loading}
              >
                {t('pages:credentials.createCredential')}
              </Button>
            </Stack>
          </Stack>
        </form>
      </Card>
    </Stack>
  );
}
