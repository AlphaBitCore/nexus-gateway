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
import { PageHeader, Stack } from '@/components/ui';
import { rangeBounds, computeHero } from './recentErrorsHelpers';
import { HeroStats } from './HeroStats';
import { FilterPanel } from './FilterPanel';
import { IssueList } from './IssueList';
import { DetailDrawer } from './DetailDrawer';
import { EventDetail } from './EventDetail';
import { SilencesPopup } from './SilencesPopup';

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
  // is 10 because the dialog body has limited height; the operator can
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

      <FilterPanel
        timeRange={timeRange}
        setTimeRange={setTimeRange}
        nodeType={nodeType}
        setNodeType={setNodeType}
        eventType={eventType}
        setEventType={setEventType}
        onRefresh={() => { groups.refetch(); silences.refetch(); }}
      />

      <HeroStats hero={hero} timeRange={timeRange} />

      <IssueList
        filteredGroups={filteredGroups}
        rawGroupsLength={rawGroups.length}
        silencesData={silences.data}
        search={search}
        setSearch={setSearch}
        hideSilenced={hideSilenced}
        setHideSilenced={setHideSilenced}
        groupsError={groups.error}
        groupsLoading={groups.loading}
        groupsRefetch={groups.refetch}
        setShowSilencesPopup={setShowSilencesPopup}
        setDetailGroup={setDetailGroup}
        silence={silence}
        unsilence={unsilence}
      />

      {detailGroup && (
        <DetailDrawer
          detailGroup={detailGroup}
          timeRange={timeRange}
          detailPages={detailPages}
          detailCursor={detailCursor}
          detailLoading={detailLoading}
          detailError={detailError}
          loadMoreDetail={loadMoreDetail}
          setDetailGroup={setDetailGroup}
          setDetailEvent={setDetailEvent}
          silence={silence}
          unsilence={unsilence}
        />
      )}

      {detailEvent && (
        <EventDetail
          detailEvent={detailEvent}
          setDetailEvent={setDetailEvent}
          enableDiag={enableDiag}
        />
      )}

      {showSilencesPopup && (
        <SilencesPopup
          showSilencesPopup={showSilencesPopup}
          setShowSilencesPopup={setShowSilencesPopup}
          silencesData={silences.data}
          unsilenceById={unsilenceById}
        />
      )}
    </Stack>
  );
}
