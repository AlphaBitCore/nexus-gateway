// src/pages/setup/SetupWizardPage.tsx
import { useTranslation } from 'react-i18next';
import { Stack, Button, Card } from '@/components/ui';
import { useTheme } from '@/theme/useTheme';
import { useSetupWizard, STEP_IDS } from './useSetupWizard';
import { SetupStepBar } from './SetupStepBar';
import { StepHealthCheck } from './steps/StepHealthCheck';
import { StepOrganization } from './steps/StepOrganization';
import { StepProvider } from './steps/StepProvider';
import { StepProject } from './steps/StepProject';
import { StepVirtualKey } from './steps/StepVirtualKey';
import { StepRoutingRule } from './steps/StepRoutingRule';
import { StepCompliance } from './steps/StepCompliance';
import { StepSummary } from './steps/StepSummary';
import styles from './SetupWizardPage.module.css';

export { checkAllSetupComplete, checkAllSetupComplete as fetchSetupCompleted } from './useSetupWizard';

export function SetupWizardPage() {
  const { t } = useTranslation();
  const { brand } = useTheme();
  const wizard = useSetupWizard();

  if (wizard.initialLoading) {
    return (
      <div className={styles.wizardRoot}>
        <Card><p className={styles.loadingText}>{t('common:loading')}</p></Card>
      </div>
    );
  }

  const currentStepId = wizard.isOnSummary ? null : STEP_IDS[wizard.currentStep];

  const refreshCurrent = () => {
    if (currentStepId) void wizard.refreshStep(currentStepId);
  };

  return (
    <div className={styles.wizardRoot}>
      <h1 className={styles.wizardTitle}>{t('pages:setup.title', 'Setup Wizard')}</h1>
      <p className={styles.wizardSubtitle}>
        {t('pages:setup.subtitle', { productName: brand.productName })}
      </p>

      <Card className={styles.stepBarCard}>
        <SetupStepBar
          currentStep={wizard.currentStep}
          results={wizard.results}
          onStepClick={wizard.goToStep}
        />
      </Card>

      <div className={styles.stepPanel} role="tabpanel">
        {currentStepId === 'health_check' && (
          <StepHealthCheck result={wizard.results.health_check} onRefresh={refreshCurrent} />
        )}
        {currentStepId === 'organization' && (
          <StepOrganization result={wizard.results.organization} onRefresh={refreshCurrent} />
        )}
        {currentStepId === 'provider' && (
          <StepProvider result={wizard.results.provider} onRefresh={refreshCurrent} />
        )}
        {currentStepId === 'project' && (
          <StepProject result={wizard.results.project} onRefresh={refreshCurrent} />
        )}
        {currentStepId === 'virtual_key' && (
          <StepVirtualKey result={wizard.results.virtual_key} onRefresh={refreshCurrent} />
        )}
        {currentStepId === 'routing_rule' && (
          <StepRoutingRule result={wizard.results.routing_rule} onRefresh={refreshCurrent} />
        )}
        {currentStepId === 'compliance' && (
          <StepCompliance
            onSkip={wizard.skipCompliance}
            onDone={wizard.completeCompliance}
          />
        )}
        {wizard.isOnSummary && (
          <StepSummary results={wizard.results} onRestart={() => wizard.goToStep(0)} />
        )}
      </div>

      {/* Navigation buttons — hidden on summary page and compliance (has its own buttons) */}
      {!wizard.isOnSummary && currentStepId !== 'compliance' && (
        <Stack direction="horizontal" gap="sm" className={styles.navBar}>
          <Button
            variant="secondary"
            onClick={wizard.goBack}
            disabled={wizard.currentStep === 0}
          >
            {t('pages:setup.back', 'Back')}
          </Button>
          <div style={{ flex: 1 }} />
          <Button onClick={wizard.goNext}>
            {t('pages:setup.next', 'Next')}
          </Button>
        </Stack>
      )}
    </div>
  );
}
