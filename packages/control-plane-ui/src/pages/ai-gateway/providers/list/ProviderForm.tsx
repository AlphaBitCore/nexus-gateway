import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useMutation } from '@/hooks/useMutation';
import { providerApi } from '@/api/services';
import type { CreateProviderInput, UpdateProviderInput } from '@/api/services';
import type { Provider } from '@/api/types';
import { ProviderConnectivityTestButton } from './ProviderConnectivityTestButton';
import { PROVIDER_ADAPTER_TYPES } from '../_shared/adapterTypes';
import {
  Dialog, FormField, Input, Select, Switch, Tooltip, Button, Stack, Divider,
} from '@/components/ui';

interface ProviderFormProps {
  provider?: Provider;
  onClose: () => void;
  onSaved: () => void;
}

export function ProviderForm({ provider, onClose, onSaved }: ProviderFormProps) {
  const { t } = useTranslation();
  const [name, setName] = useState(provider?.name ?? '');
  const [displayName, setDisplayName] = useState(provider?.displayName ?? '');
  const [description, setDescription] = useState(provider?.description ?? '');
  const [baseUrl, setBaseUrl] = useState(provider?.baseUrl ?? '');
  const [adapterType, setAdapterType] = useState(provider?.adapterType ?? 'openai');
  const [region, setRegion] = useState(provider?.region ?? '');
  const [apiVersion, setApiVersion] = useState(provider?.apiVersion ?? '');
  const [enabled, setEnabled] = useState(provider?.enabled ?? true);

  const { mutate, loading } = useMutation(
    (data: unknown) =>
      provider
        ? providerApi.update(provider.id, data as UpdateProviderInput)
        : providerApi.create(data as CreateProviderInput),
    {
      onSuccess: () => { onSaved(); onClose(); },
      successMessage: provider
        ? t('pages:providers.providerUpdated')
        : t('pages:providers.providerCreatedSuccess'),
    },
  );

  const handleSubmit = () => {
    mutate({ name, displayName, description, baseUrl, adapterType, region: region || undefined, apiVersion: apiVersion || undefined, enabled });
  };

  return (
    <Dialog
      open
      onOpenChange={(open) => { if (!open) onClose(); }}
      title={provider ? t('pages:providers.editProvider', 'Edit Provider') : t('pages:providers.createProvider')}
      size="md"
    >
      <Stack gap="md">
        <FormField label={t('pages:providers.name')} required>
          <Input
            name="name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
          />
        </FormField>
        <FormField label={t('pages:providers.displayName')}>
          <Input
            name="displayName"
            value={displayName}
            onChange={(e) => setDisplayName(e.target.value)}
          />
        </FormField>
        <FormField label={t('pages:providers.description')}>
          <Input
            name="description"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </FormField>
        <FormField
          label={t('pages:providers.baseUrl')}
          required
          helpText={t('pages:providers.formHelpBaseUrl')}
        >
          <Input
            name="baseUrl"
            value={baseUrl}
            onChange={(e) => setBaseUrl(e.target.value)}
            required
            type="url"
            placeholder={t('pages:providers.formPlaceholderBaseUrl')}
          />
        </FormField>
        <FormField
          label={t('pages:providers.adapter')}
          required
          helpText={t('pages:providers.adapterHelp')}
        >
          <Select
            value={adapterType}
            onValueChange={setAdapterType}
            options={PROVIDER_ADAPTER_TYPES.map((v) => ({
              value: v,
              label: t(`pages:providers.adapterOption_${v}`),
            }))}
          />
        </FormField>
        <FormField
          label={t('pages:providers.region')}
          helpText={t('pages:providers.regionHelp')}
        >
          <Input
            name="region"
            value={region}
            onChange={(e) => setRegion(e.target.value)}
            placeholder={t('pages:providers.regionPlaceholder')}
          />
        </FormField>
        <FormField
          label={t('pages:providers.apiVersion')}
          helpText={t('pages:providers.apiVersionHelp')}
        >
          <Input
            name="apiVersion"
            value={apiVersion}
            onChange={(e) => setApiVersion(e.target.value)}
            placeholder={t('pages:providers.apiVersionPlaceholder')}
          />
        </FormField>
        <Stack direction="horizontal" gap="sm" align="center">
          <Switch checked={enabled} onCheckedChange={setEnabled} />
          <Tooltip content="When disabled, new traffic is not routed to this provider's models. Existing credentials and models remain stored; re-enable when the endpoint is healthy again.">
            <span>{t('common:enabled')}</span>
          </Tooltip>
        </Stack>
        <Divider />
        <div>
          {provider?.id ? (
            <ProviderConnectivityTestButton variant="existing" providerId={provider.id} />
          ) : (
            <ProviderConnectivityTestButton variant="draft" name={name} adapterType={adapterType} baseUrl={baseUrl} apiKey="" />
          )}
        </div>
        <Stack direction="horizontal" gap="sm" justify="end">
          <Button variant="secondary" onClick={onClose}>
            {t('common:cancel')}
          </Button>
          <Button
            onClick={handleSubmit}
            disabled={loading || !name || !baseUrl}
            loading={loading}
          >
            {t('common:save')}
          </Button>
        </Stack>
      </Stack>
    </Dialog>
  );
}
