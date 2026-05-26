/**
 * Cache admin page — fleet-wide configuration for gateway and provider cache.
 *
 * Two top-level tabs (redesigned after "page too tall" feedback):
 *
 *   Tab 1 — Gateway Cache
 *     Card: Embedding & semantic tuning (collapsed-chip default when configured)
 *     Card: Freshness rules
 *
 *   Tab 2 — Provider Prompt Cache
 *     SettingsCacheTab (provider-prompt cache)
 *
 * Sticky StatusStrip above the tabs surfaces hit-rate + emergency disable.
 *
 * Per-route cache config does not exist — every knob is fleet-wide per the
 * "fleet-only cache" policy.
 *
 * IAM: page-level read requires either admin:prompt-cache.read OR
 * admin:semantic-cache.read. Each tab independently gates on its own action;
 * if an admin holds only one read action, the other tab is not rendered.
 */
import { useState, useEffect, useCallback, useMemo, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import { useToast } from '@/context/ToastContext';
import {
  semanticCacheConfigApi,
  type SemanticCacheUpdateInput,
} from '@/api/services/cache/semanticCacheConfig';
import { prewarm as prewarmApi } from '@/api/services/cache/semanticPrewarm';
import type { PrewarmEntry, PrewarmResult } from '@/api/services/cache/semanticPrewarm';
import { systemApi } from '@/api/services';
import type { AdminModelsByProvider, SemanticCacheConfig, ProbeResult } from '@/api/types';
import {
  PageHeader,
  Stack,
  Card,
  Button,
  FormField,
  Switch,
  Input,
  Select,
  Skeleton,
  ErrorBanner,
  AlertDialog,
  Dialog,
  Textarea,
  Tooltip,
  Tabs,
  TabsList,
  TabsTrigger,
  TabsContent,
} from '@/components/ui';
import { ProviderModelPicker } from '@/components/ProviderModelPicker';
import { StatusStrip } from './sections/StatusStrip';
import { ExtractCacheCard } from './sections/ExtractCacheCard';
import { FreshnessRulesCard } from './sections/FreshnessRulesCard';
import { ProviderPromptCacheSection } from './sections/ProviderPromptCacheSection';
import { RecentFeedbackCard } from './sections/RecentFeedbackCard';
import styles from './CachePage.module.css';

const EMPTY_PROVIDER_GROUPS: AdminModelsByProvider[] = [];

const LATENCY_FAST_MS = 50;
const LATENCY_ACCEPTABLE_MS = 90;

type ProbeLatencyTier = 'fast' | 'acceptable' | 'slow';

function latencyTier(ms: number): ProbeLatencyTier {
  if (ms < LATENCY_FAST_MS) return 'fast';
  if (ms <= LATENCY_ACCEPTABLE_MS) return 'acceptable';
  return 'slow';
}

function formatRelativeTime(isoString: string): string {
  const date = new Date(isoString);
  const now = Date.now();
  const diffMs = now - date.getTime();
  const diffMins = Math.floor(diffMs / 60_000);
  if (diffMins < 1) return 'just now';
  if (diffMins < 60) return `${diffMins}m ago`;
  const diffHours = Math.floor(diffMins / 60);
  if (diffHours < 24) return `${diffHours}h ago`;
  return `${Math.floor(diffHours / 24)}d ago`;
}

type VaryBy = 'none' | 'user' | 'vk' | 'org';
type EmbedStrategy =
  | 'last_user'
  | 'system_plus_last_user'
  | 'recent_turns'
  | 'head_plus_tail'
  | 'full_truncated';

interface Draft {
  embeddingProviderId: string | null;
  embeddingModelId: string | null;
  embeddingDimension: number | null;
  enabled: boolean;
  threshold: number;
  varyBy: VaryBy;
  embedStrategy: EmbedStrategy;
  allowCrossModel: boolean;
}

function configToDraft(cfg: SemanticCacheConfig): Draft {
  return {
    embeddingProviderId: cfg.embeddingProviderId ?? null,
    embeddingModelId: cfg.embeddingModelId ?? null,
    embeddingDimension: cfg.embeddingDimension ?? null,
    enabled: cfg.enabled,
    threshold: cfg.threshold,
    varyBy: cfg.varyBy,
    embedStrategy: cfg.embedStrategy,
    allowCrossModel: cfg.allowCrossModel,
  };
}

function isDraftChanged(draft: Draft, saved: SemanticCacheConfig): boolean {
  return (
    draft.embeddingProviderId !== (saved.embeddingProviderId ?? null) ||
    draft.embeddingModelId !== (saved.embeddingModelId ?? null) ||
    draft.enabled !== saved.enabled ||
    draft.threshold !== saved.threshold ||
    draft.varyBy !== saved.varyBy ||
    draft.embedStrategy !== saved.embedStrategy ||
    draft.allowCrossModel !== saved.allowCrossModel
  );
}

function isModelChanged(draft: Draft, saved: SemanticCacheConfig): boolean {
  return (
    draft.embeddingProviderId !== (saved.embeddingProviderId ?? null) ||
    draft.embeddingModelId !== (saved.embeddingModelId ?? null)
  );
}

// ── Pre-warm helpers ──────────────────────────────────────────────────────

const PREWARM_MAX_ENTRIES = 500;
const PREWARM_MIN_TTL = 60;
const PREWARM_MAX_TTL = 604800;

function parseCorpus(raw: string): PrewarmEntry[] {
  const trimmed = raw.trim();
  if (!trimmed) throw new Error('Empty input');

  if (trimmed.startsWith('[') || trimmed.startsWith('{')) {
    const parsed: unknown = JSON.parse(trimmed);
    const arr = Array.isArray(parsed) ? parsed : [parsed];
    return arr.map((item, i) => {
      if (typeof item !== 'object' || item === null) {
        throw new Error(`Entry ${i}: expected object`);
      }
      const e = item as Record<string, unknown>;
      if (typeof e['prompt'] !== 'string') throw new Error(`Entry ${i}: "prompt" must be a string`);
      if (typeof e['response'] !== 'string') throw new Error(`Entry ${i}: "response" must be a string`);
      return {
        prompt: e['prompt'] as string,
        response: e['response'] as string,
        model: typeof e['model'] === 'string' ? e['model'] : undefined,
        vkScope: typeof e['vkScope'] === 'string' ? e['vkScope'] : undefined,
        ttlSeconds: typeof e['ttlSeconds'] === 'number' ? e['ttlSeconds'] : undefined,
      };
    });
  }

  const lines = trimmed.split('\n').filter((l) => l.trim());
  if (lines.length < 2) throw new Error('CSV requires a header row and at least one data row');
  const header = lines[0].split(',').map((h) => h.trim().toLowerCase());
  const promptIdx = header.indexOf('prompt');
  const responseIdx = header.indexOf('response');
  if (promptIdx === -1) throw new Error('CSV header missing "prompt" column');
  if (responseIdx === -1) throw new Error('CSV header missing "response" column');
  const modelIdx = header.indexOf('model');
  const vkScopeIdx = header.indexOf('vkscope');
  const ttlIdx = header.indexOf('ttl_seconds');

  return lines.slice(1).map((line, i) => {
    const cols = line.split(',');
    const prompt = (cols[promptIdx] ?? '').trim();
    const response = (cols[responseIdx] ?? '').trim();
    if (!prompt) throw new Error(`CSV row ${i + 2}: "prompt" is empty`);
    if (!response) throw new Error(`CSV row ${i + 2}: "response" is empty`);
    const ttlRaw = ttlIdx >= 0 ? Number((cols[ttlIdx] ?? '').trim()) : NaN;
    return {
      prompt,
      response,
      model: modelIdx >= 0 ? (cols[modelIdx] ?? '').trim() || undefined : undefined,
      vkScope: vkScopeIdx >= 0 ? (cols[vkScopeIdx] ?? '').trim() || undefined : undefined,
      ttlSeconds: !isNaN(ttlRaw) && ttlRaw > 0 ? ttlRaw : undefined,
    };
  });
}

function validateCorpus(entries: PrewarmEntry[]): string[] {
  const errors: string[] = [];
  if (entries.length === 0) errors.push('No entries found');
  if (entries.length > PREWARM_MAX_ENTRIES) {
    errors.push(`Too many entries: ${entries.length} (max ${PREWARM_MAX_ENTRIES})`);
  }
  entries.forEach((e, i) => {
    if (!e.prompt) errors.push(`Entry ${i}: prompt is empty`);
    if (!e.response) errors.push(`Entry ${i}: response is empty`);
    if (e.ttlSeconds !== undefined && (e.ttlSeconds < PREWARM_MIN_TTL || e.ttlSeconds > PREWARM_MAX_TTL)) {
      errors.push(`Entry ${i}: ttlSeconds ${e.ttlSeconds} out of range [${PREWARM_MIN_TTL}, ${PREWARM_MAX_TTL}]`);
    }
  });
  return errors;
}

// ── Page ──────────────────────────────────────────────────────────────────

export function CachePage() {
  const { t } = useTranslation();
  const { addToast } = useToast();
  const canUpdateSemantic = usePermission('semantic-cache:update');
  const canReadSemantic = usePermission('semantic-cache:read');
  const canReadPromptCache = usePermission('prompt-cache:read');

  const { data, loading, error, refetch } = useApi<SemanticCacheConfig>(
    () => semanticCacheConfigApi.getConfig(),
    ['admin', 'semantic-cache', 'config'],
  );

  const { data: providerModelsData } = useApi<{ data: AdminModelsByProvider[] }>(
    () => systemApi.listModels(),
    ['admin', 'models', 'grouped', 'cache-embedding-picker'],
  );
  const providerGroups = providerModelsData?.data ?? EMPTY_PROVIDER_GROUPS;

  const [draft, setDraft] = useState<Draft | null>(null);
  const [probeResult, setProbeResult] = useState<ProbeResult | null>(null);
  const [probeLoading, setProbeLoading] = useState(false);
  const [confirmRebuildOpen, setConfirmRebuildOpen] = useState(false);

  // ── Visual-polish (C) state ─────────────────────────────────────────────
  const [activeTab, setActiveTab] = useState<'gateway' | 'provider'>(
    canReadSemantic ? 'gateway' : 'provider',
  );

  // Pre-warm modal state
  const [prewarmOpen, setPrewarmOpen] = useState(false);
  const [prewarmText, setPrewarmText] = useState('');
  const [prewarmParseError, setPrewarmParseError] = useState<string | null>(null);
  const [prewarmValidationErrors, setPrewarmValidationErrors] = useState<string[]>([]);
  const [prewarmParsed, setPrewarmParsed] = useState<PrewarmEntry[] | null>(null);
  const [prewarmDryRunResult, setPrewarmDryRunResult] = useState<PrewarmResult | null>(null);
  const [prewarmLoading, setPrewarmLoading] = useState(false);
  const [prewarmProgress, setPrewarmProgress] = useState(0);
  const fileInputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (data) {
      setDraft(configToDraft(data));
      setProbeResult(null);
    }
  }, [data]);

  const onSaveSuccess = useCallback(() => {
    refetch();
    addToast(t('pages:aiGateway.cache.saved'), 'success');
  }, [refetch, addToast, t]);

  const { mutate: executeSave, loading: saving } = useMutation(
    (input: SemanticCacheUpdateInput) => semanticCacheConfigApi.saveConfig(input),
    {
      invalidateQueries: [['admin', 'semantic-cache', 'config']],
      onSuccess: onSaveSuccess,
    },
  );

  const handleSaveClick = useCallback(() => {
    if (!draft || !data) return;
    if (isModelChanged(draft, data)) {
      setConfirmRebuildOpen(true);
      return;
    }
    void executeSave({
      embeddingProviderId: draft.embeddingProviderId,
      embeddingModelId: draft.embeddingModelId,
      embeddingDimension: draft.embeddingDimension,
      enabled: draft.enabled,
      threshold: draft.threshold,
      varyBy: draft.varyBy,
      embedStrategy: draft.embedStrategy,
      allowCrossModel: draft.allowCrossModel,
    });
  }, [draft, data, executeSave]);

  const handleRebuildConfirm = useCallback(() => {
    setConfirmRebuildOpen(false);
    if (!draft) return;
    void executeSave({
      embeddingProviderId: draft.embeddingProviderId,
      embeddingModelId: draft.embeddingModelId,
      embeddingDimension: draft.embeddingDimension,
      enabled: draft.enabled,
      threshold: draft.threshold,
      varyBy: draft.varyBy,
      embedStrategy: draft.embedStrategy,
      allowCrossModel: draft.allowCrossModel,
    });
  }, [draft, executeSave]);

  const handleProbe = useCallback(async () => {
    if (!draft?.embeddingProviderId) return;
    setProbeLoading(true);
    setProbeResult(null);
    try {
      const result = await semanticCacheConfigApi.runProbe(draft.embeddingProviderId);
      setProbeResult(result);
      if (result.ok && result.dimension) {
        setDraft((prev) =>
          prev ? { ...prev, embeddingDimension: result.dimension ?? null } : prev,
        );
      }
    } finally {
      setProbeLoading(false);
    }
  }, [draft?.embeddingProviderId]);

  // ── Pre-warm handlers ────────────────────────────────────────────────────

  const handlePrewarmOpen = useCallback(() => {
    setPrewarmText('');
    setPrewarmParseError(null);
    setPrewarmValidationErrors([]);
    setPrewarmParsed(null);
    setPrewarmDryRunResult(null);
    setPrewarmProgress(0);
    setPrewarmOpen(true);
  }, []);

  const handlePrewarmTextChange = useCallback((value: string) => {
    setPrewarmText(value);
    setPrewarmParseError(null);
    setPrewarmValidationErrors([]);
    setPrewarmParsed(null);
    setPrewarmDryRunResult(null);
  }, []);

  const handlePrewarmFileChange = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = (evt) => {
      const text = evt.target?.result;
      if (typeof text === 'string') {
        setPrewarmText(text);
        setPrewarmParseError(null);
        setPrewarmValidationErrors([]);
        setPrewarmParsed(null);
        setPrewarmDryRunResult(null);
      }
    };
    reader.readAsText(file);
  }, []);

  const parseAndValidate = useCallback((): PrewarmEntry[] | null => {
    setPrewarmParseError(null);
    setPrewarmValidationErrors([]);
    let parsed: PrewarmEntry[];
    try {
      parsed = parseCorpus(prewarmText);
    } catch (err) {
      setPrewarmParseError(err instanceof Error ? err.message : String(err));
      return null;
    }
    const errors = validateCorpus(parsed);
    if (errors.length > 0) {
      setPrewarmValidationErrors(errors);
      return null;
    }
    return parsed;
  }, [prewarmText]);

  const handlePrewarmPreview = useCallback(async () => {
    const entries = parseAndValidate();
    if (!entries) return;
    setPrewarmParsed(entries);
    setPrewarmLoading(true);
    setPrewarmProgress(10);
    setPrewarmDryRunResult(null);
    try {
      const result = await prewarmApi({ entries, dryRun: true });
      setPrewarmDryRunResult(result);
      setPrewarmProgress(100);
    } catch {
      setPrewarmParseError(t('pages:aiGateway.cache.prewarm.errorToast'));
    } finally {
      setPrewarmLoading(false);
    }
  }, [parseAndValidate, t]);

  const handlePrewarmConfirm = useCallback(async () => {
    const entries = prewarmParsed ?? parseAndValidate();
    if (!entries) return;
    setPrewarmLoading(true);
    setPrewarmProgress(5);
    setPrewarmDryRunResult(null);
    try {
      await prewarmApi({ entries, dryRun: false });
      setPrewarmProgress(100);
      addToast(t('pages:aiGateway.cache.prewarm.successToast'), 'success');
      setPrewarmOpen(false);
    } catch {
      setPrewarmParseError(t('pages:aiGateway.cache.prewarm.errorToast'));
      setPrewarmProgress(0);
    } finally {
      setPrewarmLoading(false);
    }
  }, [prewarmParsed, parseAndValidate, addToast, t]);

  // ── Derived values ───────────────────────────────────────────────────────

  const dirty = useMemo(
    () => (draft && data ? isDraftChanged(draft, data) : false),
    [draft, data],
  );

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!draft) return null;

  const hasProviderAndModel = !!draft.embeddingProviderId && !!draft.embeddingModelId;
  const killSwitchDisabled = !hasProviderAndModel;

  const saveDisabled =
    !canUpdateSemantic ||
    saving ||
    !data ||
    !isDraftChanged(draft, data) ||
    (draft.enabled && !hasProviderAndModel);

  const savedProviderName = data?.embeddingProviderId
    ? (providerGroups.find((g) => g.provider?.id === data.embeddingProviderId)?.provider
        ?.displayName ||
      providerGroups.find((g) => g.provider?.id === data.embeddingProviderId)?.provider?.name ||
      data.embeddingProviderId)
    : null;

  const savedModelName = data?.embeddingModelId
    ? (() => {
        const group = providerGroups.find((g) => g.provider?.id === data.embeddingProviderId);
        const model = group?.models?.find((m) => m.id === data.embeddingModelId);
        return model ? `${model.name} (${model.providerModelId})` : data.embeddingModelId;
      })()
    : null;

  const draftProviderName = draft.embeddingProviderId
    ? (providerGroups.find((g) => g.provider?.id === draft.embeddingProviderId)?.provider
        ?.displayName ||
      providerGroups.find((g) => g.provider?.id === draft.embeddingProviderId)?.provider?.name ||
      draft.embeddingProviderId)
    : null;

  const draftModelName = draft.embeddingModelId
    ? (() => {
        const group = providerGroups.find((g) => g.provider?.id === draft.embeddingProviderId);
        const model = group?.models?.find((m) => m.id === draft.embeddingModelId);
        return model ? `${model.name} (${model.providerModelId})` : draft.embeddingModelId;
      })()
    : null;

  const fingerprintShort = data?.embeddingFingerprint ? data.embeddingFingerprint.slice(-8) : '—';

  const showReindexWarning =
    data !== null &&
    (draft.embeddingProviderId !== (data.embeddingProviderId ?? null) ||
      draft.embeddingModelId !== (data.embeddingModelId ?? null));

  const notConfiguredLabel = t('pages:aiGateway.cache.notConfigured');

  const rebuildConfirmDescription = t('pages:aiGateway.cache.embedding.rebuildDescription', {
    oldModel: savedModelName ?? notConfiguredLabel,
    newModel:
      draftProviderName && draftModelName
        ? `${draftProviderName} / ${draftModelName}`
        : notConfiguredLabel,
    oldDim: data?.embeddingDimension ?? '?',
    newDim: draft.embeddingDimension ?? '?',
  });

  // ── Tab renderers ────────────────────────────────────────────────────────

  const renderGatewayTab = () => (
    <Stack gap="md">
      {/* Card: Extract cache (L1 exact-match) — placed first because it's
          the most-fundamental cache layer, queried before semantic. */}
      <ExtractCacheCard />

      {/* Card: Semantic cache. Embedding model is a sub-section because
          embedding only ever powers semantic cache (never standalone).
          Two-column settings grid: label on left at fixed X, control on right.
          Sub-section dividers visually group "Embedding model" / "Tuning". */}
      <Card>
        <h3 className={styles.cardHeading}>
          {t('pages:aiGateway.cache.semantic.cardTitle')}
        </h3>

        <div className={styles.settingsGrid}>
          {/* ── Embedding model ──────────────────────────────────────────── */}
          <div className={styles.settingsDivider}>
            {t('pages:aiGateway.cache.embedding.cardTitle')}
          </div>

          <label className={styles.settingLabel}>
            {t('pages:aiGateway.cache.embedding.providerLabel')}
          </label>
          <div className={styles.settingControl}>
            <Select
              value={draft.embeddingProviderId ?? ''}
              onValueChange={(v) =>
                setDraft({
                  ...draft,
                  embeddingProviderId: v === '' ? null : v,
                  embeddingModelId: null,
                })
              }
              options={providerGroups
                .filter((g) => g.provider && (g.models ?? []).some((m) => m.type === 'embedding'))
                .map((g) => ({
                  value: g.provider!.id,
                  label: g.provider!.displayName?.trim() || g.provider!.name,
                }))
                .sort((a, b) => a.label.localeCompare(b.label))}
              placeholder={t('common:providerModelPicker.selectProvider', 'Select a configured provider…')}
              disabled={!canUpdateSemantic}
              className={styles.mediumSelect}
            />
          </div>

          <label className={styles.settingLabel}>
            {t('pages:aiGateway.cache.embedding.modelLabel')}
          </label>
          <div className={styles.settingControl}>
            <Select
              value={draft.embeddingModelId ?? ''}
              onValueChange={(v) =>
                setDraft({ ...draft, embeddingModelId: v === '' ? null : v })
              }
              options={(
                providerGroups.find((g) => g.provider?.id === draft.embeddingProviderId)?.models ?? []
              )
                .filter((m) => m.type === 'embedding')
                .map((m) => ({ value: m.id, label: `${m.name} (${m.providerModelId})` }))
                .sort((a, b) => a.label.localeCompare(b.label))}
              placeholder={
                !draft.embeddingProviderId
                  ? t('common:providerModelPicker.selectProviderFirst', 'Select a provider first')
                  : t('common:providerModelPicker.selectModel', 'Select a model…')
              }
              disabled={!canUpdateSemantic || !draft.embeddingProviderId}
              className={styles.mediumSelect}
            />
          </div>

          {hasProviderAndModel && (
            <>
              <label className={styles.settingLabel}>
                {t('pages:aiGateway.cache.embedding.runProbe').replace(/^Run /, '')}
              </label>
              <div className={styles.settingControlSpan}>
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={() => void handleProbe()}
                  loading={probeLoading}
                >
                  {t('pages:aiGateway.cache.embedding.runProbe')}
                </Button>
                {probeResult?.ok && probeResult.latencyMs !== undefined && (
                  <>
                    <span className={styles[`probePill-${latencyTier(probeResult.latencyMs)}`]}>
                      {latencyTier(probeResult.latencyMs) === 'fast' &&
                        t('pages:aiGateway.cache.embedding.probeFast')}
                      {latencyTier(probeResult.latencyMs) === 'acceptable' &&
                        t('pages:aiGateway.cache.embedding.probeAcceptable')}
                      {latencyTier(probeResult.latencyMs) === 'slow' &&
                        t('pages:aiGateway.cache.embedding.probeSlow')}
                    </span>
                    <Tooltip content={t('pages:aiGateway.cache.embedding.probeLatencyTooltip')}>
                      <span className={styles.probeDetail}>
                        {t('pages:aiGateway.cache.embedding.probeLatency', {
                          ms: probeResult.latencyMs,
                        })}
                      </span>
                    </Tooltip>
                  </>
                )}
                <Tooltip
                  content={
                    data && (
                      <div className={styles.detailsTooltip}>
                        <div>
                          <span className={styles.detailsLabel}>
                            {t('pages:aiGateway.cache.embedding.statusFingerprint')}:
                          </span>{' '}
                          <code className={styles.monospace}>{fingerprintShort}</code>
                        </div>
                        <div>
                          <span className={styles.detailsLabel}>
                            {t('pages:aiGateway.cache.embedding.statusIndexName')}:
                          </span>{' '}
                          <code className={styles.monospace}>{data.redisIndexName || '—'}</code>
                        </div>
                        <div>
                          <span className={styles.detailsLabel}>
                            {t('pages:aiGateway.cache.embedding.statusUpdatedAt')}:
                          </span>{' '}
                          {data.updatedAt ? formatRelativeTime(data.updatedAt) : '—'}
                        </div>
                        <div>
                          <span className={styles.detailsLabel}>
                            {t('pages:aiGateway.cache.embedding.statusUpdatedBy')}:
                          </span>{' '}
                          {data.updatedBy ?? '—'}
                        </div>
                      </div>
                    )
                  }
                >
                  <span className={styles.helpIcon} aria-label="Embedding details">i</span>
                </Tooltip>
              </div>
            </>
          )}

          {showReindexWarning && (
            <div className={styles.settingsRowSpan}>
              <div role="alert" className={styles.warningBanner}>
                {t('pages:aiGateway.cache.embedding.reindexWarning')}
              </div>
            </div>
          )}

          {probeResult && !probeResult.ok && (
            <div className={styles.settingsRowSpan}>
              <div role="alert" className={styles.probeErrorBanner}>
                {probeResult.error ?? t('pages:aiGateway.cache.embedding.probeError')}
              </div>
            </div>
          )}

          {/* Status details surface via the "i" tooltip next to Run Probe — no
              inline panel needed (2026-05-21). */}

          {/* ── Tuning sub-section ───────────────────────────────────────── */}
          <div className={styles.settingsDivider}>
            {t('pages:aiGateway.cache.semantic.tuningSubheading')}
          </div>

          <label className={styles.settingLabel}>
            {t('pages:aiGateway.cache.semantic.killSwitch')}
          </label>
          <div className={styles.settingControl}>
            {killSwitchDisabled ? (
              <Tooltip content={t('pages:aiGateway.cache.semantic.killSwitchDisabledTooltip')}>
                <div className={styles.switchWrapper}>
                  <Switch
                    checked={false}
                    onCheckedChange={() => {}}
                    disabled
                    aria-label={t('pages:aiGateway.cache.semantic.killSwitch')}
                  />
                </div>
              </Tooltip>
            ) : (
              <Switch
                checked={draft.enabled}
                onCheckedChange={(checked) => setDraft({ ...draft, enabled: checked })}
                disabled={!canUpdateSemantic}
                aria-label={t('pages:aiGateway.cache.semantic.killSwitch')}
              />
            )}
          </div>

          <label className={styles.settingLabel}>
            {t('pages:aiGateway.cache.semantic.thresholdLabel')}
            <Tooltip content={t('pages:aiGateway.cache.semantic.thresholdHelp')}>
              <span className={styles.helpIcon} aria-label="More info">?</span>
            </Tooltip>
          </label>
          <div className={styles.settingControl}>
            <Input
              type="number"
              step="0.01"
              min={0}
              max={1}
              value={String(draft.threshold)}
              disabled={!canUpdateSemantic}
              onChange={(e) => {
                const v = parseFloat(e.target.value);
                setDraft({ ...draft, threshold: isNaN(v) ? draft.threshold : v });
              }}
              aria-label={t('pages:aiGateway.cache.semantic.thresholdLabel')}
              className={styles.shortInput}
            />
          </div>

          <label className={styles.settingLabel}>
            {t('pages:aiGateway.cache.semantic.allowCrossModelLabel')}
            <Tooltip content={t('pages:aiGateway.cache.semantic.allowCrossModelHelp')}>
              <span className={styles.helpIcon} aria-label="More info">?</span>
            </Tooltip>
          </label>
          <div className={styles.settingControl}>
            <Switch
              checked={draft.allowCrossModel}
              onCheckedChange={(checked) => setDraft({ ...draft, allowCrossModel: checked })}
              disabled={!canUpdateSemantic}
              aria-label={t('pages:aiGateway.cache.semantic.allowCrossModelLabel')}
            />
          </div>

          <label className={styles.settingLabel}>
            {t('pages:aiGateway.cache.semantic.varyByLabel')}
            <Tooltip content={t('pages:aiGateway.cache.semantic.varyByHelp')}>
              <span className={styles.helpIcon} aria-label="More info">?</span>
            </Tooltip>
          </label>
          <div className={styles.settingControl}>
            <Select
              value={draft.varyBy}
              disabled={!canUpdateSemantic}
              onValueChange={(v) => setDraft({ ...draft, varyBy: v as VaryBy })}
              options={[
                { value: 'vk', label: 'vk (single-tenant default)' },
                { value: 'user', label: 'user' },
                { value: 'org', label: 'org' },
                { value: 'none', label: 'none (no isolation)' },
              ]}
              className={styles.mediumSelect}
            />
          </div>

          <label className={styles.settingLabel}>
            {t('pages:aiGateway.cache.semantic.embedStrategyLabel')}
            <Tooltip content={t('pages:aiGateway.cache.semantic.embedStrategyHelp')}>
              <span className={styles.helpIcon} aria-label="More info">?</span>
            </Tooltip>
          </label>
          <div className={styles.settingControl}>
            <Select
              value={draft.embedStrategy}
              disabled={!canUpdateSemantic}
              onValueChange={(v) => setDraft({ ...draft, embedStrategy: v as EmbedStrategy })}
              options={[
                { value: 'system_plus_last_user', label: 'system + last user (default)' },
                { value: 'last_user', label: 'last user message only' },
                { value: 'recent_turns', label: 'recent conversation turns' },
                { value: 'head_plus_tail', label: 'head + tail (long convo)' },
                { value: 'full_truncated', label: 'full convo, truncated' },
              ]}
              className={styles.mediumSelect}
            />
          </div>
        </div>

        {/* Actions — right-aligned at the bottom of the Semantic cache card */}
        <div className={styles.settingsActions}>
          {data?.enabled && (
            <Button
              variant="secondary"
              onClick={handlePrewarmOpen}
              disabled={!canUpdateSemantic}
            >
              {t('pages:aiGateway.cache.prewarm.openButton')}
            </Button>
          )}
          <Button onClick={handleSaveClick} loading={saving} disabled={saveDisabled}>
            {t('common:save')}
          </Button>
        </div>
      </Card>

      {/* Card: Freshness rules */}
      <FreshnessRulesCard />

      {/* Card: Recent admin negative-feedback ("Mark as bad cache hit"). */}
      <RecentFeedbackCard />
    </Stack>
  );

  // ── Page composition ────────────────────────────────────────────────────

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:aiGateway.cache.title')}
        subtitle={t('pages:aiGateway.cache.subtitle')}
      />

      <StatusStrip
        semanticEnabled={data?.enabled ?? false}
        canDisable={canUpdateSemantic}
      />

      <Tabs value={activeTab} onValueChange={(v) => setActiveTab(v as 'gateway' | 'provider')}>
        <TabsList>
          {canReadSemantic && (
            <TabsTrigger value="gateway">
              <span>{t('pages:aiGateway.cache.sectionGateway.title')}</span>
              {dirty && <span className={styles.dirtyDot} aria-label="unsaved changes" />}
            </TabsTrigger>
          )}
          {canReadPromptCache && (
            <TabsTrigger value="provider">
              {t('pages:aiGateway.cache.sectionProvider.title')}
            </TabsTrigger>
          )}
        </TabsList>

        {canReadSemantic && (
          <TabsContent value="gateway">{renderGatewayTab()}</TabsContent>
        )}

        {canReadPromptCache && (
          <TabsContent value="provider">
            <ProviderPromptCacheSection />
          </TabsContent>
        )}
      </Tabs>

      {/* Pre-warm modal */}
      <Dialog
        open={prewarmOpen}
        onOpenChange={setPrewarmOpen}
        title={t('pages:aiGateway.cache.prewarm.modalTitle')}
        size="lg"
      >
        <div className={styles.prewarmModalBody}>
          <FormField label={t('pages:aiGateway.cache.prewarm.jsonLabel')}>
            <Textarea
              className={styles.prewarmTextarea}
              value={prewarmText}
              onChange={(e) => handlePrewarmTextChange(e.target.value)}
              placeholder={t('pages:aiGateway.cache.prewarm.jsonPlaceholder')}
              disabled={prewarmLoading}
              rows={8}
            />
          </FormField>

          <div className={styles.prewarmFileRow}>
            <span>{t('pages:aiGateway.cache.prewarm.csvFileLabel')}</span>
            <input
              ref={fileInputRef}
              type="file"
              accept=".json,.csv"
              className={styles.prewarmFileInput}
              onChange={handlePrewarmFileChange}
              disabled={prewarmLoading}
            />
          </div>

          {(prewarmParseError || prewarmValidationErrors.length > 0) && (
            <div role="alert" className={styles.prewarmValidationErrors}>
              {prewarmParseError && <div>{prewarmParseError}</div>}
              {prewarmValidationErrors.length > 0 && (
                <ul className={styles.prewarmValidationList}>
                  {prewarmValidationErrors.map((err) => (
                    <li key={err}>{err}</li>
                  ))}
                </ul>
              )}
            </div>
          )}

          {prewarmLoading && (
            <div className={styles.prewarmProgressRow}>
              <span className={styles.prewarmProgressLabel}>
                {t('pages:aiGateway.cache.prewarm.progressLabel')}
              </span>
              <div className={styles.prewarmProgressBar}>
                <div
                  className={styles.prewarmProgressFill}
                  style={{ width: `${prewarmProgress}%` }}
                />
              </div>
            </div>
          )}

          {prewarmDryRunResult !== null && !prewarmLoading && (
            <div className={styles.prewarmPreview}>
              <p className={styles.prewarmPreviewTitle}>
                {t('pages:aiGateway.cache.prewarm.plannedWritesLabel')}
              </p>
              <div className={styles.prewarmPreviewGrid}>
                <span className={styles.prewarmPreviewLabel}>
                  {t('pages:aiGateway.cache.prewarm.previewEntries')}
                </span>
                <span className={styles.prewarmPreviewValue}>{prewarmParsed?.length ?? 0}</span>

                <span className={styles.prewarmPreviewLabel}>
                  {t('pages:aiGateway.cache.prewarm.previewEmbedCalls')}
                </span>
                <span className={styles.prewarmPreviewValue}>
                  {prewarmDryRunResult.embeddingsCalls}
                </span>

                <span className={styles.prewarmPreviewLabel}>
                  {t('pages:aiGateway.cache.prewarm.previewCost')}
                </span>
                <span className={styles.prewarmPreviewValue}>
                  {t('pages:aiGateway.cache.prewarm.previewCostValue', {
                    cost: prewarmDryRunResult.embeddingCostUsd.toFixed(4),
                  })}
                </span>

                <span className={styles.prewarmPreviewLabel}>
                  {t('pages:aiGateway.cache.prewarm.previewDuration')}
                </span>
                <span className={styles.prewarmPreviewValue}>
                  {t('pages:aiGateway.cache.prewarm.previewDurationValue', {
                    ms: prewarmDryRunResult.durationMs,
                  })}
                </span>
              </div>
            </div>
          )}

          <div className={styles.prewarmModalActions}>
            <Button
              variant="secondary"
              onClick={() => setPrewarmOpen(false)}
              disabled={prewarmLoading}
            >
              {t('pages:aiGateway.cache.prewarm.cancelButton')}
            </Button>
            <Button
              variant="secondary"
              onClick={() => void handlePrewarmPreview()}
              loading={prewarmLoading && prewarmDryRunResult === null}
              disabled={!prewarmText.trim() || prewarmLoading}
            >
              {t('pages:aiGateway.cache.prewarm.previewButton')}
            </Button>
            <Button
              onClick={() => void handlePrewarmConfirm()}
              loading={prewarmLoading && prewarmDryRunResult !== null}
              disabled={!prewarmText.trim() || prewarmLoading}
            >
              {t('pages:aiGateway.cache.prewarm.confirmButton')}
            </Button>
          </div>
        </div>
      </Dialog>

      <AlertDialog
        open={confirmRebuildOpen}
        onOpenChange={setConfirmRebuildOpen}
        title={t('pages:aiGateway.cache.embedding.rebuildTitle')}
        description={rebuildConfirmDescription}
        confirmLabel={t('pages:aiGateway.cache.embedding.rebuildConfirm')}
        cancelLabel={t('common:cancel')}
        onConfirm={handleRebuildConfirm}
        loading={saving}
      />
    </Stack>
  );
}
