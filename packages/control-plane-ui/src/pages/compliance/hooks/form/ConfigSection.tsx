import { useTranslation } from 'react-i18next';
import type { UseFormReturn } from 'react-hook-form';
import { Switch, Tooltip, Stack, FormField, Select, Textarea } from '@/components/ui';
import { FormInput } from '@/lib/forms';
import { JsonSchemaHookConfigForm } from '@/components/config/JsonSchemaHookConfigForm';
import { HelpIconButton } from '@nexus-gateway/ui-shared';
import { HOOK_ROW_TYPE } from '@/constants/hooks';
import { ImplementationSelector } from './ImplementationSelector';
import type { HookFormValues, WebhookTargetOption } from './hookFormModel';
import styles from './HookForm.module.css';

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function omitSchemaProperty(schema: Record<string, unknown> | undefined, key: string): Record<string, unknown> | undefined {
  if (!schema || !isPlainObject(schema.properties) || !isPlainObject(schema.properties[key])) return schema;
  const properties = { ...schema.properties };
  delete properties[key];
  const required = Array.isArray(schema.required)
    ? schema.required.filter((item) => String(item) !== key)
    : schema.required;
  return { ...schema, properties, required };
}

interface ConfigSectionProps {
  form: UseFormReturn<HookFormValues>;
  type: string;
  registryError: string | null;
  implSelectOptions: { value: string; label: string }[];
  selectedImplementationId: string;
  onImplementationChange: (id: string) => void;
  webhookTargetOption: WebhookTargetOption;
  setWebhookTargetOption: (option: WebhookTargetOption) => void;
  aiguardWebhookUrl: string;
  schema: Record<string, unknown> | undefined;
  useManualConfigEditor: boolean;
  setUseManualConfigEditor: (value: boolean) => void;
  configObject: Record<string, unknown>;
  setConfigObject: (value: Record<string, unknown>) => void;
  manualConfigJson: string;
  setManualConfigJson: (value: string) => void;
}

export function ConfigSection({
  form,
  type,
  registryError,
  implSelectOptions,
  selectedImplementationId,
  onImplementationChange,
  webhookTargetOption,
  setWebhookTargetOption,
  aiguardWebhookUrl,
  schema,
  useManualConfigEditor,
  setUseManualConfigEditor,
  configObject,
  setConfigObject,
  manualConfigJson,
  setManualConfigJson,
}: ConfigSectionProps) {
  const { t } = useTranslation();
  const detectRefusalsSchema = isPlainObject(schema?.properties) && isPlainObject(schema.properties.detectRefusals)
    ? schema.properties.detectRefusals
    : null;
  const configSchema = omitSchemaProperty(schema, 'detectRefusals');
  const detectRefusalsLabel = detectRefusalsSchema && typeof detectRefusalsSchema.title === 'string'
    ? detectRefusalsSchema.title
    : 'detectRefusals';
  const detectRefusalsHelp = detectRefusalsSchema && typeof detectRefusalsSchema.description === 'string'
    ? detectRefusalsSchema.description
    : undefined;

  return (
    <section className={styles.configSection}>
      <div className={styles.sectionTitle}>{t('pages:hooks.configurationSection')}</div>
      {registryError ? (
        <p className={styles.registryError}>{registryError}</p>
      ) : null}

      <div className={styles.configTopGrid}>
        <ImplementationSelector
          implSelectOptions={implSelectOptions}
          selectedImplementationId={selectedImplementationId}
          onImplementationChange={onImplementationChange}
        />

        <div className={styles.switchField}>
          <Stack direction="horizontal" gap="sm" className={styles.enabledRow}>
            <label className={styles.enabledLabel}>{t('pages:hooks.manualJsonLabel')}</label>
            <Tooltip content={t('pages:hooks.manualJsonTooltip')}>
              <HelpIconButton aria-label={t('pages:hooks.manualJsonLabel')} />
            </Tooltip>
          </Stack>
          <Switch
            checked={useManualConfigEditor}
            onCheckedChange={(c) => {
              setUseManualConfigEditor(c);
              if (c) setManualConfigJson(JSON.stringify(configObject, null, 2));
            }}
          />
        </div>

        {detectRefusalsSchema ? (
          <FormField label={detectRefusalsLabel} helpText={detectRefusalsHelp}>
            <Switch
              checked={configObject.detectRefusals === true}
              onCheckedChange={(checked) => setConfigObject({ ...configObject, detectRefusals: checked })}
            />
          </FormField>
        ) : null}
      </div>

      {type === HOOK_ROW_TYPE.WEBHOOK ? (
        <Stack gap="sm">
          <FormField
            label={t('pages:hooks.webhookTargetLabel', 'Webhook target')}
            helpText={t(
              'pages:hooks.webhookTargetHelp',
              'Choose AIGuard for the built-in compliance endpoint, or keep custom for external webhooks.',
            )}
          >
            <Select
              value={webhookTargetOption}
              onValueChange={(value) => {
                const option = value as WebhookTargetOption;
                setWebhookTargetOption(option);
                if (option === 'aiguard') {
                  form.setValue('whEndpoint', aiguardWebhookUrl, { shouldDirty: true });
                }
              }}
              options={[
                { value: 'aiguard', label: t('pages:hooks.webhookTargetAIGuard', 'AIGuard') },
                { value: 'custom', label: t('pages:hooks.webhookTargetCustom', 'Custom') },
              ]}
            />
          </FormField>
          <FormInput form={form} name="whEndpoint" label={t('pages:hooks.endpointUrlLabel')} required helpText={t('pages:hooks.endpointUrlHelp')} type="url" placeholder={t('pages:hooks.endpointUrlPlaceholder')} />
        </Stack>
      ) : null}

      {configSchema && !useManualConfigEditor ? (
        <JsonSchemaHookConfigForm schema={configSchema} value={configObject} onChange={setConfigObject} />
      ) : (
        <FormField label={t('pages:hooks.configJsonLabel')}>
          <Textarea
            name="manual-config-json"
            value={manualConfigJson}
            onChange={(e) => setManualConfigJson(e.target.value)}
            className={styles.monoTextarea}
          />
        </FormField>
      )}
    </section>
  );
}
