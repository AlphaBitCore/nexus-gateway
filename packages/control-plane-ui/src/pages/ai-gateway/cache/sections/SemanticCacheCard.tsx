import { useTranslation } from 'react-i18next';
import { Card, Button, Switch, Input, Select, Tooltip } from '@/components/ui';
import type { AdminModelsByProvider, SemanticCacheConfig, ProbeResult } from '@/api/types';
import type { Draft, VaryBy, EmbedStrategy } from './semanticCacheDraft';
import styles from '../CachePage.module.css';

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

interface SemanticCacheCardProps {
  draft: Draft;
  onDraftChange: (next: Draft) => void;
  data: SemanticCacheConfig | null;
  providerGroups: AdminModelsByProvider[];
  canUpdateSemantic: boolean;
  probeResult: ProbeResult | null;
  probeLoading: boolean;
  onProbe: () => void;
  onPrewarmOpen: () => void;
  onSaveClick: () => void;
  saving: boolean;
  saveDisabled: boolean;
}

export function SemanticCacheCard({
  draft,
  onDraftChange,
  data,
  providerGroups,
  canUpdateSemantic,
  probeResult,
  probeLoading,
  onProbe,
  onPrewarmOpen,
  onSaveClick,
  saving,
  saveDisabled,
}: SemanticCacheCardProps) {
  const { t } = useTranslation();

  const hasProviderAndModel = !!draft.embeddingProviderId && !!draft.embeddingModelId;
  const killSwitchDisabled = !hasProviderAndModel;
  const fingerprintShort = data?.embeddingFingerprint ? data.embeddingFingerprint.slice(-8) : '—';
  const showReindexWarning =
    data !== null &&
    (draft.embeddingProviderId !== (data.embeddingProviderId ?? null) ||
      draft.embeddingModelId !== (data.embeddingModelId ?? null));

  return (
    <section className={styles.cacheCardSection}>
      <div className={styles.cacheCardHeader}>
        <h3 className={styles.cardHeading}>
          {t('pages:aiGateway.cache.semantic.cardTitle')}
        </h3>
        <p className={styles.cardSubtitle}>
          {t('pages:aiGateway.cache.semantic.cardSubtitle')}
        </p>
        <p className={styles.perfTip} role="note">
          {t('pages:aiGateway.cache.perfTip')}
        </p>
      </div>

      <Card>
        <div className={styles.settingsGrid}>
        {/* ── Embedding model ──────────────────────────────────────────── */}
        <div className={styles.settingsDivider}>
          {t('pages:aiGateway.cache.embedding.cardTitle')}
        </div>

        <div className={styles.settingField}>
          <label className={styles.settingLabel}>
            {t('pages:aiGateway.cache.embedding.providerLabel')}
          </label>
          <div className={styles.settingControl}>
            <Select
              value={draft.embeddingProviderId ?? ''}
              onValueChange={(v) =>
                onDraftChange({
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
        </div>

        <div className={styles.settingField}>
          <label className={styles.settingLabel}>
            {t('pages:aiGateway.cache.embedding.modelLabel')}
          </label>
          <div className={styles.settingControl}>
            <Select
              value={draft.embeddingModelId ?? ''}
              onValueChange={(v) =>
                onDraftChange({ ...draft, embeddingModelId: v === '' ? null : v })
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
                onClick={() => void onProbe()}
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
                <span className={styles.helpIcon} aria-label={t('pages:cache.embeddingDetailsAria')}>i</span>
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

        <div className={styles.settingField}>
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
              onCheckedChange={(checked) => onDraftChange({ ...draft, enabled: checked })}
              disabled={!canUpdateSemantic}
              aria-label={t('pages:aiGateway.cache.semantic.killSwitch')}
            />
          )}
          </div>
        </div>

        <div className={styles.settingField}>
          <label className={styles.settingLabel}>
            {t('pages:aiGateway.cache.semantic.thresholdLabel')}
            <Tooltip content={t('pages:aiGateway.cache.semantic.thresholdHelp')}>
              <span className={styles.helpIcon} aria-label={t('pages:cache.moreInfoAria')}>?</span>
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
                onDraftChange({ ...draft, threshold: isNaN(v) ? draft.threshold : v });
              }}
              aria-label={t('pages:aiGateway.cache.semantic.thresholdLabel')}
              className={styles.shortInput}
            />
          </div>
        </div>

        <div className={styles.settingField}>
          <label className={styles.settingLabel}>
            {t('pages:aiGateway.cache.semantic.allowCrossModelLabel')}
            <Tooltip content={t('pages:aiGateway.cache.semantic.allowCrossModelHelp')}>
              <span className={styles.helpIcon} aria-label={t('pages:cache.moreInfoAria')}>?</span>
            </Tooltip>
          </label>
          <div className={styles.settingControl}>
            <Switch
              checked={draft.allowCrossModel}
              onCheckedChange={(checked) => onDraftChange({ ...draft, allowCrossModel: checked })}
              disabled={!canUpdateSemantic}
              aria-label={t('pages:aiGateway.cache.semantic.allowCrossModelLabel')}
            />
          </div>
        </div>

        <div className={styles.settingField}>
          <label className={styles.settingLabel}>
            {t('pages:aiGateway.cache.semantic.varyByLabel')}
            <Tooltip content={t('pages:aiGateway.cache.semantic.varyByHelp')}>
              <span className={styles.helpIcon} aria-label={t('pages:cache.moreInfoAria')}>?</span>
            </Tooltip>
          </label>
          <div className={styles.settingControl}>
            <Select
              value={draft.varyBy}
              disabled={!canUpdateSemantic}
              onValueChange={(v) => onDraftChange({ ...draft, varyBy: v as VaryBy })}
              options={[
                { value: 'vk', label: 'vk (single-tenant default)' },
                { value: 'user', label: 'user' },
                { value: 'org', label: 'org' },
                { value: 'none', label: 'none (no isolation)' },
              ]}
              className={styles.mediumSelect}
            />
          </div>
        </div>

        <div className={styles.settingField}>
          <label className={styles.settingLabel}>
            {t('pages:aiGateway.cache.semantic.embedStrategyLabel')}
            <Tooltip content={t('pages:aiGateway.cache.semantic.embedStrategyHelp')}>
              <span className={styles.helpIcon} aria-label={t('pages:cache.moreInfoAria')}>?</span>
            </Tooltip>
          </label>
          <div className={styles.settingControl}>
            <Select
              value={draft.embedStrategy}
              disabled={!canUpdateSemantic}
              onValueChange={(v) => onDraftChange({ ...draft, embedStrategy: v as EmbedStrategy })}
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
      </div>

      {/* Actions — right-aligned */}
        <div className={styles.settingsActions}>
          {data?.enabled && (
            <Button
              variant="secondary"
              onClick={onPrewarmOpen}
              disabled={!canUpdateSemantic}
            >
              {t('pages:aiGateway.cache.prewarm.openButton')}
            </Button>
          )}
          <Button onClick={onSaveClick} loading={saving} disabled={saveDisabled}>
            {t('common:save')}
          </Button>
        </div>
      </Card>
    </section>
  );
}
