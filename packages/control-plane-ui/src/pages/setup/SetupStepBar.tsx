// src/pages/setup/SetupStepBar.tsx
import { useTranslation } from 'react-i18next';
import clsx from 'clsx';
import { STEP_IDS, TOTAL_STEPS, type StepId, type StepStatus } from './useSetupWizard';
import styles from './SetupWizardPage.module.css';

const STEP_LABEL_KEYS: Record<StepId, string> = {
  health_check: 'pages:setup.stepHealthCheck',
  organization: 'pages:setup.stepOrganization',
  provider: 'pages:setup.stepProvider',
  project: 'pages:setup.stepProject',
  virtual_key: 'pages:setup.stepVirtualKey',
  routing_rule: 'pages:setup.stepRoutingRule',
  compliance: 'pages:setup.stepCompliance',
};

interface SetupStepBarProps {
  currentStep: number;
  results: Record<StepId, { status: StepStatus }>;
  onStepClick: (index: number) => void;
}

export function SetupStepBar({ currentStep, results, onStepClick }: SetupStepBarProps) {
  const { t } = useTranslation();
  const isOnSummary = currentStep === TOTAL_STEPS;

  return (
    <nav className={styles.stepBar} role="tablist" aria-label={t('pages:setup.stepsLabel', 'Setup steps')}>
      {STEP_IDS.map((id, idx) => {
        const status = results[id].status;
        const isCurrent = idx === currentStep;
        const isDone = status === 'complete' || status === 'skipped';
        const isClickable = isDone || idx <= currentStep;

        return (
          <button
            key={id}
            type="button"
            role="tab"
            aria-selected={isCurrent}
            className={clsx(
              styles.stepBarItem,
              isCurrent && styles.stepBarItemCurrent,
              isDone && styles.stepBarItemDone,
              !isClickable && styles.stepBarItemDisabled,
            )}
            onClick={() => isClickable && onStepClick(idx)}
            disabled={!isClickable}
          >
            <span className={styles.stepBarCircle}>
              {isDone ? '\u2713' : idx + 1}
            </span>
            <span className={styles.stepBarLabel}>
              {t(STEP_LABEL_KEYS[id])}
            </span>
          </button>
        );
      })}
      {/* Summary pseudo-step */}
      <button
        type="button"
        role="tab"
        aria-selected={isOnSummary}
        className={clsx(
          styles.stepBarItem,
          isOnSummary && styles.stepBarItemCurrent,
        )}
        onClick={() => onStepClick(TOTAL_STEPS)}
        disabled={!isOnSummary}
      >
        <span className={styles.stepBarCircle}>
          {isOnSummary ? '\u2713' : TOTAL_STEPS + 1}
        </span>
        <span className={styles.stepBarLabel}>
          {t('pages:setup.stepSummary', 'Summary')}
        </span>
      </button>
    </nav>
  );
}
