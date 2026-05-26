// src/pages/setup/steps/StepOrganization.tsx
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { 
  Button,
  Card,
  Input,
  Stack
} from '@/components/ui';
import { organizationApi } from '@/api/services';
import { useToast } from '@/context/ToastContext';
import type { Organization } from '@/api/types';
import type { StepResult } from '../useSetupWizard';
import styles from '../SetupWizardPage.module.css';

interface Props {
  result: StepResult;
  onRefresh: () => void;
}

export function StepOrganization({ result, onRefresh }: Props) {
  const { t } = useTranslation();
  const { addToast } = useToast();
  const orgs = (result.data ?? []) as Organization[];

  const [name, setName] = useState('');
  const [code, setCode] = useState('');
  const [description, setDescription] = useState('');
  const [creating, setCreating] = useState(false);

  const handleCreate = async () => {
    if (!name.trim() || !code.trim()) {
      addToast(t('pages:setup.orgNameCodeRequired', 'Name and code are required'), 'error');
      return;
    }
    setCreating(true);
    try {
      await organizationApi.create({
        name: name.trim(),
        code: code.trim(),
        description: description.trim() || undefined,
      });
      addToast(t('pages:setup.orgCreated', 'Organization created'), 'success');
      onRefresh();
    } catch (e) {
      addToast((e as Error).message, 'error');
    } finally {
      setCreating(false);
    }
  };

  return (
    <div className={styles.stepContent}>
      <h2 className={styles.stepTitle}>{t('pages:setup.orgTitle', 'Create Organization')}</h2>
      <p className={styles.stepDesc}>
        {t('pages:setup.orgDesc', 'Organizations are the top-level grouping for projects, virtual keys, and quota policies. Create at least one root organization.')}
      </p>

      {result.status === 'loading' ? (
        <p className={styles.loadingText}>{t('common:loading')}</p>
      ) : result.status === 'complete' ? (
        <Card>
          <p className={styles.detectedLabel}>{t('pages:setup.detectedData', 'Detected data')}</p>
          <Stack gap="xs">
            {orgs.slice(0, 5).map((o) => (
              <div key={o.id} className={styles.summaryRow}>
                <span>{o.name}</span>
                <span className={styles.summaryCode}>({o.code})</span>
              </div>
            ))}
            {orgs.length > 5 && (
              <p className={styles.moreText}>
                {t('pages:setup.andMore', '...and {{count}} more', { count: orgs.length - 5 })}
              </p>
            )}
          </Stack>
        </Card>
      ) : (
        <Card>
          <Stack gap="sm">
            <div className={styles.formField}>
              <label className={styles.formLabel}>{t('pages:setup.orgNameLabel', 'Organization name')}</label>
              <Input className={styles.formInput} value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. Acme Corp" />
            </div>
            <div className={styles.formField}>
              <label className={styles.formLabel}>{t('pages:setup.orgCodeLabel', 'Code')}</label>
              <Input className={styles.formInput} value={code} onChange={(e) => setCode(e.target.value)} placeholder="e.g. ACME" />
            </div>
            <div className={styles.formField}>
              <label className={styles.formLabel}>{t('pages:setup.orgDescLabel', 'Description (optional)')}</label>
              <Input className={styles.formInput} value={description} onChange={(e) => setDescription(e.target.value)} />
            </div>
            <Button onClick={handleCreate} disabled={creating}>
              {creating ? t('pages:setup.creating', 'Creating...') : t('pages:setup.createOrg', 'Create Organization')}
            </Button>
          </Stack>
        </Card>
      )}
    </div>
  );
}
