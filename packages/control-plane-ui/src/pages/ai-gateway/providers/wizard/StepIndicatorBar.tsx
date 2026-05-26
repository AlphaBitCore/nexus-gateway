import clsx from 'clsx';
import { STEP_KEYS } from './types';
import styles from './ProviderWizard.module.css';

 
export function StepIndicatorBar({ current, t }: { current: number; t: any }) {
  const steps = STEP_KEYS.map((k) => t(`pages:providers.${k}`));
  return (
    <div className={styles.stepBar}>
      {steps.map((label, i) => {
        const isActive = i === current;
        const isCompleted = i < current;

        return (
          <div key={label} className={styles.stepItem}>
            <div className={styles.stepContent}>
              <div
                className={clsx(
                  styles.stepCircle,
                  isActive && styles.stepCircleActive,
                  isCompleted && styles.stepCircleCompleted,
                  !isActive && !isCompleted && styles.stepCirclePending,
                )}
              >
                {isCompleted ? (
                  <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
                    <path d="M20 6L9 17l-5-5" />
                  </svg>
                ) : (
                  i + 1
                )}
              </div>
              <span
                className={clsx(
                  isActive && styles.stepLabelActiveColor,
                  isCompleted && styles.stepLabelCompleted,
                  !isActive && !isCompleted && styles.stepLabelMuted,
                )}
              >
                {label}
              </span>
            </div>
            {i < steps.length - 1 && (
              <div
                className={clsx(
                  styles.stepConnector,
                  isCompleted ? styles.stepConnectorCompleted : styles.stepConnectorPending,
                )}
              />
            )}
          </div>
        );
      })}
    </div>
  );
}
