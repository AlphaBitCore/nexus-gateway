/**
 * AI Guard settings page — singleton config form for the AI Guard classifier.
 *
 * Reads `/api/admin/ai-guard/config`, allows the admin to choose a backend
 * (configured provider vs external OpenAI-compatible URL), edit the judge
 * prompt template, and tune timeout + cache TTL. Warns loudly when the
 * external-URL mode is selected because credentials + request bodies then
 * leave the platform.
 *
 * The dry-run probe lives in `DryRunPanel` (P-B Task 27) and is mounted
 * below the form.
 */
import { useState, useEffect, type ChangeEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { useToast } from '@/context/ToastContext';
import {
  aiGuardApi,
  type AIGuardConfig,
  type AIGuardBackendMode,
} from '@/api/services/compliance/aiguard';
import { systemApi, serviceUrlsApi } from '@/api/services';
import type { AdminModelsByProvider } from '@/api/types';
import { aiguardComplianceWebhookUrl } from '@/lib/aiguardWebhook';
import {
  PageHeader,
  Stack,
  Card,
  Button,
  Input,
  Textarea,
  FormField,
  RadioGroup,
  RadioGroupItem,
  Skeleton,
  ErrorBanner,
} from '@/components/ui';
import { ProviderModelPicker } from '@/components/ProviderModelPicker';
import { DryRunPanel } from './DryRunPanel';
import styles from './AIGuardPage.module.css';

const EMPTY_PROVIDER_GROUPS: AdminModelsByProvider[] = [];

// Keep in sync with tools/db-migrate/seed/seed-aiguard.ts and
// packages/ai-gateway/internal/aiguard/prompt.go DEFAULT_PROMPT.
const DEFAULT_PROMPT_TEMPLATE = `You are a security classifier for enterprise AI traffic. Analyze the
provided CONTENT for the detector type {{.DetectorType}}. Return ONLY
valid JSON matching this schema:

{"decision":"approve|reject_hard|block_soft|modify",
 "confidence":<0.0-1.0>,
 "reason":"<short human-readable explanation>",
 "labels":["<tag>","<tag>"],
 "redactions":[
   {"start":<int>,"end":<int>,"replacement":"<text>","action":"redact|strip|replace","reason":"<why>"}
 ]}

Guidelines:
- reject_hard: clear, high-confidence policy violation. Use sparingly.
- block_soft: likely violation; the caller may warn instead of blocking.
- approve: content is acceptable for the detector type.
- modify: emit one \`redactions[]\` entry per sensitive span. \`start\`/\`end\`
  are UTF-8 byte offsets into the verbatim CONTENT string between <<<…>>>;
  \`end\` is exclusive. \`replacement\` is the placeholder to substitute
  (e.g. "[REDACTED_EMAIL]"). \`action\` defaults to "redact" when omitted.

Always emit \`redactions\` as an array (possibly empty). Never return the
whole sanitised body — only the spans.

Example for an email leak (CONTENT = "contact me at jane@example.com please"):
{"decision":"modify","confidence":0.95,"reason":"email PII",
 "labels":["pii:email"],
 "redactions":[{"start":14,"end":31,"replacement":"[REDACTED_EMAIL]","action":"redact"}]}

Context:
- Target provider: {{.TargetProvider}}
- Target model: {{.TargetModel}}
- Upstream tags: {{.TagsJoined}}

CONTENT:
<<<
{{.Content}}
>>>
`;

type HeaderRow = { key: string; value: string };

function headersToRows(headers?: Record<string, string> | null): HeaderRow[] {
  if (!headers) return [];
  return Object.entries(headers).map(([key, value]) => ({ key, value }));
}

function rowsToHeaders(rows: HeaderRow[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const r of rows) {
    const k = r.key.trim();
    if (k) out[k] = r.value;
  }
  return out;
}

export function AIGuardPage() {
  const { t } = useTranslation();
  const { addToast } = useToast();

  const { data, loading, error, refetch } = useApi<AIGuardConfig>(
    () => aiGuardApi.getConfig(),
    ['admin', 'ai-guard', 'config'],
  );

  // Cascading provider → model picker source. Skips providers with no models
  // since the classifier needs a concrete (provider, model) pair.
  const { data: providerModelsData } = useApi<{ data: AdminModelsByProvider[] }>(
    () => systemApi.listModels(),
    ['admin', 'models', 'grouped', 'ai-guard-picker'],
  );
  const providerGroups = providerModelsData?.data ?? EMPTY_PROVIDER_GROUPS;

  // Pull the runtime AI Gateway publicURL so the displayed compliance
  // webhook URL matches whatever the actual gateway reports — no
  // hardcoded host. Falls back to the window-based heuristic via the
  // helper if the API hasn't responded yet.
  const { data: serviceURLs } = useApi(
    () => serviceUrlsApi.publicURLs(),
    ['admin', 'services', 'public-urls'],
  );

  const [draft, setDraft] = useState<AIGuardConfig | null>(null);
  const [headerRows, setHeaderRows] = useState<HeaderRow[]>([]);

  useEffect(() => {
    if (data) {
      setDraft({ ...data });
      setHeaderRows(headersToRows(data.customHeaders));
    }
  }, [data]);

  const { mutate: save, loading: saving } = useMutation(
    () => {
      if (!draft) throw new Error('no draft');
      const payload: Partial<AIGuardConfig> = {
        backendMode: draft.backendMode,
        providerId: draft.providerId ?? null,
        modelId: draft.modelId ?? null,
        externalUrl: draft.externalUrl ?? null,
        externalCredentialId: draft.externalCredentialId ?? null,
        customHeaders:
          draft.backendMode === 'external_url' ? rowsToHeaders(headerRows) : null,
        promptTemplate: draft.promptTemplate,
        timeoutMs: draft.timeoutMs,
        cacheTtlSeconds: draft.cacheTtlSeconds,
      };
      return aiGuardApi.saveConfig(payload);
    },
    {
      invalidateQueries: [['admin', 'ai-guard', 'config']],
      onSuccess: () => refetch(),
      successMessage: t('pages:settings.aiGuard.saved', 'AI Guard configuration saved'),
    },
  );

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  if (!draft) return null;
  const runtimeWebhookUrl = aiguardComplianceWebhookUrl(serviceURLs?.aiGateway);

  const onNumber = (key: 'timeoutMs' | 'cacheTtlSeconds') =>
    (e: ChangeEvent<HTMLInputElement>) => {
      const n = Number(e.target.value);
      setDraft({ ...draft, [key]: Number.isFinite(n) ? n : 0 });
    };

  const updateHeader = (i: number, patch: Partial<HeaderRow>) => {
    const next = [...headerRows];
    next[i] = { ...next[i], ...patch };
    setHeaderRows(next);
  };

  return (
    <Stack gap="md">
      <PageHeader
        title={t('pages:settings.aiGuard.title', 'AI Guard')}
        subtitle={t(
          'pages:settings.aiGuard.subtitle',
          'Configure the centralized AI content classifier used by hooks and policies.',
        )}
      />

      <Card>
        <div className={styles.form}>
          <FormField label={t('pages:settings.aiGuard.backendMode', 'Backend mode')}>
            <RadioGroup
              value={draft.backendMode}
              onValueChange={(v) =>
                setDraft({ ...draft, backendMode: v as AIGuardBackendMode })
              }
            >
              <div className={styles.radioRow}>
                <RadioGroupItem value="configured_provider" id="aig-mode-provider" />
                <label htmlFor="aig-mode-provider" className={styles.radioLabel}>
                  {t(
                    'pages:settings.aiGuard.modeConfiguredProvider',
                    'Configured provider (reuse existing provider + model)',
                  )}
                </label>
              </div>
              <div className={styles.radioRow}>
                <RadioGroupItem value="external_url" id="aig-mode-external" />
                <label htmlFor="aig-mode-external" className={styles.radioLabel}>
                  {t(
                    'pages:settings.aiGuard.modeExternalUrl',
                    'External URL (OpenAI-compatible endpoint)',
                  )}
                </label>
              </div>
            </RadioGroup>
          </FormField>

          <FormField
            label={t('pages:settings.aiGuard.complianceWebhookUrl', 'Compliance webhook URL')}
            helpText={t(
              'pages:settings.aiGuard.complianceWebhookUrlHelp',
              'Use this URL when a webhook hook should call AIGuard directly.',
            )}
          >
            <div className={styles.webhookRow}>
              <Input
                className={styles.webhookInput}
                value={runtimeWebhookUrl}
                readOnly
                onFocus={(e) => e.currentTarget.select()}
              />
              <Button
                className={styles.copyButton}
                variant="secondary"
                onClick={async () => {
                  await navigator.clipboard.writeText(runtimeWebhookUrl);
                  addToast(
                    t('pages:settings.aiGuard.complianceWebhookUrlCopied', 'Webhook URL copied'),
                    'success',
                  );
                }}
              >
                {t('pages:settings.aiGuard.copyWebhookUrl', 'Copy URL')}
              </Button>
            </div>
          </FormField>

          {draft.backendMode === 'external_url' && (
            <div role="alert" className={styles.warningBanner}>
              {t(
                'pages:settings.aiGuard.externalWarning',
                'External URL mode sends classification payloads outside your configured provider fleet. Credentials and request bodies will leave the platform via plain HTTP — verify the endpoint, TLS, and data-handling agreement before enabling.',
              )}
            </div>
          )}

          {draft.backendMode === 'configured_provider' ? (
            <ProviderModelPicker
              providerGroups={providerGroups}
              providerId={draft.providerId ?? null}
              modelId={draft.modelId ?? null}
              onChange={({ providerId, modelId }) =>
                setDraft({ ...draft, providerId, modelId })
              }
              providerLabel={t('pages:settings.aiGuard.providerId', 'Provider')}
              modelLabel={t('pages:settings.aiGuard.modelId', 'Model')}
              helpText={t(
                'pages:settings.aiGuard.providerHint',
                'Only providers and models you have already configured are selectable. Add new entries on the Providers page.',
              )}
            />
          ) : (
            <Stack gap="sm">
              <FormField label={t('pages:settings.aiGuard.externalUrl', 'Endpoint URL')}>
                <Input
                  type="url"
                  value={draft.externalUrl ?? ''}
                  onChange={(e) => setDraft({ ...draft, externalUrl: e.target.value })}
                  placeholder="https://api.example.com/v1/chat/completions"
                />
              </FormField>
              <FormField
                label={t(
                  'pages:settings.aiGuard.externalCredentialId',
                  'Credential ID',
                )}
              >
                <Input
                  value={draft.externalCredentialId ?? ''}
                  onChange={(e) =>
                    setDraft({ ...draft, externalCredentialId: e.target.value })
                  }
                  placeholder={t(
                    'pages:settings.aiGuard.externalCredentialIdPlaceholder',
                    'uuid of a stored credential',
                  )}
                />
              </FormField>
              <FormField label={t('pages:settings.aiGuard.modelName', 'Model name')}>
                <Input
                  value={draft.modelId ?? ''}
                  onChange={(e) => setDraft({ ...draft, modelId: e.target.value })}
                  placeholder="gpt-4o-mini"
                />
              </FormField>

              <div>
                <h3 className={styles.subheading}>
                  {t('pages:settings.aiGuard.customHeaders', 'Custom headers')}
                </h3>
                <p className={styles.helpText}>
                  {t(
                    'pages:settings.aiGuard.customHeadersHelp',
                    'Sent on every classifier request to the external endpoint.',
                  )}
                </p>
                {headerRows.map((row, i) => (
                  <div key={i} className={styles.headerRow}>
                    <Input
                      className={styles.headerKey}
                      placeholder={t(
                        'pages:settings.aiGuard.headerNamePlaceholder',
                        'Header name',
                      )}
                      value={row.key}
                      onChange={(e) => updateHeader(i, { key: e.target.value })}
                    />
                    <Input
                      className={styles.headerValue}
                      placeholder={t(
                        'pages:settings.aiGuard.headerValuePlaceholder',
                        'Header value',
                      )}
                      value={row.value}
                      onChange={(e) => updateHeader(i, { value: e.target.value })}
                    />
                    <Button
                      variant="danger"
                      onClick={() =>
                        setHeaderRows(headerRows.filter((_, j) => j !== i))
                      }
                    >
                      {t('common:remove', 'Remove')}
                    </Button>
                  </div>
                ))}
                <Button
                  variant="secondary"
                  onClick={() =>
                    setHeaderRows([...headerRows, { key: '', value: '' }])
                  }
                >
                  {t('common:add', 'Add')}
                </Button>
              </div>
            </Stack>
          )}

          <div>
            <div className={styles.promptFieldHeader}>
              <label htmlFor="aig-prompt-template" className={styles.promptFieldLabel}>
                {t('pages:settings.aiGuard.promptTemplate', 'Judge prompt template')}
              </label>
              <Button
                variant="secondary"
                size="sm"
                onClick={() => setDraft({ ...draft, promptTemplate: DEFAULT_PROMPT_TEMPLATE })}
              >
                {t('pages:settings.aiGuard.resetPromptTemplate', 'Reset to default')}
              </Button>
            </div>
            <Textarea
              id="aig-prompt-template"
              className={styles.promptTextarea}
              value={draft.promptTemplate}
              onChange={(e) =>
                setDraft({ ...draft, promptTemplate: e.target.value })
              }
            />
          </div>

          <FormField
            label={t('pages:settings.aiGuard.timeoutMs', 'Timeout (ms)')}
          >
            <Input
              type="number"
              min={1000}
              max={30000}
              step={500}
              className={styles.numberField}
              value={String(draft.timeoutMs)}
              onChange={onNumber('timeoutMs')}
            />
          </FormField>

          <FormField
            label={t('pages:settings.aiGuard.cacheTtlSeconds', 'Cache TTL (seconds, 0 to disable)')}
          >
            <Input
              type="number"
              min={0}
              max={86400}
              step={60}
              className={styles.numberField}
              value={String(draft.cacheTtlSeconds)}
              onChange={onNumber('cacheTtlSeconds')}
            />
          </FormField>

          <div className={styles.actions}>
            <Button onClick={() => save(undefined as never)} loading={saving}>
              {t('common:save', 'Save')}
            </Button>
          </div>
        </div>
      </Card>

      <DryRunPanel currentConfig={draft} />
    </Stack>
  );
}
