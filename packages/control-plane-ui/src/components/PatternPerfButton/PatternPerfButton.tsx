import { useState } from 'react';
import { useTranslation } from 'react-i18next';

import { rulePacksApi, type PatternPerfResult } from '@/api/services';
import { Badge, Button } from '@/components/ui';

import styles from './PatternPerfButton.module.css';

interface PatternPerfButtonProps {
  pattern: string;
  flags?: string;
}

const VERDICT_VARIANT = { ok: 'success', slow: 'danger', invalid: 'warning' } as const;

/**
 * PatternPerfButton runs the authoring-time Vectorscan performance test for a
 * single regex (rule-pack rule or hook pattern) and shows a verdict, the
 * per-50KB clean/adversarial scan cost, and plain-language fix suggestions. The
 * control plane has no libhs, so the measurement is proxied to the AI Gateway.
 */
export function PatternPerfButton({ pattern, flags = '' }: PatternPerfButtonProps) {
  const { t } = useTranslation();
  const [result, setResult] = useState<PatternPerfResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  const run = async () => {
    setLoading(true);
    setResult(null);
    setError(null);
    try {
      const r = await rulePacksApi.patternPerfTest(pattern, flags);
      if (r.success === false) {
        setError(r.error || t('pages:hooks.rulePacks.perfTest.unreachable', 'AI Gateway unreachable'));
      } else {
        setResult(r);
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : t('pages:hooks.rulePacks.perfTest.failed', 'Performance test failed'));
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className={styles.wrap}>
      <Button variant="secondary" type="button" onClick={run} disabled={loading || pattern.trim() === ''}>
        {loading
          ? t('pages:hooks.rulePacks.perfTest.testing', 'Testing…')
          : t('pages:hooks.rulePacks.perfTest.test', 'Test performance')}
      </Button>

      {error && <p className={styles.error}>{error}</p>}

      {result && (
        <div className={styles.result}>
          <div className={styles.verdictRow}>
            <Badge variant={VERDICT_VARIANT[result.verdict]}>
              {t(`pages:hooks.rulePacks.perfTest.verdict.${result.verdict}`, result.verdict)}
            </Badge>
            {result.compiles && (
              <span className={styles.metrics}>
                {t('pages:hooks.rulePacks.perfTest.clean', 'clean')} {Math.round(result.cleanScanUs)}µs
                {' · '}
                {t('pages:hooks.rulePacks.perfTest.adversarial', 'adversarial')} {Math.round(result.adversarialScanUs)}µs
              </span>
            )}
          </div>
          {(result.suggestions ?? []).length > 0 && (
            <ul className={styles.suggestions}>
              {(result.suggestions ?? []).map((s) => (
                <li key={s}>{s}</li>
              ))}
            </ul>
          )}
        </div>
      )}
    </div>
  );
}
