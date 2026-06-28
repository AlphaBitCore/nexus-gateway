import { useTranslation } from 'react-i18next';
import type { DiagEvent, DiagGroup, DiagLevel } from '@/api/services/infrastructure/diag/diagevents';
import {
  Stack, Button, Badge,
  Dialog, LoadingSpinner, ErrorBanner,
} from '@/components/ui';
import { Sparkline } from '@/components/ui/Sparkline';
import { fmtTime, fmtRelative, levelBadgeVariant, bucketCounts } from './recentErrorsHelpers';
import styles from './InfraRecentErrorsPage.module.css';

interface DetailDrawerProps {
  detailGroup: DiagGroup;
  timeRange: string;
  detailPages: DiagEvent[];
  detailCursor: string | null;
  detailLoading: boolean;
  detailError: string | null;
  loadMoreDetail: () => void;
  setDetailGroup: (g: DiagGroup | null) => void;
  setDetailEvent: (e: DiagEvent | null) => void;
  silence: {
    mutate: (input: { messageHash: string; level: DiagLevel; ttlSeconds: number; reason: string }) => Promise<unknown>;
    loading: boolean;
  };
  unsilence: {
    mutate: (input: { messageHash: string; level: string }) => Promise<unknown>;
    loading: boolean;
  };
}

function SilenceIcon() {
  return (
    <svg className={styles.buttonIcon} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M18 8a6 6 0 0 0-12 0c0 7-3 7-3 7h15" />
      <path d="M13.73 21a2 2 0 0 1-3.46 0" />
      <path d="M21 3 3 21" />
    </svg>
  );
}

export function DetailDrawer({
  detailGroup,
  timeRange,
  detailPages,
  detailCursor,
  detailLoading,
  detailError,
  loadMoreDetail,
  setDetailGroup,
  setDetailEvent,
  silence,
  unsilence,
}: DetailDrawerProps) {
  const { t } = useTranslation('pages');

  return (
    /* ── Issue-detail drawer (right slide-out, xl ~960px) ──
        Body is fixed: group meta + sparkline + Silence actions +
        paginated Affected-events table inline. Clicking a row in the
        events table opens a separate centered Event-detail popup on
        top of the drawer; closing that popup returns the operator to
        their place in the scrolled list.
     */
    <Dialog
      open={!!detailGroup}
      onOpenChange={(open) => {
        if (!open) {
          setDetailGroup(null);
          setDetailEvent(null);
        }
      }}
      title={t('infrastructure.recentErrors.detailTitle')}
      variant="drawer"
      size="xl"
      footerClassName={styles.drawerFooter}
      footer={(
        <div className={styles.drawerFooterActions}>
          <Button
            type="button"
            variant="secondary"
            onClick={() => {
              setDetailGroup(null);
              setDetailEvent(null);
            }}
          >
            {t('common:close', 'Close')}
          </Button>
        </div>
      )}
    >
      <div className={styles.drawerBody}>
        <Stack gap="sm">
          <dl className={styles.detailMeta}>
            <dt>{t('infrastructure.recentErrors.colLevel')}</dt>
            <dd>{String(detailGroup.maxLevel).toUpperCase()}</dd>
            <dt>{t('infrastructure.recentErrors.colSource')}</dt>
            <dd>{detailGroup.source}</dd>
            <dt>{t('infrastructure.recentErrors.colAffected')}</dt>
            <dd>{detailGroup.affectedNodes}</dd>
            <dt>{t('infrastructure.recentErrors.colTotal')}</dt>
            <dd>{detailGroup.totalOccurrences}</dd>
            <dt>{t('infrastructure.recentErrors.colFirstSeen')}</dt>
            <dd>{fmtTime(detailGroup.firstSeen)} ({fmtRelative(detailGroup.firstSeen, t)})</dd>
            <dt>{t('infrastructure.recentErrors.colLastSeen')}</dt>
            <dd>{fmtTime(detailGroup.lastSeen)} ({fmtRelative(detailGroup.lastSeen, t)})</dd>
            <dt>{t('infrastructure.recentErrors.messageHash')}</dt>
            <dd className={styles.codeCell}>{detailGroup.messageHash}</dd>
          </dl>

          <div>
            <h4 className={styles.expandedHeading}>{t('infrastructure.recentErrors.colMessage')}</h4>
            <pre className={styles.detailJson}>{detailGroup.sampleMessage}</pre>
          </div>

          {detailGroup.buckets.length >= 2 && (
            <div>
              <h4 className={styles.expandedHeading}>
                {t('infrastructure.recentErrors.fleetSparkSub', { range: t(`infrastructure.recentErrors.range${timeRange}`) })}
              </h4>
              <Sparkline
                data={bucketCounts(detailGroup.buckets)}
                width={880}
                height={48}
                color="var(--color-danger)"
              />
            </div>
          )}

          <Stack direction="horizontal" gap="sm">
          {detailGroup.silenced ? (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              loading={unsilence.loading}
              onClick={() =>
                unsilence
                  .mutate({ messageHash: detailGroup.messageHash, level: detailGroup.maxLevel })
                  .catch(() => undefined)
              }
            >
              {t('infrastructure.recentErrors.actionUnsilence')}
            </Button>
          ) : (
            <>
              <Button
                type="button"
                variant="secondary"
                size="sm"
                loading={silence.loading}
                onClick={() =>
                  silence
                    .mutate({
                      messageHash: detailGroup.messageHash,
                      level: detailGroup.maxLevel as DiagLevel,
                      ttlSeconds: 60 * 60,
                      reason: 'snoozed-1h',
                    })
                    .catch(() => undefined)
                }
              >
                <SilenceIcon />
                {t('infrastructure.recentErrors.actionSilence1h')}
              </Button>
              <Button
                type="button"
                variant="secondary"
                size="sm"
                loading={silence.loading}
                onClick={() =>
                  silence
                    .mutate({
                      messageHash: detailGroup.messageHash,
                      level: detailGroup.maxLevel as DiagLevel,
                      ttlSeconds: 24 * 60 * 60,
                      reason: 'snoozed-24h',
                    })
                    .catch(() => undefined)
                }
              >
                <SilenceIcon />
                {t('infrastructure.recentErrors.actionSilence24h')}
              </Button>
            </>
          )}
          </Stack>

          <div>
          <h4 className={styles.expandedHeading}>
            {t('infrastructure.recentErrors.affectedThingsHeading')}
            {' '}
            <span className={styles.heroSub}>
              ({detailPages.length}
              {detailCursor ? '+' : ''})
            </span>
          </h4>

          {detailError && <ErrorBanner message={detailError} />}

          {detailLoading && detailPages.length === 0 ? (
            <LoadingSpinner />
          ) : detailPages.length === 0 ? (
            <div className={styles.empty}>{t('infrastructure.recentErrors.noAffected')}</div>
          ) : (
            <>
              <table style={{ width: '100%', fontSize: 'var(--g-font-size-xs)', borderCollapse: 'collapse' }}>
                <thead>
                  <tr>
                    <th style={{ textAlign: 'left', padding: 'var(--g-space-1) var(--g-space-2)', whiteSpace: 'nowrap' }}>{t('infrastructure.recentErrors.colTime')}</th>
                    <th style={{ textAlign: 'left', padding: 'var(--g-space-1) var(--g-space-2)' }}>{t('infrastructure.recentErrors.colLevel')}</th>
                    <th style={{ textAlign: 'left', padding: 'var(--g-space-1) var(--g-space-2)' }}>{t('infrastructure.recentErrors.colSource')}</th>
                    <th style={{ textAlign: 'left', padding: 'var(--g-space-1) var(--g-space-2)' }}>{t('infrastructure.recentErrors.colThing')}</th>
                    <th style={{ textAlign: 'left', padding: 'var(--g-space-1) var(--g-space-2)' }}>{t('infrastructure.recentErrors.eventType')}</th>
                    <th style={{ textAlign: 'right', padding: 'var(--g-space-1) var(--g-space-2)' }}>{t('infrastructure.recentErrors.colRepeat')}</th>
                    <th style={{ textAlign: 'left', padding: 'var(--g-space-1) var(--g-space-2)', width: 24 }}></th>
                  </tr>
                </thead>
                <tbody>
                  {detailPages.map((ev) => (
                    <tr
                      key={ev.id}
                      onClick={() => setDetailEvent(ev)}
                      style={{ cursor: 'pointer' }}
                    >
                      <td style={{ padding: 'var(--g-space-1) var(--g-space-2)', whiteSpace: 'nowrap' }}>{fmtTime(ev.occurredAt)}</td>
                      <td style={{ padding: 'var(--g-space-1) var(--g-space-2)' }}>
                        <Badge variant={levelBadgeVariant(String(ev.level))}>
                          {String(ev.level).toUpperCase()}
                        </Badge>
                      </td>
                      <td style={{ padding: 'var(--g-space-1) var(--g-space-2)' }}>{ev.source}</td>
                      <td style={{ padding: 'var(--g-space-1) var(--g-space-2)' }} className={styles.codeCell}>{ev.nodeId}</td>
                      <td style={{ padding: 'var(--g-space-1) var(--g-space-2)' }} className={styles.codeCell}>{ev.eventType}</td>
                      <td style={{ padding: 'var(--g-space-1) var(--g-space-2)', textAlign: 'right' }}>
                        {ev.repeatCount > 1 ? `×${ev.repeatCount}` : ''}
                      </td>
                      <td style={{ padding: 'var(--g-space-1) var(--g-space-2)', textAlign: 'right' }}>
                        <span className={styles.heroSub}>›</span>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>

              {detailCursor && (
                <div style={{ textAlign: 'center', paddingTop: 'var(--g-space-2)' }}>
                  <Button
                    type="button"
                    variant="secondary"
                    size="sm"
                    loading={detailLoading}
                    onClick={loadMoreDetail}
                  >
                    {t('infrastructure.recentErrors.loadMore')}
                  </Button>
                </div>
              )}
            </>
          )}
          </div>
        </Stack>
      </div>
    </Dialog>
  );
}
