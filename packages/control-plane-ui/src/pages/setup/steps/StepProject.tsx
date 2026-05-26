// src/pages/setup/steps/StepProject.tsx
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { 
  Button,
  Card,
  Input,
  OrgTreeSelect,
  Stack
} from '@/components/ui';
import { projectApi } from '@/api/services';
import { useToast } from '@/context/ToastContext';
import type { Project } from '@/api/types';
import type { StepResult } from '../useSetupWizard';
import styles from '../SetupWizardPage.module.css';

interface Props {
  result: StepResult;
  onRefresh: () => void;
}

export function StepProject({ result, onRefresh }: Props) {
  const { t } = useTranslation();
  const { addToast } = useToast();
  const projects = (result.data ?? []) as Project[];

  const [name, setName] = useState('');
  const [code, setCode] = useState('');
  const [orgId, setOrgId] = useState('');
  const [creating, setCreating] = useState(false);

  const handleCreate = async () => {
    if (!name.trim() || !code.trim()) {
      addToast(t('pages:setup.projectNameCodeRequired', 'Name and code are required'), 'error');
      return;
    }
    setCreating(true);
    try {
      await projectApi.create({
        name: name.trim(),
        code: code.trim(),
        organizationId: orgId || undefined,
      });
      addToast(t('pages:setup.projectCreated', 'Project created'), 'success');
      onRefresh();
    } catch (e) {
      addToast((e as Error).message, 'error');
    } finally {
      setCreating(false);
    }
  };

  return (
    <div className={styles.stepContent}>
      <h2 className={styles.stepTitle}>{t('pages:setup.projectTitle', 'Create Project')}</h2>
      <p className={styles.stepDesc}>
        {t('pages:setup.projectDesc', 'Projects group virtual keys and policies under an organization. Create at least one project to scope client access.')}
      </p>

      {result.status === 'loading' ? (
        <p className={styles.loadingText}>{t('common:loading')}</p>
      ) : result.status === 'complete' ? (
        <Card>
          <p className={styles.detectedLabel}>{t('pages:setup.detectedData', 'Detected data')}</p>
          <Stack gap="xs">
            {projects.slice(0, 5).map((p) => (
              <div key={p.id} className={styles.summaryRow}>
                <span>{p.name}</span>
                <span className={styles.summaryCode}>({p.code}){p.organization ? ` · ${p.organization.name}` : ''}</span>
              </div>
            ))}
            {projects.length > 5 && (
              <p className={styles.moreText}>{t('pages:setup.andMore', '...and {{count}} more', { count: projects.length - 5 })}</p>
            )}
          </Stack>
        </Card>
      ) : (
        <Card>
          <Stack gap="sm">
            <div className={styles.formField}>
              <label className={styles.formLabel}>{t('pages:setup.projectNameLabel', 'Project name')}</label>
              <Input className={styles.formInput} value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. Platform API" />
            </div>
            <div className={styles.formField}>
              <label className={styles.formLabel}>{t('pages:setup.projectCodeLabel', 'Code')}</label>
              <Input className={styles.formInput} value={code} onChange={(e) => setCode(e.target.value)} placeholder="e.g. PLATFORM" />
            </div>
            <div className={styles.formField}>
              <label className={styles.formLabel}>{t('pages:setup.projectOrgLabel', 'Organization')}</label>
              <OrgTreeSelect value={orgId} onChange={(v) => setOrgId(v as string)} allowClear />
            </div>
            <Button onClick={handleCreate} disabled={creating}>
              {creating ? t('pages:setup.creating', 'Creating...') : t('pages:setup.createProject', 'Create Project')}
            </Button>
          </Stack>
        </Card>
      )}
    </div>
  );
}
