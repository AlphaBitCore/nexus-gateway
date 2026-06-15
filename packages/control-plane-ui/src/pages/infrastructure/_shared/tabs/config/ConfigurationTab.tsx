/**
 * ConfigurationTab — single tab on Node Detail that subsumes the
 * legacy "Config Sync" + "Applied Config" tabs.
 *
 * Renders one row per templated config_key with a 4-column merged view:
 *
 *   Key | Template default | Override (active) | Applied | Actions
 *
 * Behaviour summary (spec: node-per-thing-override-and-force-sync-design §8.2 + §8.5):
 *
 *  - Toolbar shows Target/Applied versions + override count + stale count, with
 *    "Force resync all" and "+ Add override ▾" actions on the right. The Add
 *    Override dropdown lists keys that don't yet have an active override and
 *    are not in the global-only blacklist.
 *  - Killswitch bypass banner — red banner appears when an override exists on
 *    `killswitch` AND that override deliberately disables the killswitch
 *    (override.state.engaged === false). Heuristic chosen because the global
 *    "engaged" flag isn't reliably observable from this endpoint without
 *    inferring from template state, so we surface override-driven bypass
 *    rather than the more conservative "any override".
 *  - Override rows get a purple stripe + light purple background, an
 *    "override" badge, and (when stale) a "stale" badge. Action set is
 *    Edit / Clear / Force resync.
 *  - Plain rows (no override) expose "+ Override" and "Force resync".
 *  - Blacklist rows (`credentials`, `virtual_keys`) are greyed; the
 *    "+ Override" action is disabled with a tooltip explaining the policy.
 *    Force resync stays enabled — replay is unrelated to override authority.
 *  - JSON cells render via JSON.stringify(value, null, 2) and auto-scroll for
 *    long payloads; no inline diff in v1.
 *
 * The override editor drawer (P-F1) is mounted via `editorState`. Save flows
 * through `hubApi.setOverride` and triggers a refetch on success.
 */
import { useState, useCallback, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { hubApi } from '@/api/services/infrastructure/nodes/hub';
import type {
  AppliedConfigResponse,
  NodeAppliedOutcome,
} from '@/api/services/infrastructure/nodes/hub';
import { ErrorBanner, Skeleton } from '@/components/ui';
import { useToast } from '@/context/ToastContext';
import { OverrideEditorDrawer } from '../../../overrides/OverrideEditorDrawer';
import { ConfigurationToolbar } from './ConfigurationToolbar';
import { KillswitchBanner } from './KillswitchBanner';
import { ConfigTable } from './ConfigTable';
import { NON_OVERRIDABLE, detectKillswitchBypass } from './configHelpers';
import type { EditorState } from './configHelpers';
import styles from './ConfigurationTab.module.css';

export interface ConfigurationTabProps {
  thingId: string;
  thingType: string;
  /**
   * Per-key apply outcomes carried from the parent NodeDetail page's
   * /api/admin/nodes/:id fetch. Surfaced inline as a small badge +
   * tooltip on each row when applyError is non-null, so an operator
   * can see the exact failure beside the offending config_key without
   * leaving the Configuration tab. May be null when the node has not
   * yet reported an outcome (fresh process) or when the field is
   * absent from a stale response.
   */
  appliedOutcomes?: Record<string, NodeAppliedOutcome> | null;
}

export function ConfigurationTab({ thingId, thingType, appliedOutcomes }: ConfigurationTabProps) {
  const { t } = useTranslation();
  const { addToast } = useToast();

  const { data, loading, error, refetch } = useApi<AppliedConfigResponse>(
    () => hubApi.getAppliedConfig(thingId),
    ['admin', 'thing-applied-config', thingId],
    { skip: !thingId },
  );

  const [resyncingKey, setResyncingKey] = useState<string | null>(null);
  const [resyncingAll, setResyncingAll] = useState(false);
  const [clearingKey, setClearingKey] = useState<string | null>(null);
  // P-F1 mounts OverrideEditorDrawer when this is non-null; the drawer reads
  // mode + configKey + the live AppliedConfigEntry off `data.configs`.
  const [editorState, setEditorState] = useState<EditorState | null>(null);

  const handleResyncKey = useCallback(async (configKey: string) => {
    if (!thingId) return;
    setResyncingKey(configKey);
    try {
      await hubApi.resyncNodeAll(thingId, { configKey });
      addToast(t('pages:infrastructure.configuration.resyncSuccess'), 'success');
      refetch();
    } catch (err) {
      const msg = err instanceof Error && err.message
        ? err.message
        : t('pages:infrastructure.configuration.mutationFailed');
      addToast(msg, 'error');
    } finally {
      setResyncingKey(null);
    }
  }, [thingId, addToast, t, refetch]);

  const handleResyncAll = useCallback(async () => {
    if (!thingId) return;
    setResyncingAll(true);
    try {
      const res = await hubApi.resyncNodeAll(thingId);
      const failedCount = res.failed?.length ?? 0;
      const pushedCount = res.keyCount ?? 0;
      if (failedCount > 0) {
        // Partial success: Hub returned 200 with at least one per-key
        // delivery failure. Surface the first failed configKey + a count
        // of any remaining failures so the operator knows what to retry.
        const firstFailed = res.failed?.[0]?.configKey ?? '';
        const others = Math.max(0, failedCount - 1);
        addToast(
          t('pages:infrastructure.configuration.resyncAllPartial', {
            pushed: pushedCount,
            total: pushedCount + failedCount,
            firstKey: firstFailed,
            others,
          }),
          'warning',
        );
      } else {
        addToast(
          t('pages:infrastructure.configuration.resyncAllSuccess', {
            count: pushedCount,
          }),
          'success',
        );
      }
      refetch();
    } catch (err) {
      const msg = err instanceof Error && err.message
        ? err.message
        : t('pages:infrastructure.configuration.mutationFailed');
      addToast(msg, 'error');
    } finally {
      setResyncingAll(false);
    }
  }, [thingId, addToast, t, refetch]);

  const handleClearOverride = useCallback(async (configKey: string) => {
    if (!thingId) return;
    setClearingKey(configKey);
    try {
      await hubApi.clearOverride(thingId, configKey);
      addToast(t('pages:infrastructure.configuration.clearSuccess'), 'success');
      refetch();
    } catch (err) {
      const msg = err instanceof Error && err.message
        ? err.message
        : t('pages:infrastructure.configuration.mutationFailed');
      addToast(msg, 'error');
    } finally {
      setClearingKey(null);
    }
  }, [thingId, addToast, t, refetch]);

  const openEditor = useCallback((mode: 'add' | 'edit', configKey: string) => {
    setEditorState({ mode, configKey });
  }, []);

  const closeEditor = useCallback(() => {
    setEditorState(null);
  }, []);

  const handleEditorSaved = useCallback(() => {
    setEditorState(null);
    refetch();
  }, [refetch]);

  const sortedKeys = useMemo(
    () => (data ? Object.keys(data.configs).sort() : []),
    [data],
  );

  const overrideCount = useMemo(
    () =>
      sortedKeys.reduce((acc, k) => (data?.configs[k]?.override ? acc + 1 : acc), 0),
    [sortedKeys, data],
  );

  const staleCount = useMemo(
    () =>
      sortedKeys.reduce((acc, k) => {
        const o = data?.configs[k]?.override;
        return o && o.stale ? acc + 1 : acc;
      }, 0),
    [sortedKeys, data],
  );

  const addableKeys = useMemo(
    () =>
      sortedKeys.filter(
        (k) => !NON_OVERRIDABLE.has(k) && !data?.configs[k]?.override,
      ),
    [sortedKeys, data],
  );

  const killswitchEntry = data?.configs.killswitch;
  const showKillswitchBanner = detectKillswitchBypass(killswitchEntry);

  if (loading && !data) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner error={error} onRetry={refetch} />;
  if (!data) return null;

  const desiredVer = data.targetVersion ?? 0;
  const reportedVer = data.appliedVersion ?? 0;

  if (sortedKeys.length === 0) {
    // Hub is the broker that pushes templates to other nodes; it does not
    // consume managed templates itself. The generic "no templates" copy
    // reads as a bug for that node, so surface a Hub-specific message.
    const emptyKey =
      thingType === 'nexus-hub'
        ? 'pages:infrastructure.configuration.noTemplatesHub'
        : 'pages:infrastructure.configuration.noTemplates';
    return <p className={styles.emptyText}>{t(emptyKey)}</p>;
  }

  return (
    <>
      <ConfigurationToolbar
        desiredVer={desiredVer}
        reportedVer={reportedVer}
        overrideCount={overrideCount}
        staleCount={staleCount}
        resyncingAll={resyncingAll}
        resyncingKey={resyncingKey}
        handleResyncAll={handleResyncAll}
        addableKeys={addableKeys}
        openEditor={openEditor}
      />

      <KillswitchBanner show={showKillswitchBanner} killswitchEntry={killswitchEntry} />

      <ConfigTable
        data={data}
        sortedKeys={sortedKeys}
        appliedOutcomes={appliedOutcomes}
        resyncingKey={resyncingKey}
        resyncingAll={resyncingAll}
        clearingKey={clearingKey}
        openEditor={openEditor}
        handleClearOverride={handleClearOverride}
        handleResyncKey={handleResyncKey}
      />

      {editorState && (() => {
        const entry = data.configs[editorState.configKey];
        if (!entry) return null;
        const templateVer = entry.templateVer ?? 0;
        return (
          <OverrideEditorDrawer
            open={true}
            thingId={thingId}
            thingType={thingType}
            configKey={editorState.configKey}
            mode={editorState.mode}
            templateState={entry.templateState}
            templateVer={templateVer}
            existingOverride={editorState.mode === 'edit' ? entry.override : undefined}
            onClose={closeEditor}
            onSaved={handleEditorSaved}
          />
        );
      })()}
    </>
  );
}
