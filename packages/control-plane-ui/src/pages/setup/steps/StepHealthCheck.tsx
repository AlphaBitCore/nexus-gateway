// src/pages/setup/steps/StepHealthCheck.tsx
import { useTranslation } from 'react-i18next';
import { Stack, Card, Badge } from '@/components/ui';
import type { StepResult, HealthCheckData } from '../useSetupWizard';
import styles from '../SetupWizardPage.module.css';

interface Props {
  result: StepResult;
  onRefresh: () => void;
}

export function StepHealthCheck({ result, onRefresh }: Props) {
  const { t } = useTranslation();
  const data = result.data as HealthCheckData | undefined;
  const checks = data?.checks ?? {};

  return (
    <div className={styles.stepContent}>
      <h2 className={styles.stepTitle}>{t('pages:setup.healthCheckTitle', 'System Health Check')}</h2>
      <p className={styles.stepDesc}>
        {t('pages:setup.healthCheckDesc', 'Verify that the gateway and its backend dependencies (database, Redis) are healthy before proceeding.')}
      </p>

      {result.status === 'loading' ? (
        <p className={styles.loadingText}>{t('common:loading')}</p>
      ) : result.status === 'error' ? (
        <Card>
          <Stack gap="sm">
            <p className={styles.errorText}>{t('pages:setup.healthCheckError', 'Could not reach the gateway.')}</p>
            <button type="button" className={styles.retryBtn} onClick={onRefresh}>
              {t('common:retry')}
            </button>
          </Stack>
        </Card>
      ) : (
        <Card>
          <Stack gap="sm">
            <div className={styles.summaryRow}>
              <span>{t('pages:setup.overallStatus', 'Overall')}</span>
              <Badge variant={data?.status === 'ready' ? 'success' : 'warning'}>
                {data?.status ?? 'unknown'}
              </Badge>
            </div>
            {Object.entries(checks).map(([name, status]) => (
              <div key={name} className={styles.summaryRow}>
                <span>{name}</span>
                <Badge variant={status === 'ok' || status === 'ready' ? 'success' : 'danger'}>
                  {status}
                </Badge>
              </div>
            ))}
          </Stack>
        </Card>
      )}
    </div>
  );
}
