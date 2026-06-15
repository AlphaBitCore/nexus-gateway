/**
 * Card — Extract cache (L1 exact-match) fleet config.
 *
 * Surfaces the two admin-tunable knobs on the extract_cache_config
 * singleton:
 *   - enabled  (master switch — hot-toggled via Hub Category B push)
 *   - ttlSeconds (cache entry TTL, range [60, 604800])
 *
 * `applyFreshnessRules` lives on the same singleton row but is rendered
 * on the Freshness rules card — that's where it sits in the admin's
 * mental model ("does my freshness-rules editor actually do anything?").
 * Both cards POST the full payload on save so they don't overwrite each
 * other's field.
 *
 * IAM: read = admin:extract-cache.read; save = admin:extract-cache.update.
 */
import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { usePermission } from '@/hooks/usePermission';
import { useToast } from '@/context/ToastContext';
import {
  extractCacheConfigApi,
  type ExtractCacheConfig,
} from '@/api/services/cache/extractCacheConfig';
import {
  Card,
  Button,
  Switch,
  Input,
  Skeleton,
  ErrorBanner,
  Tooltip,
} from '@/components/ui';
import styles from './ExtractCacheCard.module.css';

interface Draft {
  enabled: boolean;
  ttlSeconds: number;
}

function isDraftChanged(draft: Draft, saved: ExtractCacheConfig): boolean {
  return draft.enabled !== saved.enabled || draft.ttlSeconds !== saved.ttlSeconds;
}

export function ExtractCacheCard() {
  const { t } = useTranslation();
  const { addToast } = useToast();
  const canUpdate = usePermission('extract-cache:update');

  const { data, loading, error, refetch } = useApi<ExtractCacheConfig>(
    () => extractCacheConfigApi.getConfig(),
    ['admin', 'extract-cache', 'config'],
  );

  const [draft, setDraft] = useState<Draft | null>(null);

  useEffect(() => {
    if (data) {
      setDraft({ enabled: data.enabled, ttlSeconds: data.ttlSeconds });
    }
  }, [data]);

  const { mutate: executeSave, loading: saving } = useMutation(
    (_: void) => {
      if (!draft || !data) {
        throw new Error('save called before draft initialized');
      }
      // Send full payload (including applyFreshnessRules read from saved
      // state) so the Freshness card's field isn't trampled.
      return extractCacheConfigApi.saveConfig({
        enabled: draft.enabled,
        ttlSeconds: draft.ttlSeconds,
        applyFreshnessRules: data.applyFreshnessRules,
      });
    },
    {
      invalidateQueries: [['admin', 'extract-cache', 'config']],
      onSuccess: () => {
        refetch();
        addToast(t('pages:aiGateway.cache.extract.saved'), 'success');
      },
    },
  );

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!draft || !data) return null;

  const dirty = isDraftChanged(draft, data);
  const ttlOutOfRange = draft.enabled && (draft.ttlSeconds < 60 || draft.ttlSeconds > 604800);
  const saveDisabled = !canUpdate || saving || !dirty || ttlOutOfRange;

  return (
    <section className={styles.cardSection}>
      <div className={styles.cardHeader}>
        <h3 className={styles.cardHeading}>
          {t('pages:aiGateway.cache.extract.cardTitle')}
        </h3>
        <p className={styles.cardSubtitle}>
          {t('pages:aiGateway.cache.extract.cardSubtitle')}
        </p>
      </div>

      <Card>
        <div className={styles.settingsGrid}>
          <div className={styles.settingField}>
            <label className={styles.settingLabel}>
              {t('pages:aiGateway.cache.extract.enabled')}
            </label>
            <div className={styles.settingControl}>
            <Switch
              checked={draft.enabled}
              onCheckedChange={(v) => setDraft({ ...draft, enabled: v })}
              disabled={!canUpdate}
              aria-label={t('pages:aiGateway.cache.extract.enabled')}
            />
            </div>
          </div>

          <div className={styles.settingField}>
            <label className={styles.settingLabel}>
              {t('pages:aiGateway.cache.extract.ttlLabel')}
              <Tooltip content={t('pages:aiGateway.cache.extract.ttlHelp')}>
                <span className={styles.helpIcon} aria-label={t('pages:cache.moreInfoAria')}>?</span>
              </Tooltip>
            </label>
            <div className={styles.settingControl}>
            <Input
              type="number"
              min={60}
              max={604800}
              value={String(draft.ttlSeconds)}
              disabled={!canUpdate || !draft.enabled}
              onChange={(e) => {
                const v = parseInt(e.target.value, 10);
                setDraft({ ...draft, ttlSeconds: isNaN(v) ? draft.ttlSeconds : v });
              }}
              aria-label={t('pages:aiGateway.cache.extract.ttlLabel')}
              className={styles.ttlInput}
            />
            {ttlOutOfRange && (
              <span role="alert" className={styles.rowError}>
                {t('pages:aiGateway.cache.extract.ttlOutOfRange')}
              </span>
            )}
            </div>
          </div>
        </div>

        <div className={styles.settingsActions}>
          <Button onClick={() => void executeSave(undefined)} loading={saving} disabled={saveDisabled}>
            {t('common:save')}
          </Button>
        </div>
      </Card>
    </section>
  );
}
