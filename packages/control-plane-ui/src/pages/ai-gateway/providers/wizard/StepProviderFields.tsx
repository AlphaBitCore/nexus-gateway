import {
  FormField, Input, Select, Stack,
} from '@/components/ui';
import type { ProviderWizardHook } from './useProviderWizard';
import { PROVIDER_ADAPTER_TYPES } from '../_shared/adapterTypes';
import styles from './ProviderWizard.module.css';

export function StepProviderFields({ wizard }: { wizard: ProviderWizardHook }) {
  const {
    t,
    isCustom,
    name, setName,
    nameError, nameChecking,
    displayName, setDisplayName,
    baseUrl, setBaseUrl,
    adapterType, setAdapterType,
    description, setDescription,
  } = wizard;

  return (
    <div className={styles.stepPanelLarge}>
      <h2 className={styles.stepTitle}>{t('pages:providers.providerDetails', 'Provider details')}</h2>
      <p className={styles.stepSubtitle}>
        {isCustom
          ? t('pages:providers.providerDetailsCustomHint')
          : t('pages:providers.providerDetailsTemplateHint')}
      </p>
      {(displayName || isCustom) && (
        <p className={styles.templateFromLabel}>
          {isCustom ? t('pages:providers.customProvider') : t('pages:providers.fromTemplate', { name: displayName || name })}
        </p>
      )}
      <Stack gap="md" className={styles.formStack}>
        <FormField
          label={t('pages:providers.name')}
          required
          error={nameError ?? undefined}
          helpText={
            nameChecking
              ? t('pages:providers.nameChecking', 'Checking availability…')
              : nameError
                ? undefined
                : t('pages:providers.nameHelp', 'Used as the gateway routing path (/<name>/...). Must be unique.')
          }
        >
          <Input value={name} onChange={e => setName(e.target.value)} placeholder={t('pages:providers.placeholderName')} />
        </FormField>
        <FormField label={t('pages:providers.displayName')}>
          <Input value={displayName} onChange={e => setDisplayName(e.target.value)} placeholder={t('pages:providers.placeholderDisplayName')} />
        </FormField>
        <FormField label={t('pages:providers.baseUrl')} required>
          <Input value={baseUrl} onChange={e => setBaseUrl(e.target.value)} type="url" placeholder={t('pages:providers.placeholderBaseUrl')} />
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
        <FormField label={t('pages:providers.description')}>
          <Input value={description} onChange={e => setDescription(e.target.value)} placeholder={t('pages:providers.placeholderDescriptionOptional')} />
        </FormField>
      </Stack>
    </div>
  );
}
