// src/pages/setup/steps/StepSummary.tsx
import { useTranslation } from 'react-i18next';
import { useNavigate } from 'react-router-dom';
import { Stack, Card, Button, Badge } from '@/components/ui';
import { useTheme } from '@/theme/useTheme';
import { type StepId, type StepResult, type ProviderStepData } from '../useSetupWizard';
import type { Organization, Project, VirtualKey, RoutingRule } from '@/api/types';
import styles from '../SetupWizardPage.module.css';

interface Props {
  results: Record<StepId, StepResult>;
  onRestart: () => void;
}

interface SummaryRow {
  label: string;
  value: string;
  status: 'success' | 'warning' | 'default';
}

export function StepSummary({ results, onRestart }: Props) {
  const { t } = useTranslation();
  const { brand } = useTheme();
  const navigate = useNavigate();

  const orgs = (results.organization.data ?? []) as Organization[];
  const provData = results.provider.data as ProviderStepData | undefined;
  const providers = provData?.providers?.filter((p) => p.enabled) ?? [];
  const credentials = provData?.credentials ?? [];
  const projects = (results.project.data ?? []) as Project[];
  const vks = (results.virtual_key.data ?? []) as VirtualKey[];
  const rules = (results.routing_rule.data ?? []) as RoutingRule[];

  const rows: SummaryRow[] = [
    {
      label: t('pages:setup.summaryOrgs', 'Organizations'),
      value: orgs.length > 0 ? `${orgs.length} (${orgs.slice(0, 3).map((o) => o.name).join(', ')}${orgs.length > 3 ? '...' : ''})` : t('pages:setup.none', 'None'),
      status: orgs.length > 0 ? 'success' : 'warning',
    },
    {
      label: t('pages:setup.summaryProviders', 'Providers'),
      value: providers.length > 0 ? `${providers.length} (${providers.slice(0, 3).map((p) => p.displayName || p.name).join(', ')}${providers.length > 3 ? '...' : ''})` : t('pages:setup.none', 'None'),
      status: providers.length > 0 ? 'success' : 'warning',
    },
    {
      label: t('pages:setup.summaryCredentials', 'Credentials'),
      value: String(credentials.length),
      status: credentials.length > 0 ? 'success' : 'warning',
    },
    {
      label: t('pages:setup.summaryProjects', 'Projects'),
      value: projects.length > 0 ? `${projects.length} (${projects.slice(0, 3).map((p) => p.name).join(', ')}${projects.length > 3 ? '...' : ''})` : t('pages:setup.none', 'None'),
      status: projects.length > 0 ? 'success' : 'warning',
    },
    {
      label: t('pages:setup.summaryVks', 'Virtual Keys'),
      value: String(vks.length),
      status: vks.length > 0 ? 'success' : 'warning',
    },
    {
      label: t('pages:setup.summaryRouting', 'Routing Rules'),
      value: rules.length > 0 ? `${rules.length} (${rules.slice(0, 3).map((r) => r.name).join(', ')}${rules.length > 3 ? '...' : ''})` : t('pages:setup.none', 'None'),
      status: rules.length > 0 ? 'success' : 'warning',
    },
    {
      label: t('pages:setup.summaryCompliance', 'Compliance'),
      value: results.compliance.status === 'skipped'
        ? t('pages:setup.skipped', 'Skipped')
        : t('pages:setup.configured', 'Configured'),
      status: 'default',
    },
  ];

  return (
    <div className={styles.stepContent}>
      <h2 className={styles.stepTitle}>{t('pages:setup.summaryTitle', 'Setup Complete')}</h2>
      <p className={styles.stepDesc}>
        {t('pages:setup.summaryDesc', { productName: brand.productName })}
      </p>

      <Card>
        <Stack gap="sm">
          {rows.map((row) => (
            <div key={row.label} className={styles.summaryRow}>
              <span>{row.label}</span>
              <Badge variant={row.status}>{row.value}</Badge>
            </div>
          ))}
        </Stack>
      </Card>

      <Stack direction="horizontal" gap="sm" className={styles.summaryActions}>
        <Button variant="secondary" onClick={onRestart}>
          {t('pages:setup.restartWizard', 'Restart Wizard')}
        </Button>
        <Button onClick={() => navigate('/')}>
          {t('pages:setup.goToDashboard', 'Go to Dashboard')}
        </Button>
      </Stack>
    </div>
  );
}
