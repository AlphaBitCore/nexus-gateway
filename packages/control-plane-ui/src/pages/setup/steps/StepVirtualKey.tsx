// src/pages/setup/steps/StepVirtualKey.tsx
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { 
  Button,
  Card,
  Input,
  Stack
} from '@/components/ui';
import { virtualKeyApi } from '@/api/services';
import { useToast } from '@/context/ToastContext';
import type { VirtualKey } from '@/api/types';
import type { StepResult } from '../useSetupWizard';
import styles from '../SetupWizardPage.module.css';

interface Props {
  result: StepResult;
  onRefresh: () => void;
}

export function StepVirtualKey({ result, onRefresh }: Props) {
  const { t } = useTranslation();
  const { addToast } = useToast();
  const keys = (result.data ?? []) as VirtualKey[];

  const [name, setName] = useState('');
  const [creating, setCreating] = useState(false);

  const handleCreate = async () => {
    if (!name.trim()) {
      addToast(t('pages:setup.vkNameRequired', 'Slug is required'), 'error');
      return;
    }
    setCreating(true);
    try {
      await virtualKeyApi.create({ name: name.trim(), enabled: true });
      addToast(t('pages:setup.vkCreated', 'Virtual key created'), 'success');
      onRefresh();
    } catch (e) {
      addToast((e as Error).message, 'error');
    } finally {
      setCreating(false);
    }
  };

  return (
    <div className={styles.stepContent}>
      <h2 className={styles.stepTitle}>{t('pages:setup.vkTitle', 'Issue Virtual Key')}</h2>
      <p className={styles.stepDesc}>
        {t('pages:setup.vkDesc', 'Virtual keys are how clients authenticate to the gateway. Issue one for each project or team that needs access.')}
      </p>

      {result.status === 'loading' ? (
        <p className={styles.loadingText}>{t('common:loading')}</p>
      ) : result.status === 'complete' ? (
        <Card>
          <p className={styles.detectedLabel}>{t('pages:setup.detectedData', 'Detected data')}</p>
          <Stack gap="xs">
            {keys.slice(0, 5).map((k) => (
              <div key={k.id} className={styles.summaryRow}>
                <span>{k.name}</span>
                <span className={styles.summaryCode}>
                  {k.project ? `· ${k.project.name ?? k.projectId}` : ''}
                </span>
              </div>
            ))}
            {keys.length > 5 && (
              <p className={styles.moreText}>{t('pages:setup.andMore', '...and {{count}} more', { count: keys.length - 5 })}</p>
            )}
          </Stack>
        </Card>
      ) : (
        <Card>
          <Stack gap="sm">
            <div className={styles.formField}>
              <label className={styles.formLabel}>{t('pages:setup.vkNameLabel', 'Virtual key name')}</label>
              <Input className={styles.formInput} value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. my-app-key" />
            </div>
            <p className={styles.hintText}>
              {t('pages:setup.vkHint', 'Creates an enabled key with default settings. Configure rate limits and allowed models later.')}
            </p>
            <Button onClick={handleCreate} disabled={creating}>
              {creating ? t('pages:setup.creating', 'Creating...') : t('pages:setup.createVk', 'Create Virtual Key')}
            </Button>
          </Stack>
        </Card>
      )}
    </div>
  );
}
