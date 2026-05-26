/**
 * InfraRecentErrorsPage — Recent Errors / Diag triage view.
 *
 * Layout:
 *   1. Hero strip — 4 tiles (errors-per-hour + trend, active issues, top
 *      offender source, newest issue) + an aggregate per-5min sparkline.
 *      All computed client-side from /api/admin/diag-events/groups so the
 *      backend stays a single endpoint.
 *   2. Issue list — one row per (messageHash, maxLevel) group with an
 *      inline sparkline, first/last seen, "NEW" badge for issues that
 *      first appeared within the last hour, and quick actions (View /
 *      Silence / Unsilence / Enable diag). Silenced rows are faded to
 *      keep operator focus on actionable items.
 *   3. Detail dialog — full event metadata + recent occurrences when an
 *      issue row is clicked.
 *   4. Filter panel — collapsed by default. Most operators arrive
 *      triaging; search/filter is a power feature, not the front door.
 *
 * Silence semantics: a silence keyed (messageHash, level) collapses the
 * group's `silenced` flag from the server. Quick presets (1h, 24h,
 * permanent) wrap the same POST /api/admin/diag-silences endpoint.
 */
import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { diagEventsApi, diagSilencesApi } from '@/api/services/infrastructure/diag/diagevents';
import { diagModeApi } from '@/api/services/infrastructure/diag/diagmode';
import type {
  DiagEvent,
  DiagEventListResponse,
  DiagGroup,
  DiagLevel,
} from '@/api/services/infrastructure/diag/diagevents';
import {
  PageHeader, Stack, Card, Button, Badge, Select, Input,
  Dialog, LoadingSpinner, ErrorBanner,
} from '@/components/ui';
import { Sparkline } from '@/components/ui/Sparkline';
import styles from './InfraRecentErrorsPage.module.css';

const TIME_RANGE_MS: Record<string, number> = {
  '1h': 60 * 60 * 1000,
  '24h': 24 * 60 * 60 * 1000,
  '7d': 7 * 24 * 60 * 60 * 1000,
  '30d': 30 * 24 * 60 * 60 * 1000,
};

const ALL = '__all__';
const NODE_TYPE_OPTIONS = ['agent', 'ai-gateway', 'compliance-proxy', 'control-plane', 'nexus-hub'];

/** Compact local-time string for table cells. */
function fmtTime(iso: string): string {
  try { return new Date(iso).toLocaleString(); } catch { return iso; }
}

/** "3m ago" / "2h ago" relative formatter — kept short and dependency-free. */
function fmtRelative(iso: string, t: (k: string, p?: any) => string): string {
  const delta = Math.max(0, Date.now() - new Date(iso).getTime());
  const m = Math.round(delta / 60000);
  if (m < 1) return t('infrastructure.recentErrors.relativeJustNow');
  if (m < 60) return t('infrastructure.recentErrors.relativeMin', { n: m });
  const h = Math.round(m / 60);
  if (h < 48) return t('infrastructure.recentErrors.relativeHour', { n: h });
  const d = Math.round(h / 24);
  return t('infrastructure.recentErrors.relativeDay', { n: d });
}

function rangeBounds(rangeKey: string): { from: string; to: string } {
  const ms = TIME_RANGE_MS[rangeKey] ?? TIME_RANGE_MS['24h'];
  const now = Date.now();
  return { from: new Date(now - ms).toISOString(), to: new Date(now).toISOString() };
}

function levelBadgeVariant(level: string): 'danger' | 'warning' | 'info' {
  const lvl = String(level).toLowerCase();
  if (lvl === 'fatal') return 'danger';
  if (lvl === 'error') return 'danger';
  if (lvl === 'warn') return 'warning';
  return 'info';
}

/** Pluck the integer counts out of a group's buckets array. */
function bucketCounts(buckets: DiagGroup['buckets']): number[] {
  return (buckets ?? []).map((b) => b.count);
}

interface HeroStats {
  errorsThisHour: number;
  errorsPrevHour: number;
  activeIssues: number;
  topOffenderSource: string;
  topOffenderPct: number;
  newestIssue: DiagGroup | null;
  fleetSparkline: number[];
}

/**
 * Roll up the hero-tile inputs from the groups response. All groups in
 * the response cover the requested [from, to) window, so we project
 * onto the last hour from `buckets`.
 */
function computeHero(groups: DiagGroup[]): HeroStats {
  const hourAgo = Date.now() - 60 * 60 * 1000;
  const twoHrAgo = Date.now() - 2 * 60 * 60 * 1000;

  let errorsThisHour = 0;
  let errorsPrevHour = 0;
  let activeIssues = 0;
  let newestIssue: DiagGroup | null = null;
  const sourceTotals = new Map<string, number>();
  const fleetBucketCount = new Map<string, number>();

  for (const g of groups) {
    const lvl = String(g.maxLevel ?? g.level ?? '').toLowerCase();
    const isHigh = lvl === 'error' || lvl === 'fatal';
    if (isHigh && !g.silenced) activeIssues += 1;
    sourceTotals.set(g.source, (sourceTotals.get(g.source) ?? 0) + g.totalOccurrences);

    for (const b of g.buckets ?? []) {
      const ts = new Date(b.ts).getTime();
      if (isHigh && ts >= hourAgo) errorsThisHour += b.count;
      else if (isHigh && ts >= twoHrAgo && ts < hourAgo) errorsPrevHour += b.count;
      fleetBucketCount.set(b.ts, (fleetBucketCount.get(b.ts) ?? 0) + b.count);
    }

    if (!newestIssue) newestIssue = g;
    else if (new Date(g.firstSeen).getTime() > new Date(newestIssue.firstSeen).getTime()) {
      newestIssue = g;
    }
  }

  let topOffenderSource = '';
  let topOffenderTotal = 0;
  let allTotal = 0;
  for (const [src, n] of sourceTotals) {
    allTotal += n;
    if (n > topOffenderTotal) { topOffenderSource = src; topOffenderTotal = n; }
  }
  const topOffenderPct = allTotal > 0 ? topOffenderTotal / allTotal : 0;

  const fleetSparkline = Array.from(fleetBucketCount.entries())
    .sort((a, b) => (a[0] < b[0] ? -1 : 1))
    .map(([, n]) => n);

  return {
    errorsThisHour,
    errorsPrevHour,
    activeIssues,
    topOffenderSource,
    topOffenderPct,
    newestIssue,
    fleetSparkline,
  };
}

export default function InfraRecentErrorsPage() {
  const { t } = useTranslation('pages');

  // ── Filter state ──
  const [timeRange, setTimeRange] = useState('24h');
  const [nodeType, setNodeType] = useState<string>('');
  // eventType narrows to error / crash / lifecycle / watchdog. Empty
  // = "all event types". Lifecycle events (agent.startup / shutdown /
  // pause / resume / sso_login) flow through the same
  // thing_diag_event table as crash/error rows; without this filter
  // admin's view interleaves them and can't isolate either class.
  const [eventType, setEventType] = useState<string>('');
  const [search, setSearch] = useState('');
  const [filterOpen, setFilterOpen] = useState(false);
  const [hideSilenced, setHideSilenced] = useState(true);

  const { from, to } = useMemo(() => rangeBounds(timeRange), [timeRange]);

  // ── Groups (primary data source) ──
  const groups = useApi<DiagGroup[]>(
    () =>
      diagEventsApi.groups({
        from,
        to,
        nodeType: nodeType || undefined,
        eventType: eventType || undefined,
      }),
    ['admin', 'diag-events', 'groups', from, to, nodeType, eventType],
  );

  // ── Detail dialog ──
  // Affected-events list is server-paginated via the existing
  // /api/admin/diag-events cursor. We accumulate pages in `detailPages`
  // and reset whenever the operator opens a different group. Page size
  // is 25 because the dialog body has limited height; the operator can
  // hit "Load more" to walk the cursor when triaging a long tail.
  const [showSilencesPopup, setShowSilencesPopup] = useState(false);

  // Drawer (group context) + optional event-detail popup on top.
  //   * `detailGroup` non-null → drawer open. Drawer body always shows
  //     the group meta + a paginated Affected-events list inline.
  //   * `detailEvent` non-null → small popup open on top of the drawer
  //     with that event's full detail (attrs/stack/etc). Closing the
  //     popup leaves the drawer intact so the operator returns to the
  //     same scrolled position in the events list.
  const [detailGroup, setDetailGroup] = useState<DiagGroup | null>(null);
  const [detailEvent, setDetailEvent] = useState<DiagEvent | null>(null);
  const [detailPages, setDetailPages] = useState<DiagEvent[]>([]);
  const [detailCursor, setDetailCursor] = useState<string | null>(null);
  const [detailLoading, setDetailLoading] = useState(false);
  const [detailError, setDetailError] = useState<string | null>(null);
  const DETAIL_PAGE_SIZE = 10;

  // Re-sync the drawer's `detailGroup` snapshot whenever `groups.data`
  // refreshes (e.g. after a silence/unsilence). Without this the drawer
  // keeps showing the stale silenced=false state even though the table
  // behind it has flipped.
  useEffect(() => {
    if (!detailGroup || !groups.data) return;
    const fresh = groups.data.find(
      (g) => g.messageHash === detailGroup.messageHash && g.maxLevel === detailGroup.maxLevel,
    );
    if (fresh && fresh.silenced !== detailGroup.silenced) {
      setDetailGroup(fresh);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- intentionally exclude detailGroup to avoid loop
  }, [groups.data]);

  // Fetch the first page when a group is opened; reset when it closes.
  useEffect(() => {
    if (!detailGroup) {
      setDetailPages([]);
      setDetailCursor(null);
      setDetailError(null);
      setDetailLoading(false);
      return;
    }
    let cancelled = false;
    setDetailLoading(true);
    setDetailError(null);
    diagEventsApi
      .list({
        level: detailGroup.maxLevel as DiagLevel,
        from,
        to,
        q: detailGroup.sampleMessage.slice(0, 60),
        limit: DETAIL_PAGE_SIZE,
      })
      .then((resp: DiagEventListResponse) => {
        if (cancelled) return;
        setDetailPages(resp.data);
        setDetailCursor(resp.nextCursor || null);
      })
      .catch((err: Error) => {
        if (cancelled) return;
        setDetailError(err.message);
      })
      .finally(() => {
        if (cancelled) return;
        setDetailLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [detailGroup, from, to]);

  const loadMoreDetail = async () => {
    if (!detailGroup || !detailCursor) return;
    setDetailLoading(true);
    setDetailError(null);
    try {
      const resp = await diagEventsApi.list({
        level: detailGroup.maxLevel as DiagLevel,
        from,
        to,
        q: detailGroup.sampleMessage.slice(0, 60),
        limit: DETAIL_PAGE_SIZE,
        cursor: detailCursor,
      });
      // Dedupe on id in case of overlapping cursor pages (server bug-defensive).
      setDetailPages((prev) => {
        const seen = new Set(prev.map((e) => e.id));
        return [...prev, ...resp.data.filter((e) => !seen.has(e.id))];
      });
      setDetailCursor(resp.nextCursor || null);
    } catch (err) {
      setDetailError(err instanceof Error ? err.message : String(err));
    } finally {
      setDetailLoading(false);
    }
  };

  // ── Silence mutations ──
  // onSuccess refreshes BOTH groups (to flip the `silenced` flag on the
  // affected row) AND silences (to update the "Silences (N)" badge in
  // the header). detailGroup is then re-synced via the useEffect below.
  const silence = useMutation(
    async (input: { messageHash: string; level: DiagLevel; ttlSeconds: number; reason: string }) =>
      diagSilencesApi.create(input),
    {
      successMessage: t('infrastructure.recentErrors.silenced'),
      errorMessage: t('infrastructure.recentErrors.silenceFailed'),
      onSuccess: () => {
        groups.refetch();
        silences.refetch();
      },
    },
  );

  // To unsilence we look up the active silence for (hash, level) and DELETE.
  // Cheaper than a dedicated endpoint, and ListActiveDiagSilences is small.
  const silences = useApi(() => diagSilencesApi.list(), ['admin', 'diag-silences', 'list']);

  const unsilence = useMutation(
    async (input: { messageHash: string; level: string }) => {
      const match = (silences.data ?? []).find(
        (s) => s.messageHash === input.messageHash && s.level === input.level,
      );
      if (!match) throw new Error('no matching silence');
      await diagSilencesApi.remove(match.id);
    },
    {
      successMessage: t('infrastructure.recentErrors.unsilenced'),
      errorMessage: t('infrastructure.recentErrors.unsilenceFailed'),
      onSuccess: () => {
        groups.refetch();
        silences.refetch();
      },
    },
  );

  // Direct unsilence by silence id — used from the Manage Silences popup
  // where the user already has the row id and doesn't need a join lookup.
  const unsilenceById = useMutation(
    (id: string) => diagSilencesApi.remove(id),
    {
      successMessage: t('infrastructure.recentErrors.unsilenced'),
      errorMessage: t('infrastructure.recentErrors.unsilenceFailed'),
      onSuccess: () => {
        groups.refetch();
        silences.refetch();
      },
    },
  );

  // ── Diag-mode action (kept from the previous design) ──
  const enableDiag = useMutation(
    (nodeId: string) => {
      const until = new Date(Date.now() + 60 * 60 * 1000).toISOString();
      return diagModeApi.enable(nodeId, { until, reason: 'from-recent-errors' });
    },
    {
      successMessage: t('infrastructure.recentErrors.diagEnabled'),
      errorMessage: t('infrastructure.recentErrors.diagEnableFailed'),
    },
  );

  // ── Derived data ──
  const rawGroups = groups.data ?? [];

  const filteredGroups = useMemo(() => {
    const term = search.trim().toLowerCase();
    return rawGroups.filter((g) => {
      if (hideSilenced && g.silenced) return false;
      if (term && !g.sampleMessage.toLowerCase().includes(term) && !g.messageHash.toLowerCase().includes(term)) return false;
      return true;
    });
  }, [rawGroups, hideSilenced, search]);

  const hero = useMemo(() => computeHero(rawGroups), [rawGroups]);

  // ── Render ──
  return (
    <Stack gap="lg">
      <PageHeader
        title={t('infrastructure.recentErrors.title')}
        subtitle={t('infrastructure.recentErrors.description')}
      />

      {/* ── Hero strip ── */}
      <div className={styles.heroRow}>
        <div className={styles.heroTile}>
          <span className={styles.heroLabel}>{t('infrastructure.recentErrors.tileErrorsPerHour')}</span>
          <span className={styles.heroValue}>{hero.errorsThisHour}</span>
          <span className={styles.heroSub}>
            {hero.errorsThisHour > hero.errorsPrevHour ? (
              <span className={styles.heroSubUp}>↑ </span>
            ) : hero.errorsThisHour < hero.errorsPrevHour ? (
              <span className={styles.heroSubDown}>↓ </span>
            ) : null}
            {t('infrastructure.recentErrors.tileErrorsPerHourSub', { n: hero.errorsPrevHour })}
          </span>
        </div>
        <div className={styles.heroTile}>
          <span className={styles.heroLabel}>{t('infrastructure.recentErrors.tileActiveIssues')}</span>
          <span className={styles.heroValue}>{hero.activeIssues}</span>
          <span className={styles.heroSub}>{t('infrastructure.recentErrors.tileActiveIssuesSub')}</span>
        </div>
        <div className={styles.heroTile}>
          <span className={styles.heroLabel}>{t('infrastructure.recentErrors.tileTopOffender')}</span>
          <span className={styles.heroValue}>{hero.topOffenderSource || '—'}</span>
          <span className={styles.heroSub}>
            {hero.topOffenderSource
              ? t('infrastructure.recentErrors.tileTopOffenderSub', { pct: Math.round(hero.topOffenderPct * 100) })
              : t('infrastructure.recentErrors.empty')}
          </span>
        </div>
        <div className={styles.heroTile}>
          <span className={styles.heroLabel}>{t('infrastructure.recentErrors.tileNewest')}</span>
          <span className={styles.heroValue} style={{ fontSize: 'var(--g-font-size-md)', fontWeight: 'var(--g-font-weight-semibold)', whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
            {hero.newestIssue ? hero.newestIssue.sampleMessage : '—'}
          </span>
          <span className={styles.heroSub}>
            {hero.newestIssue ? fmtRelative(hero.newestIssue.firstSeen, t) : t('infrastructure.recentErrors.empty')}
          </span>
        </div>
      </div>

      {hero.fleetSparkline.length >= 2 && (
        <Card>
          <div className={styles.heroSparkRow}>
            <Sparkline
              data={hero.fleetSparkline}
              width={840}
              height={48}
              color="var(--color-danger)"
            />
            <div className={styles.heroSub}>
              {t('infrastructure.recentErrors.fleetSparkSub', { range: t(`infrastructure.recentErrors.range${timeRange}`) })}
            </div>
          </div>
        </Card>
      )}

      {/* ── Filter panel (collapsed by default; sits above Issues so the
           bar between hero and list stays the focal point for triage) ── */}
      <Card>
        <Stack gap="sm">
          <button
            type="button"
            className={styles.filterToggle}
            onClick={() => setFilterOpen((v) => !v)}
            aria-expanded={filterOpen}
          >
            {filterOpen ? '▼' : '▶'} {t('infrastructure.recentErrors.filtersHeading')}
          </button>
          {filterOpen && (
            <div className={styles.filterBar}>
              <div className={styles.filterField}>
                <span className={styles.filterLabel}>{t('infrastructure.recentErrors.filterTimeRange')}</span>
                <Select
                  value={timeRange}
                  onValueChange={setTimeRange}
                  options={[
                    { value: '1h', label: t('infrastructure.recentErrors.range1h') },
                    { value: '24h', label: t('infrastructure.recentErrors.range24h') },
                    { value: '7d', label: t('infrastructure.recentErrors.range7d') },
                    { value: '30d', label: t('infrastructure.recentErrors.range30d') },
                  ]}
                />
              </div>
              <div className={styles.filterField}>
                <span className={styles.filterLabel}>{t('infrastructure.recentErrors.filterThingType')}</span>
                <Select
                  value={nodeType || ALL}
                  onValueChange={(v) => setNodeType(v === ALL ? '' : v)}
                  options={[
                    { value: ALL, label: t('infrastructure.filterAll') },
                    ...NODE_TYPE_OPTIONS.map((s) => ({ value: s, label: s })),
                  ]}
                />
              </div>
              <div className={styles.filterField}>
                <span className={styles.filterLabel}>{t('infrastructure.recentErrors.filterEventType')}</span>
                <Select
                  value={eventType || ALL}
                  onValueChange={(v) => setEventType(v === ALL ? '' : v)}
                  options={[
                    { value: ALL, label: t('infrastructure.filterAll') },
                    { value: 'error', label: t('infrastructure.recentErrors.eventTypeError') },
                    { value: 'crash', label: t('infrastructure.recentErrors.eventTypeCrash') },
                    { value: 'lifecycle', label: t('infrastructure.recentErrors.eventTypeLifecycle') },
                    { value: 'watchdog', label: t('infrastructure.recentErrors.eventTypeWatchdog') },
                  ]}
                />
              </div>
              <div className={styles.filterField}>
                <span className={styles.filterLabel}>{t('infrastructure.recentErrors.filterShowSilenced')}</span>
                <Button
                  type="button"
                  variant={hideSilenced ? 'secondary' : 'primary'}
                  size="sm"
                  onClick={() => setHideSilenced((v) => !v)}
                >
                  {hideSilenced
                    ? t('infrastructure.recentErrors.filterShowSilencedOff')
                    : t('infrastructure.recentErrors.filterShowSilencedOn')}
                </Button>
              </div>
              <div className={styles.filterField}>
                <span className={styles.filterLabel}>&nbsp;</span>
                <Button type="button" variant="secondary" size="sm" onClick={() => { groups.refetch(); silences.refetch(); }}>
                  {t('infrastructure.recentErrors.refresh')}
                </Button>
              </div>
            </div>
          )}
        </Stack>
      </Card>

      {/* ── Issue list ── */}
      <Card>
        <Stack gap="sm">
          <div className={styles.issuesHeader}>
            <h3 className={styles.sectionTitle}>
              {t('infrastructure.recentErrors.issuesHeading', { n: filteredGroups.length })}
            </h3>
            <Stack direction="horizontal" gap="sm">
              {(silences.data?.length ?? 0) > 0 && (
                <Button
                  type="button"
                  variant="secondary"
                  size="sm"
                  className={styles.silencesPill}
                  onClick={() => setShowSilencesPopup(true)}
                >
                  🔕 {t('infrastructure.recentErrors.actionManageSilences', { n: silences.data?.length ?? 0 })}
                </Button>
              )}
              <Input
                type="search"
                className={styles.issuesSearch}
                placeholder={t('infrastructure.recentErrors.searchPlaceholder')}
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                aria-label={t('infrastructure.recentErrors.filterSearch')}
              />
            </Stack>
          </div>

          {groups.error ? (
            <ErrorBanner message={groups.error.message} onRetry={groups.refetch} />
          ) : groups.loading && rawGroups.length === 0 ? (
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
                        <Badge variant={levelBadgeVariant(g.maxLevel)}>
                          {String(g.maxLevel).toUpperCase()}
                        </Badge>
                        {isNew && !g.silenced && (
                          <span className={styles.newBadge}>{t('infrastructure.recentErrors.badgeNew')}</span>
                        )}
                        <span className={styles.issueMsg}>{g.sampleMessage}</span>
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
                              🔕 {t('infrastructure.recentErrors.actionSilence1h')}
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
                              🔕 {t('infrastructure.recentErrors.actionSilence24h')}
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

      {/* ── Issue-detail drawer (right slide-out, xl ~960px) ──
          Body is fixed: group meta + sparkline + Silence actions +
          paginated Affected-events table inline. Clicking a row in the
          events table opens a separate centered Event-detail popup on
          top of the drawer; closing that popup returns the operator to
          their place in the scrolled list.
       */}
      {detailGroup && (
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
        >
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
                    🔕 {t('infrastructure.recentErrors.actionSilence1h')}
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
                    🔕 {t('infrastructure.recentErrors.actionSilence24h')}
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
        </Dialog>
      )}

      {/* ── Event-detail popup (centered, on top of the drawer) ── */}
      {detailEvent && (
        <Dialog
          open={!!detailEvent}
          onOpenChange={(open) => { if (!open) setDetailEvent(null); }}
          title={t('infrastructure.recentErrors.eventDetailTitle')}
          size="lg"
        >
          <Stack gap="sm">
            <dl className={styles.detailMeta}>
              <dt>{t('infrastructure.recentErrors.colTime')}</dt>
              <dd>{fmtTime(detailEvent.occurredAt)}</dd>
              <dt>{t('infrastructure.recentErrors.colLevel')}</dt>
              <dd>{String(detailEvent.level).toUpperCase()}</dd>
              <dt>{t('infrastructure.recentErrors.colThing')}</dt>
              <dd className={styles.codeCell}>{detailEvent.nodeId} ({detailEvent.nodeType})</dd>
              <dt>{t('infrastructure.recentErrors.colSource')}</dt>
              <dd>{detailEvent.source}</dd>
              <dt>{t('infrastructure.recentErrors.eventType')}</dt>
              <dd>{detailEvent.eventType}</dd>
              <dt>{t('infrastructure.recentErrors.repeatCount')}</dt>
              <dd>{detailEvent.repeatCount}</dd>
            </dl>

            <div>
              <h4 className={styles.expandedHeading}>{t('infrastructure.recentErrors.colMessage')}</h4>
              <pre className={styles.detailJson}>{detailEvent.message}</pre>
            </div>

            {detailEvent.attrs && Object.keys(detailEvent.attrs).length > 0 && (
              <div>
                <h4 className={styles.expandedHeading}>{t('infrastructure.recentErrors.attrs')}</h4>
                <pre className={styles.detailJson}>{JSON.stringify(detailEvent.attrs, null, 2)}</pre>
              </div>
            )}

            {detailEvent.stackTrace && (
              <div>
                <h4 className={styles.expandedHeading}>{t('infrastructure.recentErrors.stackTrace')}</h4>
                <pre className={styles.detailStack}>{detailEvent.stackTrace}</pre>
              </div>
            )}

            {detailEvent.nodeType === 'agent' && (
              <div>
                <Button
                  type="button"
                  variant="secondary"
                  size="sm"
                  loading={enableDiag.loading}
                  onClick={() => enableDiag.mutate(detailEvent.nodeId).catch(() => undefined)}
                >
                  {t('infrastructure.recentErrors.actionEnableDiag')}
                </Button>
              </div>
            )}
          </Stack>
        </Dialog>
      )}

      {/* ── Active silences popup ──
          Shows every active silence with its expiry (or "permanent")
          and a per-row Unsilence button. Opened from the "Silences (N)"
          button in the Issues header so the operator can audit/cancel
          what they (or another admin) have ack'd.
       */}
      {showSilencesPopup && (
        <Dialog
          open={showSilencesPopup}
          onOpenChange={(open) => { if (!open) setShowSilencesPopup(false); }}
          title={t('infrastructure.recentErrors.silencesPopupTitle', { n: silences.data?.length ?? 0 })}
          size="lg"
        >
          <Stack gap="sm">
            {(silences.data?.length ?? 0) === 0 ? (
              <div className={styles.empty}>{t('infrastructure.recentErrors.silencesPopupEmpty')}</div>
            ) : (
              <table style={{ width: '100%', fontSize: 'var(--g-font-size-xs)', borderCollapse: 'collapse' }}>
                <thead>
                  <tr>
                    <th style={{ textAlign: 'left', padding: 'var(--g-space-1) var(--g-space-2)' }}>{t('infrastructure.recentErrors.colLevel')}</th>
                    <th style={{ textAlign: 'left', padding: 'var(--g-space-1) var(--g-space-2)' }}>{t('infrastructure.recentErrors.messageHash')}</th>
                    <th style={{ textAlign: 'left', padding: 'var(--g-space-1) var(--g-space-2)' }}>{t('infrastructure.recentErrors.silencedAt')}</th>
                    <th style={{ textAlign: 'left', padding: 'var(--g-space-1) var(--g-space-2)' }}>{t('infrastructure.recentErrors.silenceExpires')}</th>
                    <th style={{ textAlign: 'left', padding: 'var(--g-space-1) var(--g-space-2)' }}>{t('infrastructure.recentErrors.silenceReason')}</th>
                    <th style={{ textAlign: 'left', padding: 'var(--g-space-1) var(--g-space-2)' }}></th>
                  </tr>
                </thead>
                <tbody>
                  {(silences.data ?? []).map((s) => (
                    <tr key={s.id}>
                      <td style={{ padding: 'var(--g-space-1) var(--g-space-2)' }}>
                        <Badge variant={levelBadgeVariant(s.level)}>{String(s.level).toUpperCase()}</Badge>
                      </td>
                      <td style={{ padding: 'var(--g-space-1) var(--g-space-2)' }} className={styles.codeCell}>
                        {s.messageHash.slice(0, 12)}…
                      </td>
                      <td style={{ padding: 'var(--g-space-1) var(--g-space-2)' }}>{fmtTime(s.silencedAt)}</td>
                      <td style={{ padding: 'var(--g-space-1) var(--g-space-2)' }}>
                        {s.expiresAt
                          ? `${fmtTime(s.expiresAt)} (${fmtRelative(s.expiresAt, t)})`
                          : t('infrastructure.recentErrors.silencePermanent')}
                      </td>
                      <td style={{ padding: 'var(--g-space-1) var(--g-space-2)' }} className={styles.codeCell}>
                        {s.reason || '—'}
                      </td>
                      <td style={{ padding: 'var(--g-space-1) var(--g-space-2)', textAlign: 'right' }}>
                        <Button
                          type="button"
                          variant="ghost"
                          size="sm"
                          loading={unsilenceById.loading}
                          onClick={() => unsilenceById.mutate(s.id).catch(() => undefined)}
                        >
                          {t('infrastructure.recentErrors.actionUnsilence')}
                        </Button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Stack>
        </Dialog>
      )}
    </Stack>
  );
}
