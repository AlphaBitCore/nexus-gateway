/**
 * Per-rule Retry Policy editor.
 *
 * Surfaces the two fields of `RetryPolicy` that admins can override from the
 * UI (`maxAttemptsPerTarget`, `retryOn`). Backoff knobs are intentionally
 * omitted — they live in YAML only (see docs/users/api/openapi/admin/e34-s3-routing-retry-policy.yaml §6.3).
 *
 * Mode model:
 *   "default" — rule inherits the YAML default; submission sends `retryPolicy: null`
 *               (PUT) or omits the field (POST).
 *   "custom"  — rule carries an explicit override; submission sends the
 *               structured object.
 */
import { useTranslation } from 'react-i18next';
import { Card, Input, Stack, Tooltip } from '@/components/ui';
import type { ErrorClass } from '@/api/types';
import styles from './RoutingRuleForm.module.css';
import { HelpIconButton } from '@nexus-gateway/ui-shared';

export type RetryPolicyMode = 'default' | 'custom';

/** All admin-selectable error classes, in the canonical UI order. */
export const RETRY_ON_OPTIONS: readonly ErrorClass[] = ['network', 'timeout', '429', '5xx'] as const;

export const RETRY_POLICY_MIN_ATTEMPTS = 1;
export const RETRY_POLICY_MAX_ATTEMPTS = 5;

export interface RetryPolicySectionProps {
  mode: RetryPolicyMode;
  onModeChange: (next: RetryPolicyMode) => void;
  /** String for raw input UX; validated on submit. */
  maxAttempts: string;
  onMaxAttemptsChange: (next: string) => void;
  retryOn: ErrorClass[];
  onRetryOnChange: (next: ErrorClass[]) => void;
}

export function RetryPolicySection({
  mode,
  onModeChange,
  maxAttempts,
  onMaxAttemptsChange,
  retryOn,
  onRetryOnChange,
}: RetryPolicySectionProps) {
  const { t } = useTranslation();
  const disabled = mode !== 'custom';

  const toggleClass = (cls: ErrorClass) => {
    if (retryOn.includes(cls)) {
      onRetryOnChange(retryOn.filter(c => c !== cls));
    } else {
      onRetryOnChange([...retryOn, cls]);
    }
  };

  const helpTooltip = (body: string) => (
    <Tooltip content={body}>
      <HelpIconButton aria-label={body} />
    </Tooltip>
  );

  return (
    <Card padding="lg" data-testid="routing-retry-policy-section">
      <div className={`${styles.labelRow} ${styles.sectionTitleSpacing}`}>
        <div className={styles.sectionTitle}>{t('pages:routing.retryPolicy.title')}</div>
      </div>

      <Stack gap="sm">
        <label className={styles.fieldLabel} style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-1-5)' }}>
          <input
            type="radio"
            name="routing-retry-policy-mode"
            value="default"
            checked={mode === 'default'}
            onChange={() => onModeChange('default')}
            data-testid="retry-policy-mode-default"
          />
          {t('pages:routing.retryPolicy.platformDefault')}
        </label>

        <label className={styles.fieldLabel} style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-1-5)' }}>
          <input
            type="radio"
            name="routing-retry-policy-mode"
            value="custom"
            checked={mode === 'custom'}
            onChange={() => onModeChange('custom')}
            data-testid="retry-policy-mode-custom"
          />
          {t('pages:routing.retryPolicy.custom')}
        </label>

        <div style={{ paddingLeft: 'var(--g-space-6)', opacity: disabled ? 0.5 : 1 }}>
          <Stack gap="sm">
            <div>
              <div className={styles.labelRow}>
                <label htmlFor="retry-max-attempts" className={styles.fieldLabel}>
                  {t('pages:routing.retryPolicy.maxAttempts')}
                </label>
                {helpTooltip(t('pages:routing.retryPolicy.maxAttemptsHelp'))}
              </div>
              <Input
                id="retry-max-attempts"
                className={styles.textInput}
                type="number"
                min={RETRY_POLICY_MIN_ATTEMPTS}
                max={RETRY_POLICY_MAX_ATTEMPTS}
                step={1}
                value={maxAttempts}
                onChange={(e) => onMaxAttemptsChange(e.target.value)}
                disabled={disabled}
                aria-disabled={disabled}
                data-testid="retry-max-attempts-input"
              />
            </div>

            <div>
              <div className={styles.labelRow}>
                <span className={styles.fieldLabel}>{t('pages:routing.retryPolicy.retryOn')}</span>
                {helpTooltip(t('pages:routing.retryPolicy.retryOnHelp'))}
              </div>
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--g-space-1-5) var(--g-space-4)' }}>
                {RETRY_ON_OPTIONS.map((cls) => (
                  <label
                    key={cls}
                    style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-1)', fontSize: 'var(--g-font-size-sm)' }}
                  >
                    <input
                      type="checkbox"
                      checked={retryOn.includes(cls)}
                      onChange={() => toggleClass(cls)}
                      disabled={disabled}
                      aria-disabled={disabled}
                      data-testid={`retry-on-${cls}`}
                    />
                    <code>{cls}</code>
                  </label>
                ))}
              </div>
            </div>
          </Stack>
        </div>
      </Stack>
    </Card>
  );
}

/* ─── Pure helpers — exported for unit tests + reuse from create/edit hooks ─── */

/** Initial mode + form values from the persisted RoutingRule.retryPolicy. */
export function deriveRetryPolicyInitialState(
  rp: import('@/api/types').RetryPolicy | null | undefined,
): { mode: RetryPolicyMode; maxAttempts: string; retryOn: ErrorClass[] } {
  if (!rp) {
    return { mode: 'default', maxAttempts: '3', retryOn: ['network', 'timeout', '5xx'] };
  }
  return {
    mode: 'custom',
    maxAttempts:
      typeof rp.maxAttemptsPerTarget === 'number' && rp.maxAttemptsPerTarget > 0
        ? String(rp.maxAttemptsPerTarget)
        : '3',
    retryOn: Array.isArray(rp.retryOn) ? rp.retryOn.slice() : [],
  };
}

/**
 * Build the wire payload value for `retryPolicy`. Returns:
 *   { mode: 'default' } → caller serializes as `null` on PUT (or omits on POST).
 *   { mode: 'custom', value } → caller embeds `value` verbatim.
 *
 * Returns `error` when the admin selected Custom but provided an invalid
 * `maxAttemptsPerTarget`. `retryOn` accepts an empty array — the spec defines
 * that as "retry nothing" rather than an error.
 */
export type RetryPolicyBuildResult =
  | { ok: true; mode: 'default' }
  | { ok: true; mode: 'custom'; value: import('@/api/types').RetryPolicy }
  | { ok: false; error: string };

export function buildRetryPolicyPayload(
  mode: RetryPolicyMode,
  maxAttemptsRaw: string,
  retryOn: ErrorClass[],
): RetryPolicyBuildResult {
  if (mode === 'default') {
    return { ok: true, mode: 'default' };
  }
  const trimmed = maxAttemptsRaw.trim();
  const value: import('@/api/types').RetryPolicy = {};
  if (trimmed.length > 0) {
    const n = Number(trimmed);
    if (
      !Number.isFinite(n) ||
      !Number.isInteger(n) ||
      n < RETRY_POLICY_MIN_ATTEMPTS ||
      n > RETRY_POLICY_MAX_ATTEMPTS
    ) {
      return {
        ok: false,
        error: `maxAttemptsPerTarget must be an integer in [${RETRY_POLICY_MIN_ATTEMPTS},${RETRY_POLICY_MAX_ATTEMPTS}]`,
      };
    }
    value.maxAttemptsPerTarget = n;
  }
  // Always emit retryOn even when empty — empty array is meaningful per spec.
  value.retryOn = retryOn.slice();
  return { ok: true, mode: 'custom', value };
}

/**
 * True when the typed `maxAttempts` string would fail validation in Custom
 * mode. Used by the form's submit-disabled gate.
 */
export function isRetryPolicyMaxAttemptsInvalid(mode: RetryPolicyMode, raw: string): boolean {
  if (mode !== 'custom') return false;
  const trimmed = raw.trim();
  if (trimmed.length === 0) return false; // omitted → backend treats as unset
  const n = Number(trimmed);
  return (
    !Number.isFinite(n) ||
    !Number.isInteger(n) ||
    n < RETRY_POLICY_MIN_ATTEMPTS ||
    n > RETRY_POLICY_MAX_ATTEMPTS
  );
}
