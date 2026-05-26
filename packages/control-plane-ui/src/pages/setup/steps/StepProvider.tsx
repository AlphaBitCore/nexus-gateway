// src/pages/setup/steps/StepProvider.tsx
import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { 
  Button,
  Card,
  Input,
  Stack
} from '@/components/ui';
import { providerApi, credentialApi } from '@/api/services';
import { useToast } from '@/context/ToastContext';
import type { ApiProviderTemplate } from '@/api/types';
import type { StepResult, ProviderStepData } from '../useSetupWizard';
import styles from '../SetupWizardPage.module.css';

interface Props {
  result: StepResult;
  onRefresh: () => void;
}

export function StepProvider({ result, onRefresh }: Props) {
  const { t } = useTranslation();
  const { addToast } = useToast();
  const stepData = result.data as ProviderStepData | undefined;
  const providers = stepData?.providers ?? [];
  const credentials = stepData?.credentials ?? [];

  const [templates, setTemplates] = useState<ApiProviderTemplate[]>([]);
  const [selectedTemplate, setSelectedTemplate] = useState('');
  const [customName, setCustomName] = useState('');
  const [customBaseUrl, setCustomBaseUrl] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [creating, setCreating] = useState(false);

  useEffect(() => {
    providerApi.getTemplates().then((res) => setTemplates(res.data ?? [])).catch(() => {});
  }, []);

  const isCustom = selectedTemplate === '_custom';
  const activeTemplate = templates.find((tpl) => tpl.name === selectedTemplate);

  const handleCreate = async () => {
    if (!isCustom && !selectedTemplate) {
      addToast(t('pages:setup.selectTemplate', 'Select a provider template'), 'error');
      return;
    }
    if (isCustom && (!customName.trim() || !customBaseUrl.trim())) {
      addToast(t('pages:setup.providerNameUrlRequired', 'Name and Base URL are required'), 'error');
      return;
    }
    if (!apiKey.trim()) {
      addToast(t('pages:setup.apiKeyRequired', 'API Key is required'), 'error');
      return;
    }

    setCreating(true);
    try {
      let providerId: string;
      if (isCustom) {
        const p = await providerApi.create({
          name: customName.trim(),
          baseUrl: customBaseUrl.trim(),
          adapterType: 'openai',
          enabled: true,
        });
        providerId = p.id;
      } else {
        // Template catalog is static JSON under public/provider-templates/;
        // we already have the meta fields in `activeTemplate`, so create the
        // provider directly. (Setup intentionally skips registering the model
        // catalog — that's surfaced in the full wizard, not this setup step.)
        if (!activeTemplate) throw new Error('template meta missing');
        const p = await providerApi.create({
          name: activeTemplate.name,
          displayName: activeTemplate.displayName,
          description: activeTemplate.description,
          baseUrl: activeTemplate.baseUrl,
          adapterType: activeTemplate.adapterType,
          enabled: true,
        });
        providerId = p.id;
      }
      await credentialApi.create({
        name: `${isCustom ? customName.trim() : activeTemplate?.displayName ?? selectedTemplate} API Key`,
        providerId,
        apiKey: apiKey.trim(),
        enabled: true,
      });
      addToast(t('pages:setup.providerCreated', 'Provider and credential created'), 'success');
      onRefresh();
    } catch (e) {
      addToast((e as Error).message, 'error');
    } finally {
      setCreating(false);
    }
  };

  return (
    <div className={styles.stepContent}>
      <h2 className={styles.stepTitle}>{t('pages:setup.providerTitle', 'Connect AI Provider')}</h2>
      <p className={styles.stepDesc}>
        {t('pages:setup.providerDesc', 'The gateway needs at least one enabled provider with an API key (credential) to route traffic to upstream AI services.')}
      </p>

      {result.status === 'loading' ? (
        <p className={styles.loadingText}>{t('common:loading')}</p>
      ) : result.status === 'complete' ? (
        <Card>
          <p className={styles.detectedLabel}>{t('pages:setup.detectedData', 'Detected data')}</p>
          <Stack gap="xs">
            {providers.filter((p) => p.enabled).map((p) => {
              const credCount = credentials.filter((c) => c.providerId === p.id).length;
              return (
                <div key={p.id} className={styles.summaryRow}>
                  <span>{p.displayName || p.name}</span>
                  <span className={styles.summaryCode}>
                    {t('pages:setup.credentialCount', '{{count}} credential(s)', { count: credCount })}
                  </span>
                </div>
              );
            })}
          </Stack>
        </Card>
      ) : (
        <Card>
          <Stack gap="sm">
            <div className={styles.formField}>
              <label className={styles.formLabel}>{t('pages:setup.templateLabel', 'Provider template')}</label>
              <select
                className={styles.formSelect}
                value={selectedTemplate}
                onChange={(e) => setSelectedTemplate(e.target.value)}
              >
                <option value="">{t('pages:setup.selectTemplateOption', '-- Select --')}</option>
                {templates.map((tmpl) => (
                  <option key={tmpl.name} value={tmpl.name}>
                    {tmpl.displayName} ({tmpl.modelCount} models)
                  </option>
                ))}
                <option value="_custom">{t('pages:setup.customProvider', 'Custom provider')}</option>
              </select>
            </div>

            {isCustom && (
              <>
                <div className={styles.formField}>
                  <label className={styles.formLabel}>{t('pages:setup.providerNameLabel', 'Provider name')}</label>
                  <Input className={styles.formInput} value={customName} onChange={(e) => setCustomName(e.target.value)} />
                </div>
                <div className={styles.formField}>
                  <label className={styles.formLabel}>{t('pages:setup.providerBaseUrlLabel', 'Base URL')}</label>
                  <Input className={styles.formInput} value={customBaseUrl} onChange={(e) => setCustomBaseUrl(e.target.value)} placeholder="https://api.example.com" />
                </div>
              </>
            )}

            <div className={styles.formField}>
              <label className={styles.formLabel}>{t('pages:setup.apiKeyLabel', 'API Key')}</label>
              <Input className={styles.formInput} type="password" value={apiKey} onChange={(e) => setApiKey(e.target.value)} />
            </div>

            <Button onClick={handleCreate} disabled={creating}>
              {creating ? t('pages:setup.creating', 'Creating...') : t('pages:setup.createProvider', 'Create Provider')}
            </Button>
          </Stack>
        </Card>
      )}
    </div>
  );
}
