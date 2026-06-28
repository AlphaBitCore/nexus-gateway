/**
 * Cache admin page — fleet-wide configuration for gateway and provider cache.
 *
 * Two top-level tabs (redesigned after "page too tall" feedback):
 *
 *   Tab 1 — Gateway Cache
 *     ExtractCacheCard (L1 exact-match)
 *     SemanticCacheCard (embedding & semantic tuning) + PrewarmModal
 *     FreshnessRulesCard
 *     RecentFeedbackCard
 *
 *   Tab 2 — Provider Prompt Cache
 *     ProviderPromptCacheSection
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
import { useState, useEffect, useCallback, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import { useToast } from '@/context/ToastContext';
import {
  semanticCacheConfigApi,
  type SemanticCacheUpdateInput,
} from '@/api/services/cache/semanticCacheConfig';
import { systemApi } from '@/api/services';
import type { AdminModelsByProvider, SemanticCacheConfig, ProbeResult } from '@/api/types';
import {
  PageHeader,
  Stack,
  Skeleton,
  ErrorBanner,
  AlertDialog,
  Tabs,
  TabsList,
  TabsTrigger,
  TabsContent,
} from '@/components/ui';
import { StatusStrip } from './sections/StatusStrip';
import { ExtractCacheCard } from './sections/ExtractCacheCard';
import { FreshnessRulesCard } from './sections/FreshnessRulesCard';
import { ProviderPromptCacheSection } from './sections/ProviderPromptCacheSection';
import { RecentFeedbackCard } from './sections/RecentFeedbackCard';
import { SemanticCacheCard } from './sections/SemanticCacheCard';
import { PrewarmModal } from './sections/PrewarmModal';
import { type Draft, configToDraft, isDraftChanged, isModelChanged } from './sections/semanticCacheDraft';
import styles from './CachePage.module.css';
import trafficTabStyles from '../../traffic/analytics/TrafficAnalyticsPage.module.css';

const EMPTY_PROVIDER_GROUPS: AdminModelsByProvider[] = [];

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
  const [prewarmOpen, setPrewarmOpen] = useState(false);

  // ── Visual-polish (C) state ─────────────────────────────────────────────
  const [activeTab, setActiveTab] = useState<'gateway' | 'provider'>(
    canReadSemantic ? 'gateway' : 'provider',
  );

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

  // ── Derived values ───────────────────────────────────────────────────────

  const dirty = useMemo(
    () => (draft && data ? isDraftChanged(draft, data) : false),
    [draft, data],
  );

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!draft) return null;

  const hasProviderAndModel = !!draft.embeddingProviderId && !!draft.embeddingModelId;

  const saveDisabled =
    !canUpdateSemantic ||
    saving ||
    !data ||
    !isDraftChanged(draft, data) ||
    (draft.enabled && !hasProviderAndModel);

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
    <Stack gap="lg">
      {/* Card: Extract cache (L1 exact-match) — placed first because it's
          the most-fundamental cache layer, queried before semantic. */}
      <ExtractCacheCard />

      {/* Card: Semantic cache (embedding model + tuning + pre-warm/save). */}
      <SemanticCacheCard
        draft={draft}
        onDraftChange={(next) => setDraft(next)}
        data={data ?? null}
        providerGroups={providerGroups}
        canUpdateSemantic={canUpdateSemantic}
        probeResult={probeResult}
        probeLoading={probeLoading}
        onProbe={() => void handleProbe()}
        onPrewarmOpen={() => setPrewarmOpen(true)}
        onSaveClick={handleSaveClick}
        saving={saving}
        saveDisabled={saveDisabled}
      />

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
        <TabsList className={trafficTabStyles.sourceTabsList}>
          {canReadSemantic && (
            <TabsTrigger value="gateway" className={trafficTabStyles.sourceTabsTrigger}>
              <span>{t('pages:aiGateway.cache.sectionGateway.title')}</span>
              {dirty && <span className={styles.dirtyDot} aria-label="unsaved changes" />}
            </TabsTrigger>
          )}
          {canReadPromptCache && (
            <TabsTrigger value="provider" className={trafficTabStyles.sourceTabsTrigger}>
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

      <PrewarmModal open={prewarmOpen} onOpenChange={setPrewarmOpen} />

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
