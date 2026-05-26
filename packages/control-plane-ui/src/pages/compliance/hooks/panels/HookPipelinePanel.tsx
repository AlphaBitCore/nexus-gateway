import { useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import clsx from 'clsx';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { hookApi } from '@/api/services';
import { LoadingSpinner, ErrorBanner } from '@/components/ui';
import type { HookCategory, HookChainStep, HookExecutionChain, HookReorderResponse } from '@/api/types';
import {
  HOOK_CATEGORY,
  HOOK_EXECUTION_FLOW_KIND,
  type HookStage,
} from '@/constants/hooks';
import styles from './HookPipelinePanel.module.css';

function categoryChipClass(category: HookCategory, s: Record<string, string>): string {
  switch (category) {
    case HOOK_CATEGORY.COMPLIANCE:      return s.chipCategoryCompliance;
    case HOOK_CATEGORY.TRAFFIC_CONTROL: return s.chipCategoryTrafficControl;
    case HOOK_CATEGORY.QUALITY:         return s.chipCategoryQuality;
    case HOOK_CATEGORY.OBSERVABILITY:   return s.chipCategoryObservability;
    default:                            return s.chipCategoryDefault;
  }
}

function ReorderButtons({
  phase,
  steps,
  index,
  disabled,
  onApplyIds,
}: {
  phase: HookStage;
  steps: HookChainStep[];
  index: number;
  disabled: boolean;
  onApplyIds: (payload: { stage: HookStage; ids: string[] }) => void;
}) {
  const applySwap = (a: number, b: number) => {
    const ids = steps.map((s) => s.hookConfigId);
    const t = ids[a];
    ids[a] = ids[b]!;
    ids[b] = t!;
    onApplyIds({ stage: phase, ids });
  };

  const { t } = useTranslation();
  return (
    <div className={styles.reorderCol}>
      <button
        type="button"
        disabled={disabled || index === 0}
        onClick={() => applySwap(index, index - 1)}
        className={styles.reorderBtn}
      >
        {t('pages:hooks.reorderUp')}
      </button>
      <button
        type="button"
        disabled={disabled || index >= steps.length - 1}
        onClick={() => applySwap(index, index + 1)}
        className={styles.reorderBtn}
      >
        {t('pages:hooks.reorderDown')}
      </button>
    </div>
  );
}

function StepCard({ step }: { step: HookChainStep }) {
  const { t } = useTranslation();
  return (
    <div className={step.enabled ? styles.stepCardEnabled : styles.stepCardDisabled}>
      <div className={styles.stepHeader}>
        <span className={styles.stepOrder}>#{step.order}</span>
        <span className={styles.stepName}>{step.name}</span>
        <span className={clsx(styles.chip, categoryChipClass(step.classification.category, styles))}>
          {step.classification.categoryLabel}
        </span>
        <span className={styles.phaseChip}>
          {step.classification.phaseLabel}
        </span>
        {!step.wired && (
          <span className={styles.notWiredChip}>{t('pages:hooks.pipelineNotWired')}</span>
        )}
        <span className={styles.stepMeta}>
          {t('pages:hooks.pipelinePriority')} {step.priority} &middot; {step.executionMode}
        </span>
      </div>
      {step.classification.implementationLabel && (
        <div className={styles.stepImpl}>
          {t('pages:hooks.pipelineImplementation')} {step.classification.implementationLabel}
          {step.classification.dualPhaseCapable && (
            <span className={styles.dualPhaseNote}>
              &middot; {t('pages:hooks.pipelineDualPhaseCapable')}
            </span>
          )}
        </div>
      )}
    </div>
  );
}

export function HookPipelinePanel() {
  const { t } = useTranslation();
  const { data, loading, error, refetch } = useApi<HookExecutionChain>(
    () => hookApi.getExecutionChain(),
    ['admin', 'hooks', 'execution-chain'],
  );

  const reorderFn = useCallback(
    (input: { stage: HookStage; ids: string[] }) =>
      hookApi.reorder({ stage: input.stage, ids: input.ids }) as Promise<HookReorderResponse>,
    [],
  );

  const { mutate: reorderHooks, loading: reorderLoading } = useMutation(reorderFn, {
    invalidateQueries: [['api', 'admin', 'hooks']],
    successMessage: t('pages:hooks.reorderSuccess'),
  });

  if (loading) return <LoadingSpinner />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!data) return null;

  return (
    <div className={styles.panel}>
      <h2 className={styles.title}>{t('pages:hooks.executionChain', 'Execution chain')}</h2>
      <p className={styles.subtitle}>
        {t('pages:hooks.pipelineDescription')}
      </p>
      <p className={styles.summary}>
        {t('pages:hooks.pipelineSummary', { total: data.totalHooks, enabled: data.enabledHooks })}
      </p>
      <p className={styles.policyNote}>
        <strong>{t('pages:hooks.pipelinePolicies')}</strong> {t('pages:hooks.pipelinePolicyNote')}
      </p>
      <p className={styles.policyNote}>
        <strong>{t('pages:hooks.pipelineOrder')}</strong> {t('pages:hooks.pipelineOrderNote')}
      </p>
      <p className={styles.reorderHint}>{t('pages:hooks.pipelineReorderHint')}</p>

      <div className={styles.timeline}>
        {data.flow.map((node, i) => (
          <div key={`${node.kind}-${node.id}-${i}`} className={styles.timelineNode}>
            <div
              className={`${styles.timelineDot} ${
                node.kind === HOOK_EXECUTION_FLOW_KIND.MILESTONE ? styles.dotMilestone : styles.dotHook
              }`}
            />
            {node.kind === HOOK_EXECUTION_FLOW_KIND.MILESTONE && (
              <div className={styles.milestoneLabel}>{t(`pages:hooks.flowLabel.${node.id}`, node.label)}</div>
            )}
            {node.kind === HOOK_EXECUTION_FLOW_KIND.HOOK_SEGMENT && (
              <>
                <div className={styles.segmentLabel}>{t(`pages:hooks.flowLabel.${node.id}`, node.label)}</div>
                {node.steps.length === 0 ? (
                  <div className={styles.emptyPhase}>
                    {t('pages:hooks.pipelineEmptyPhase')}
                  </div>
                ) : (
                  <div className={styles.stepsColumn}>
                    {node.steps.map((step, idx) => (
                      <div key={step.hookConfigId} className={styles.stepRow}>
                        {node.steps.length > 1 && (
                          <ReorderButtons
                            phase={node.phase}
                            steps={node.steps}
                            index={idx}
                            disabled={reorderLoading}
                            onApplyIds={reorderHooks}
                          />
                        )}
                        <StepCard step={step} />
                      </div>
                    ))}
                  </div>
                )}
              </>
            )}
          </div>
        ))}
      </div>

      <div className={styles.footer}>
        {t('pages:hooks.pipelineFooterDecisions')}
        <br />
        {t('pages:hooks.pipelineFooterOutcome')}
      </div>
    </div>
  );
}
