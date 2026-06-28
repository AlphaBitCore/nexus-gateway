import { useTranslation } from 'react-i18next';
import type { DiagGroup, DiagLevel, DiagSilence } from '@/api/services/infrastructure/diag/diagevents';
import {
  Stack, Card, Button, Badge, Input,
  LoadingSpinner, ErrorBanner,
} from '@/components/ui';
import { Sparkline } from '@/components/ui/Sparkline';
import { fmtRelative, levelBadgeVariant, bucketCounts } from './recentErrorsHelpers';
import styles from './InfraRecentErrorsPage.module.css';

function SilenceIcon() {
  return (
    <svg className={styles.buttonIcon} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M18 8a6 6 0 0 0-12 0c0 7-3 7-3 7h15" />
      <path d="M13.73 21a2 2 0 0 1-3.46 0" />
      <path d="M21 3 3 21" />
    </svg>
  );
}

interface IssueListProps {
  filteredGroups: DiagGroup[];
  rawGroupsLength: number;
  silencesData: DiagSilence[] | null;
  search: string;
  setSearch: (v: string) => void;
  hideSilenced: boolean;
  setHideSilenced: React.Dispatch<React.SetStateAction<boolean>>;
  groupsError: Error | null;
  groupsLoading: boolean;
  groupsRefetch: () => void;
  setShowSilencesPopup: (v: boolean) => void;
  setDetailGroup: (g: DiagGroup) => void;
  silence: {
    mutate: (input: { messageHash: string; level: DiagLevel; ttlSeconds: number; reason: string }) => Promise<unknown>;
    loading: boolean;
  };
  unsilence: {
    mutate: (input: { messageHash: string; level: string }) => Promise<unknown>;
    loading: boolean;
  };
}

export function IssueList({
  filteredGroups,
  rawGroupsLength,
  silencesData,
  search,
  setSearch,
  hideSilenced,
  setHideSilenced,
  groupsError,
  groupsLoading,
  groupsRefetch,
  setShowSilencesPopup,
  setDetailGroup,
  silence,
  unsilence,
}: IssueListProps) {
  const { t } = useTranslation('pages');

  return (
    /* ── Issue list ── */
    <div className={styles.issuesSection}>
      <div className={styles.issuesHeader}>
        <Stack direction="horizontal" gap="sm" className={styles.issuesControls}>
          <div className={styles.issuesSearchBox}>
            <span className={styles.issuesSearchIcon} aria-hidden="true" />
            <Input
              type="search"
              className={styles.issuesSearch}
              placeholder={t('infrastructure.recentErrors.searchPlaceholder')}
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              aria-label={t('infrastructure.recentErrors.filterSearch')}
            />
          </div>
          <Button
            type="button"
            variant={hideSilenced ? 'secondary' : 'primary'}
            size="sm"
            className={styles.showSilencedButton}
            onClick={() => setHideSilenced((v) => !v)}
          >
            {hideSilenced
              ? t('infrastructure.recentErrors.filterShowSilencedOff')
              : t('infrastructure.recentErrors.filterShowSilencedOn')}
          </Button>
        </Stack>
        <div className={styles.issuesHeaderRight}>
          {(silencesData?.length ?? 0) > 0 && (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className={styles.silencesPill}
              onClick={() => setShowSilencesPopup(true)}
            >
              <SilenceIcon />
              {t('infrastructure.recentErrors.actionManageSilences', { n: silencesData?.length ?? 0 })}
            </Button>
          )}
          {(silencesData?.length ?? 0) > 0 && <span className={styles.issuesDivider} aria-hidden="true" />}
          <h3 className={styles.sectionTitle}>
            {t('infrastructure.recentErrors.issuesHeading', { n: filteredGroups.length })}
          </h3>
        </div>
      </div>

      <Card>
        <Stack gap="sm">
        {groupsError ? (
          <ErrorBanner message={groupsError.message} onRetry={groupsRefetch} />
        ) : groupsLoading && rawGroupsLength === 0 ? (
          <LoadingSpinner />
        ) : filteredGroups.length === 0 ? (
          <div className={styles.empty}>{t('infrastructure.recentErrors.empty')}</div>
        ) : (
          <div className={styles.issueList}>
            {filteredGroups.map((g) => {
              const isNew = new Date(g.firstSeen).getTime() > Date.now() - 60 * 60 * 1000;
              const rowClass = [
                styles.issueRow,
                g.silenced ? styles.issueRowSilenced : '',
                isNew && !g.silenced ? styles.issueRowNew : '',
              ].filter(Boolean).join(' ');

              return (
                <div
                  key={`${g.messageHash}_${g.maxLevel}`}
                  className={rowClass}
                  onClick={() => setDetailGroup(g)}
                >
                  <div className={styles.issueMain}>
                    <div className={styles.issueHead}>
                      <span className={styles.issueMsg}>{g.sampleMessage}</span>
                      <Badge variant={levelBadgeVariant(g.maxLevel)}>
                        {String(g.maxLevel).toUpperCase()}
                      </Badge>
                      {isNew && !g.silenced && (
                        <span className={styles.newBadge}>{t('infrastructure.recentErrors.badgeNew')}</span>
                      )}
                    </div>
                    <div className={styles.issueMeta}>
                      <span>{t('infrastructure.recentErrors.metaSource', { source: g.source })}</span>
                      <span>{t('infrastructure.recentErrors.metaAffected', { n: g.affectedNodes })}</span>
                      <span>{t('infrastructure.recentErrors.metaTotal', { n: g.totalOccurrences })}</span>
                      <span>{t('infrastructure.recentErrors.metaFirstSeen', { rel: fmtRelative(g.firstSeen, t) })}</span>
                      <span>{t('infrastructure.recentErrors.metaLastSeen', { rel: fmtRelative(g.lastSeen, t) })}</span>
                      {g.silenced && (
                        <span><Badge variant="outline">{t('infrastructure.recentErrors.silencedBadge')}</Badge></span>
                      )}
                    </div>
                  </div>
                  <div className={styles.issueRight}>
                    {g.buckets.length >= 2 ? (
                      <Sparkline
                        data={bucketCounts(g.buckets)}
                        width={120}
                        height={28}
                        color={isNew ? 'var(--color-warning)' : 'var(--color-danger)'}
                      />
                    ) : (
                      <span className={styles.heroSub}>—</span>
                    )}
                    <div className={styles.issueActions} onClick={(e) => e.stopPropagation()}>
                      {g.silenced ? (
                        <Button
                          type="button"
                          variant="ghost"
                          size="sm"
                          loading={unsilence.loading}
                          onClick={() => unsilence.mutate({ messageHash: g.messageHash, level: g.maxLevel }).catch(() => undefined)}
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
                            onClick={() => silence.mutate({
                              messageHash: g.messageHash,
                              level: g.maxLevel as DiagLevel,
                              ttlSeconds: 60 * 60,
                              reason: 'snoozed-1h',
                            }).catch(() => undefined)}
                          >
                            <SilenceIcon />
                            {t('infrastructure.recentErrors.actionSilence1h')}
                          </Button>
                          <Button
                            type="button"
                            variant="secondary"
                            size="sm"
                            loading={silence.loading}
                            onClick={() => silence.mutate({
                              messageHash: g.messageHash,
                              level: g.maxLevel as DiagLevel,
                              ttlSeconds: 24 * 60 * 60,
                              reason: 'snoozed-24h',
                            }).catch(() => undefined)}
                          >
                            <SilenceIcon />
                            {t('infrastructure.recentErrors.actionSilence24h')}
                          </Button>
                        </>
                      )}
                    </div>
                  </div>
                </div>
              );
            })}
          </div>
        )}
        </Stack>
      </Card>
    </div>
  );
}
