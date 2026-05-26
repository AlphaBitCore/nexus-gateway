/**
 * InputStagingSelector — cross-bundle dropdown for choosing an embedding
 * input truncation strategy.
 *
 * Consumers (CP UI, Agent Dashboard) pass translated label strings via the
 * `labels` prop so this component stays free of any i18n framework
 * dependency. i18n keys live in
 * packages/ui-shared/src/i18n/{en,zh,es}/shared.json under `inputStaging`.
 */

import { useMemo, type ChangeEvent, type SelectHTMLAttributes } from 'react';
import clsx from 'clsx';
import { suggestStrategy } from './suggest';
import type { InputStagingStrategy, InputStagingProfile } from './suggest';
import styles from './InputStagingSelector.module.css';

export type { InputStagingStrategy, InputStagingProfile } from './suggest';

/** Per-strategy translated strings provided by the consumer. */
export interface InputStagingStrategyLabels {
  last_user: string;
  system_plus_last_user: string;
  recent_turns: string;
  head_plus_tail: string;
  full_truncated: string;
}

/** Translated UI strings for the selector widget itself. */
export interface InputStagingSelectorStrings {
  /** Dropdown label, e.g. "Input Staging Strategy". */
  label?: string;
  /** Text for the "Recommended" badge appended to the suggested option. */
  recommendedBadge?: string;
  /** Optional help text rendered beneath the dropdown. */
  helpText?: string;
  /** Per-strategy option labels. */
  strategies?: Partial<InputStagingStrategyLabels>;
  /** Per-strategy tooltip titles. */
  tooltips?: Partial<InputStagingStrategyLabels>;
}

export interface InputStagingSelectorProps
  extends Omit<SelectHTMLAttributes<HTMLSelectElement>, 'value' | 'onChange' | 'children'> {
  /** Total token capacity of the target model. Must be >= 1. */
  modelContextLimit: number;
  /** Currently selected strategy value. */
  value: InputStagingStrategy;
  /** Called immediately when the user picks a different option. */
  onChange: (next: InputStagingStrategy) => void;
  /**
   * Expected output profile of the workload — used by suggestStrategy() to
   * compute the recommended option. Defaults to 'generic'.
   */
  profile?: InputStagingProfile;
  /** Disables the select element. */
  disabled?: boolean;
  /**
   * Translated UI strings. Any key that is omitted falls back to the built-in
   * English default so the component stays functional without full i18n wiring.
   *
   * Consumer pattern (CP UI):
   *   const { t } = useTranslation('shared');
   *   labels={{ label: t('inputStaging.label'), ... }}
   */
  strings?: InputStagingSelectorStrings;
  /** aria-label for the select element (accessibility). */
  ariaLabel?: string;
  className?: string;
}

const STRATEGIES: InputStagingStrategy[] = [
  'last_user',
  'system_plus_last_user',
  'recent_turns',
  'head_plus_tail',
  'full_truncated',
];

/** English fall-back option labels (used when strings.strategies is absent/partial). */
const DEFAULT_LABELS: InputStagingStrategyLabels = {
  last_user: 'Last User Message',
  system_plus_last_user: 'System + Last User',
  recent_turns: 'Recent Turns',
  head_plus_tail: 'Head + Tail',
  full_truncated: 'Full (Truncated)',
};

/** English fall-back tooltips. */
const DEFAULT_TOOLTIPS: InputStagingStrategyLabels = {
  last_user:
    'Only the final user message is sent for cache lookup. Smallest, fastest.',
  system_plus_last_user:
    'System prompt + final user message. Balances persona context with query specificity.',
  recent_turns:
    'Most recent conversation turns that fit. Best for multi-turn flows.',
  head_plus_tail:
    'First and last portions of the conversation. Useful when both opening setup and recent context matter.',
  full_truncated:
    'Full conversation, hard-truncated from the head if needed. Legacy mode.',
};

/**
 * InputStagingSelector renders a labelled `<select>` of the five input
 * truncation strategies, with the auto-recommended option marked with a
 * "Recommended" badge.
 *
 * - Calls suggestStrategy(modelContextLimit, profile) to determine the badge.
 * - Re-evaluates the badge when modelContextLimit or profile changes.
 * - Never auto-changes the selected value — the admin's choice is sticky.
 * - Emits onChange immediately on user selection.
 */
export function InputStagingSelector({
  modelContextLimit,
  value,
  onChange,
  profile = 'generic',
  disabled = false,
  strings,
  ariaLabel,
  className,
  ...selectProps
}: InputStagingSelectorProps) {
  // Re-evaluate suggested strategy when modelContextLimit or profile changes.
  const suggested = useMemo(
    () => suggestStrategy(modelContextLimit, profile),
    [modelContextLimit, profile],
  );

  const resolvedLabel = strings?.label ?? 'Input Staging Strategy';
  const resolvedBadge = strings?.recommendedBadge ?? 'Recommended';
  const resolvedHelp = strings?.helpText;

  function getStrategyLabel(s: InputStagingStrategy): string {
    return strings?.strategies?.[s] ?? DEFAULT_LABELS[s];
  }

  function getTooltip(s: InputStagingStrategy): string {
    return strings?.tooltips?.[s] ?? DEFAULT_TOOLTIPS[s];
  }

  function handleChange(e: ChangeEvent<HTMLSelectElement>) {
    onChange(e.target.value as InputStagingStrategy);
  }

  return (
    <div className={clsx(styles.root, className)}>
      <div className={styles.labelRow}>
        <label className={styles.label} htmlFor="input-staging-select">
          {resolvedLabel}
        </label>
      </div>

      <div className={styles.selectWrapper}>
        <select
          id="input-staging-select"
          className={styles.select}
          value={value}
          onChange={handleChange}
          disabled={disabled}
          aria-label={ariaLabel}
          {...selectProps}
        >
          {STRATEGIES.map((strategy) => {
            const isRecommended = strategy === suggested;
            const optionLabel = getStrategyLabel(strategy);
            return (
              <option
                key={strategy}
                value={strategy}
                title={getTooltip(strategy)}
              >
                {isRecommended
                  ? `${optionLabel} (${resolvedBadge})`
                  : optionLabel}
              </option>
            );
          })}
        </select>

        {/* Chevron icon — styled via CSS, pointer-events:none */}
        <svg
          className={styles.chevron}
          width="16"
          height="16"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
          aria-hidden="true"
        >
          <polyline points="6 9 12 15 18 9" />
        </svg>
      </div>

      {/* Recommended badge shown outside the select for sighted users */}
      {suggested === value && (
        <span className={styles.badge} aria-live="polite">
          {resolvedBadge}
        </span>
      )}

      {resolvedHelp && (
        <p className={styles.helpText}>{resolvedHelp}</p>
      )}
    </div>
  );
}

InputStagingSelector.displayName = 'InputStagingSelector';
