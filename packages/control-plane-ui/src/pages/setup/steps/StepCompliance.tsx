// src/pages/setup/steps/StepCompliance.tsx
import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import { Stack, Card, Button } from '@/components/ui';
import styles from '../SetupWizardPage.module.css';

interface Props {
  onSkip: () => void;
  onDone: () => void;
}

export function StepCompliance({ onSkip, onDone }: Props) {
  const { t } = useTranslation();

  const items = [
    {
      label: t('pages:setup.complianceHooks', 'Compliance Hooks'),
      desc: t('pages:setup.complianceHooksDesc', 'PII redaction, content safety, and quality checks on requests and responses.'),
      to: '/compliance/hooks',
    },
    {
      label: t('pages:setup.complianceQuota', 'Quota Policies'),
      desc: t('pages:setup.complianceQuotaDesc', 'Budget and token limits for users, projects, and organizations.'),
      to: '/ai-gateway/quota-policies',
    },
    {
      label: t('pages:setup.complianceProxy', 'Compliance Proxy'),
      desc: t('pages:setup.complianceProxyDesc', 'Transparent TLS proxy for enterprise traffic interception.'),
      to: '/infrastructure/proxy-rollout',
    },
  ];

  return (
    <div className={styles.stepContent}>
      <h2 className={styles.stepTitle}>{t('pages:setup.complianceTitle', 'Configure Compliance')}</h2>
      <p className={styles.stepDesc}>
        {t('pages:setup.complianceDesc', 'Optional compliance features. You can configure these now or skip and set them up later.')}
      </p>

      <Stack gap="sm">
        {items.map((item) => (
          <Card key={item.to}>
            <Stack direction="horizontal" gap="md" align="center">
              <div style={{ flex: 1 }}>
                <div className={styles.summaryRow}><strong>{item.label}</strong></div>
                <p className={styles.hintText}>{item.desc}</p>
              </div>
              <Link to={item.to}>
                <Button variant="ghost" size="sm">{t('pages:setup.openPage', 'Open')}</Button>
              </Link>
            </Stack>
          </Card>
        ))}
      </Stack>

      <Stack direction="horizontal" gap="sm" className={styles.complianceActions}>
        <Button variant="secondary" onClick={onSkip}>{t('pages:setup.skip', 'Skip')}</Button>
        <Button onClick={onDone}>{t('pages:setup.done', 'Done')}</Button>
      </Stack>
    </div>
  );
}
