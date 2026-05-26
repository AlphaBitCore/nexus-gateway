// src/pages/setup/steps/StepRoutingRule.tsx
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { 
  Button,
  Card,
  Input,
  Stack
} from '@/components/ui';
import { routingApi } from '@/api/services';
import { useToast } from '@/context/ToastContext';
import type { RoutingRule } from '@/api/types';
import type { StepResult } from '../useSetupWizard';
import styles from '../SetupWizardPage.module.css';

interface Props {
  result: StepResult;
  onRefresh: () => void;
}

export function StepRoutingRule({ result, onRefresh }: Props) {
  const { t } = useTranslation();
  const { addToast } = useToast();
  const rules = (result.data ?? []) as RoutingRule[];

  const [name, setName] = useState('Default Route');
  const [creating, setCreating] = useState(false);

  const handleCreate = async () => {
    if (!name.trim()) {
      addToast(t('pages:setup.routingNameRequired', 'Name is required'), 'error');
      return;
    }
    setCreating(true);
    try {
      await routingApi.create({
        name: name.trim(),
        strategyType: 'single_provider',
        config: {},
        priority: 100,
        enabled: true,
      });
      addToast(t('pages:setup.routingCreated', 'Routing rule created'), 'success');
      onRefresh();
    } catch (e) {
      addToast((e as Error).message, 'error');
    } finally {
      setCreating(false);
    }
  };

  return (
    <div className={styles.stepContent}>
      <h2 className={styles.stepTitle}>{t('pages:setup.routingTitle', 'Define Routing Rule')}</h2>
      <p className={styles.stepDesc}>
        {t('pages:setup.routingDesc', 'Routing rules determine how requests reach AI providers. Create a default catch-all rule to get started.')}
      </p>

      {result.status === 'loading' ? (
        <p className={styles.loadingText}>{t('common:loading')}</p>
      ) : result.status === 'complete' ? (
        <Card>
          <p className={styles.detectedLabel}>{t('pages:setup.detectedData', 'Detected data')}</p>
          <Stack gap="xs">
            {rules.slice(0, 5).map((r) => (
              <div key={r.id} className={styles.summaryRow}>
                <span>{r.name}</span>
                <span className={styles.summaryCode}>
                  {r.strategyType} · priority {r.priority}
                </span>
              </div>
            ))}
            {rules.length > 5 && (
              <p className={styles.moreText}>{t('pages:setup.andMore', '...and {{count}} more', { count: rules.length - 5 })}</p>
            )}
          </Stack>
        </Card>
      ) : (
        <Card>
          <Stack gap="sm">
            <div className={styles.formField}>
              <label className={styles.formLabel}>{t('pages:setup.routingNameLabel', 'Rule name')}</label>
              <Input className={styles.formInput} value={name} onChange={(e) => setName(e.target.value)} />
            </div>
            <p className={styles.hintText}>
              {t('pages:setup.routingHint', 'Creates a single-provider routing rule at priority 100. Refine the strategy on the Routing Rules page after setup.')}
            </p>
            <Button onClick={handleCreate} disabled={creating}>
              {creating ? t('pages:setup.creating', 'Creating...') : t('pages:setup.createRouting', 'Create Routing Rule')}
            </Button>
          </Stack>
        </Card>
      )}
    </div>
  );
}
