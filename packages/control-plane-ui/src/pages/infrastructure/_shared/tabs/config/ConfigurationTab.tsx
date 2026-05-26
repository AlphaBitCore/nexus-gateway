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
  AppliedConfigEntry,
  AppliedConfigResponse,
  NodeAppliedOutcome,
} from '@/api/services/infrastructure/nodes/hub';
import {
  Button,
  ErrorBanner,
  Skeleton,
  Tooltip,
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui';
import { useToast } from '@/context/ToastContext';
import { OverrideEditorDrawer } from '../../../overrides/OverrideEditorDrawer';
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

/**
 * Mirrors `configtypes.IsBlacklisted` on the Go side (the unexported
 * `nonOverridableConfigKeys` map). Keep these two lists in sync — the server
 * enforces the policy, the UI uses the list to pre-emptively grey out the row
 * + disable the override action.
 */
const NON_OVERRIDABLE: ReadonlySet<string> = new Set(['credentials', 'virtual_keys']);

type EditorState = { mode: 'add' | 'edit'; configKey: string };

function renderJson(value: unknown): string {
  if (value === null || value === undefined) return '—';
  return JSON.stringify(value, null, 2);
}

function deepEqualJson(a: unknown, b: unknown): boolean {
  if (a === b) return true;
  try {
    return JSON.stringify(a) === JSON.stringify(b);
  } catch {
    return false;
  }
}

/**
 * Detects an active override on `killswitch` whose state turns the killswitch
 * off. Used to render the bypass banner. We treat any payload whose
 * `engaged === false` as a deliberate disable; everything else (including a
 * missing field) is left to the server's policy semantics.
 */
function detectKillswitchBypass(entry: AppliedConfigEntry | undefined): boolean {
  if (!entry || !entry.override) return false;
  const state = entry.override.state as { engaged?: unknown } | null | undefined;
  return state !== null && typeof state === 'object' && state.engaged === false;
}

export function ConfigurationTab({ thingId, thingType, appliedOutcomes }: ConfigurationTabProps) {
  const { t, i18n } = useTranslation();
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
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
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
      <div className={styles.toolbar}>
        <div className={styles.counters}>
          <span>
            {t('pages:infrastructure.configuration.targetApplied', {
              target: desiredVer,
              applied: reportedVer,
            })}
          </span>
          <span>
            <span className={styles.counterDot}>{'·'}</span>
            {t('pages:infrastructure.configuration.nKeysOverridden', {
              count: overrideCount,
            })}
          </span>
          {staleCount > 0 && (
            <span>
              <span className={styles.counterDot}>{'·'}</span>
              {t('pages:infrastructure.configuration.nStale', {
                count: staleCount,
              })}
            </span>
          )}
        </div>
        <div className={styles.toolbarActions}>
          <Button
            variant="primary"
            size="sm"
            loading={resyncingAll}
            disabled={resyncingKey !== null}
            onClick={handleResyncAll}
          >
            {t('pages:infrastructure.configuration.forceResyncAll')}
          </Button>
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button
                variant="secondary"
                size="sm"
                disabled={addableKeys.length === 0}
              >
                {t('pages:infrastructure.configuration.addOverride')}
                {' ▾'}
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end">
              {addableKeys.map((k) => (
                <DropdownMenuItem
                  key={k}
                  onSelect={() => openEditor('add', k)}
                >
                  <code>{k}</code>
                </DropdownMenuItem>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>

      {showKillswitchBanner && killswitchEntry?.override && (
        <div className={styles.killswitchBanner} role="alert">
          <span className={styles.killswitchIcon} aria-hidden="true">{'⚠'}</span>
          <span>
            {t('pages:infrastructure.configuration.killswitchBypassBanner', {
              actor: killswitchEntry.override.setBy,
              when: new Date(killswitchEntry.override.setAt).toLocaleString(i18n.language),
            })}
          </span>
        </div>
      )}

      <div className={styles.tableWrap}>
        <div className={styles.table} role="table">
          <div className={styles.headerRow} role="row">
            <div className={styles.headerCell} role="columnheader">
              {t('pages:infrastructure.configuration.colKey')}
            </div>
            <div className={styles.headerCell} role="columnheader">
              {t('pages:infrastructure.configuration.colTemplate')}
            </div>
            <div className={styles.headerCell} role="columnheader">
              {t('pages:infrastructure.configuration.colOverride')}
            </div>
            <div className={styles.headerCell} role="columnheader">
              {t('pages:infrastructure.configuration.colApplied')}
            </div>
            <div className={styles.headerCell} role="columnheader">
              {t('pages:infrastructure.configuration.actions')}
            </div>
          </div>

          {sortedKeys.map((configKey) => {
            const entry = data.configs[configKey];
            const isBlacklisted = NON_OVERRIDABLE.has(configKey);
            const hasOverride = !!entry.override;
            const stale = !!entry.override?.stale;
            const outcome = appliedOutcomes?.[configKey] ?? null;
            const applyError = outcome?.applyError ?? null;

            const appliedEqualsTemplate =
              entry.templateState !== undefined &&
              deepEqualJson(entry.appliedConfig, entry.templateState);
            const appliedEqualsOverride =
              entry.override !== undefined &&
              deepEqualJson(entry.appliedConfig, entry.override.state);

            const rowClass = [
              styles.row,
              hasOverride ? styles.overrideRow : '',
              isBlacklisted ? styles.blacklistRow : '',
              applyError ? styles.applyErrorRow : '',
            ]
              .filter(Boolean)
              .join(' ');

            return (
              <div key={configKey} className={rowClass} role="row">
                <div className={styles.cell} role="cell">
                  <div className={styles.keyCell}>
                    <div className={styles.keyHeading}>
                      <code className={styles.keyCode}>{configKey}</code>
                      {hasOverride && (
                        <span className={styles.overrideBadge}>
                          {t('pages:infrastructure.configuration.overrideBadge')}
                        </span>
                      )}
                      {stale && (
                        <span className={styles.staleBadge}>
                          {t('pages:infrastructure.configuration.staleBadge')}
                        </span>
                      )}
                      {isBlacklisted && (
                        <span className={styles.globalOnlyBadge}>
                          {t('pages:infrastructure.configuration.globalOnlyBadge')}
                        </span>
                      )}
                    </div>
                    <span className={styles.keyMeta}>
                      {t('pages:infrastructure.configuration.templateNoteAtVer', {
                        n: entry.templateVer ?? 0,
                      })}
                    </span>
                    {outcome?.appliedAt && (
                      <span className={styles.keyMeta}>
                        {t('pages:infrastructure.configuration.lastAppliedAt', {
                          when: new Date(outcome.appliedAt).toLocaleString(i18n.language),
                          version: outcome.appliedVersion ?? 0,
                        })}
                      </span>
                    )}
                    {applyError && (
                      <span
                        className={styles.applyErrorPill}
                        title={`${applyError.message}\n@ ${new Date(applyError.at).toLocaleString(i18n.language)}`}
                      >
                        <span className={styles.applyErrorIcon} aria-hidden="true">{'⚠'}</span>
                        {t('pages:infrastructure.configuration.applyErrorBadge')}
                      </span>
                    )}
                  </div>
                </div>

                <div className={styles.cell} role="cell">
                  <pre
                    className={styles.jsonCell}
                    data-testid={`config-template-${configKey}`}
                  >
                    {renderJson(entry.templateState)}
                  </pre>
                </div>

                <div className={styles.cell} role="cell">
                  {hasOverride ? (
                    <pre
                      className={styles.jsonCell}
                      data-testid={`config-override-${configKey}`}
                    >
                      {renderJson(entry.override?.state)}
                    </pre>
                  ) : (
                    <span className={styles.dash}>{'—'}</span>
                  )}
                </div>

                <div className={styles.cell} role="cell">
                  {appliedEqualsOverride && hasOverride ? (
                    <span className={styles.equalsHint}>
                      {t('pages:infrastructure.configuration.equalsOverride')}
                    </span>
                  ) : appliedEqualsTemplate && !hasOverride ? (
                    <span className={styles.equalsHint}>
                      {t('pages:infrastructure.configuration.equalsTemplate')}
                    </span>
                  ) : (
                    <pre
                      className={styles.jsonCell}
                      data-testid={`config-applied-${configKey}`}
                    >
                      {renderJson(entry.appliedConfig)}
                    </pre>
                  )}
                </div>

                <div className={styles.cell} role="cell">
                  <div className={styles.actions}>
                    {hasOverride ? (
                      <>
                        <Button
                          variant="secondary"
                          size="sm"
                          disabled={isBlacklisted}
                          onClick={() => openEditor('edit', configKey)}
                        >
                          {t('pages:infrastructure.configuration.editOverride')}
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          loading={clearingKey === configKey}
                          disabled={isBlacklisted}
                          onClick={() => handleClearOverride(configKey)}
                        >
                          {t('pages:infrastructure.configuration.clearOverride')}
                        </Button>
                      </>
                    ) : isBlacklisted ? (
                      <Tooltip content={t('pages:infrastructure.configuration.globalOnlyTooltip')}>
                        <span>
                          <Button variant="secondary" size="sm" disabled>
                            {t('pages:infrastructure.configuration.addOverride')}
                          </Button>
                        </span>
                      </Tooltip>
                    ) : (
                      <Button
                        variant="secondary"
                        size="sm"
                        onClick={() => openEditor('add', configKey)}
                      >
                        {t('pages:infrastructure.configuration.addOverride')}
                      </Button>
                    )}
                    <Button
                      variant="ghost"
                      size="sm"
                      loading={resyncingKey === configKey}
                      disabled={resyncingAll}
                      onClick={() => handleResyncKey(configKey)}
                    >
                      {entry.inSync
                        ? t('pages:infrastructure.configuration.forceResync')
                        : t('pages:infrastructure.configuration.syncNow')}
                    </Button>
                  </div>
                </div>
              </div>
            );
          })}
        </div>
      </div>

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
