/**
 * DryRunPanel — small tester that POSTs `/api/admin/ai-guard/dry-run` and
 * renders the normalised request + classifier response side by side.
 *
 * Uses the `aiGuardApi.dryRun` service (P-B Task 25). The admin handler
 * (P-B Task 24) constructs an AI_GATEWAY-context call through the same
 * dispatcher as live traffic so the probe reflects real classifier
 * behaviour, including the cache-hit metadata on the second run.
 */
import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  aiGuardApi,
  type AIGuardConfig,
  type DryRunResult,
} from '@/api/services/compliance/aiguard';
import styles from './DryRunPanel.module.css';
import {
  Card,
  Stack,
  Button,
  Select,
  Textarea,
  FormField,
  ErrorBanner,
} from '@/components/ui';

/**
 * Detector types supported by the ai-guard classifier. Mirrors the curated
 * list in the OpenAPI spec; "custom" lets an admin probe an
 * arbitrary detector_type without patching the UI.
 */
const DETECTOR_TYPES = [
  'prompt_injection',
  'jailbreak',
  'toxicity',
  'secret_leak',
  'tool_call_safety',
  'hallucination',
  'data_exfiltration',
  'custom',
] as const;

type DetectorType = (typeof DETECTOR_TYPES)[number];

export interface DryRunPanelProps {
  /**
   * The current AI Guard config. Not used for request construction today
   * (the server resolves the config from the singleton row), but accepted
   * so the panel can surface backend-mode hints as the UI evolves.
   */
  currentConfig: AIGuardConfig;
}

export function DryRunPanel({ currentConfig: _currentConfig }: DryRunPanelProps) {
  const { t } = useTranslation();

  const [detectorType, setDetectorType] = useState<DetectorType>('prompt_injection');
  const [content, setContent] = useState('');
  const [loading, setLoading] = useState(false);
  const [result, setResult] = useState<DryRunResult | null>(null);
  const [errorMessage, setErrorMessage] = useState<string | null>(null);

  const detectorOptions = DETECTOR_TYPES.map((d) => ({ value: d, label: d }));

  const onRun = async () => {
    setLoading(true);
    setErrorMessage(null);
    try {
      const res = await aiGuardApi.dryRun({
        detector_type: detectorType,
        content,
        context: { ingress: 'AI_GATEWAY' },
      });
      setResult(res);
    } catch (err) {
      setErrorMessage(err instanceof Error ? err.message : String(err));
      setResult(null);
    } finally {
      setLoading(false);
    }
  };

  return (
    <Stack gap="md">
      <div>
        <h2 className={styles.sectionTitle}>
          {t('pages:settings.aiGuard.dryRun.title', 'Dry run')}
        </h2>
        <p className={styles.subtitleText}>
          {t(
            'pages:settings.aiGuard.dryRun.subtitle',
            'Probe the classifier without touching live hooks. Returns the same payload as /v1/ai-guard/classify.',
          )}
        </p>
      </div>
      <Card>
        <Stack gap="md">
          <FormField
            label={t('pages:settings.aiGuard.dryRun.detectorType', 'Detector type')}
          >
            <div className={styles.detectorSelectWrap}>
              <Select
                value={detectorType}
                onValueChange={(v) => setDetectorType(v as DetectorType)}
                options={detectorOptions}
              />
            </div>
          </FormField>

          <FormField
            label={t('pages:settings.aiGuard.dryRun.content', 'Content')}
          >
            <Textarea
              className={styles.contentTextarea}
              value={content}
              onChange={(e) => setContent(e.target.value)}
              rows={6}
              placeholder={t(
                'pages:settings.aiGuard.dryRun.contentPlaceholder',
                'Paste the text to classify',
              )}
            />
          </FormField>

          <div>
            <Button
              className={styles.runButton}
              onClick={onRun}
              loading={loading}
              disabled={loading || content.trim() === ''}
            >
              {t('pages:settings.aiGuard.dryRun.run', 'Run')}
            </Button>
          </div>

          {errorMessage && <ErrorBanner message={errorMessage} />}

          {result && (
            <Stack gap="sm">
              <div
                style={{
                  display: 'flex',
                  gap: 'var(--g-space-4)',
                  flexWrap: 'wrap',
                  fontSize: 'var(--g-font-size-base)',
                }}
              >
                <div>
                  <strong>
                    {t('pages:settings.aiGuard.dryRun.decision', 'Decision')}:
                  </strong>{' '}
                  <span data-testid="dry-run-decision">{result.response.decision}</span>
                </div>
                <div>
                  <strong>
                    {t('pages:settings.aiGuard.dryRun.latency', 'Latency')}:
                  </strong>{' '}
                  <span data-testid="dry-run-latency">
                    {result.response.metadata.judge_latency_ms}
                  </span>{' '}
                  ms
                </div>
                <div>
                  <strong>
                    {t('pages:settings.aiGuard.dryRun.cache', 'Cache')}:
                  </strong>{' '}
                  {result.response.metadata.cache_hit
                    ? t('pages:settings.aiGuard.dryRun.cacheHit', 'hit')
                    : t('pages:settings.aiGuard.dryRun.cacheMiss', 'miss')}
                </div>
              </div>

              <div
                style={{
                  display: 'grid',
                  gridTemplateColumns: '1fr 1fr',
                  gap: 'var(--g-space-3)',
                }}
              >
                <div>
                  <h3 style={{ fontSize: 'var(--g-font-size-base)', marginBottom: 'var(--g-space-1)' }}>
                    {t('pages:settings.aiGuard.dryRun.requestJson', 'Request')}
                  </h3>
                  <pre className={styles.jsonPre}>
                    {JSON.stringify(result.request, null, 2)}
                  </pre>
                </div>
                <div>
                  <h3 style={{ fontSize: 'var(--g-font-size-base)', marginBottom: 'var(--g-space-1)' }}>
                    {t('pages:settings.aiGuard.dryRun.responseJson', 'Response')}
                  </h3>
                  <pre className={styles.jsonPre}>
                    {JSON.stringify(result.response, null, 2)}
                  </pre>
                </div>
              </div>
            </Stack>
          )}
        </Stack>
      </Card>
    </Stack>
  );
}
