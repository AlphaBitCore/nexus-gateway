import { ErrorBanner, Breadcrumb, Button } from '@/components/ui';
import { useProviderWizard } from './useProviderWizard';
import { StepIndicatorBar } from './StepIndicatorBar';
import { StepTemplate } from './StepTemplate';
import { StepProviderFields } from './StepProviderFields';
import { StepCredential } from './StepCredential';
import { StepModels } from './StepModels';
import { StepReview } from './StepReview';
import styles from './ProviderWizard.module.css';

export function ProviderWizard() {
  const wizard = useProviderWizard();
  const { t, step, error, clearError, submitting, canNext, goBack, goNext, handleSubmit } = wizard;

  return (
    <div className={styles.wizardRoot}>
      <div className={styles.wizardContent}>
        <Breadcrumb items={[
          { label: t('pages:providers.title', 'Providers'), to: '/ai-gateway/providers' },
          { label: t('pages:providers.wizardTitle', 'Connect an AI provider') },
        ]} />

        <header className={styles.headerSection}>
          <p className={styles.headerTag}>{t('pages:providers.wizardTag', 'Configuration')}</p>
          <h1 className={styles.headerTitle}>{t('pages:providers.wizardTitle', 'Connect an AI provider')}</h1>
          <p className={styles.headerSubtitle}>
            {t('pages:providers.wizardSubtitle', 'Pick a template or custom first, confirm provider fields, then add a credential. Use custom if your API is not listed.')}
          </p>
        </header>

        <StepIndicatorBar current={step} t={t} />

        {error && (
          <div className={styles.errorWrapper}>
            <ErrorBanner message={error} onRetry={clearError} />
          </div>
        )}

        {step === 0 && <StepTemplate wizard={wizard} />}
        {step === 1 && <StepProviderFields wizard={wizard} />}
        {step === 2 && <StepCredential wizard={wizard} />}
        {step === 3 && <StepModels wizard={wizard} />}
        {step === 4 && <StepReview wizard={wizard} />}

        <div className={styles.wizardFooter}>
          <Button
            variant="secondary"
            onClick={goBack}
          >
            {step === 0 ? t('common:cancel') : t('common:back')}
          </Button>
          {step < 4 ? (
            <Button onClick={goNext} disabled={!canNext()}>
              {t('pages:providers.continue', 'Continue')}
            </Button>
          ) : (
            <Button onClick={handleSubmit} disabled={submitting} className={styles.submitBtn}>
              {submitting ? t('pages:providers.creating', 'Creating\u2026') : t('pages:providers.createProvider')}
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}
