/**
 * State + data wiring for the alert inbox (AlertListPage).
 *
 * Owns the filter state (state / severity / sourceType multi-selects, debounced
 * ruleId, since/until date range, advanced-panel toggle), the paged list fetch,
 * the ack/resolve row mutations, the i18n label maps, and the drawer selection.
 * The page component consumes the returned bag and renders pure layout.
 *
 * Auto-refresh: every 15s the hook calls `refetch()` so the inbox stays fresh
 * without the user hitting reload. The interval is cleared on unmount.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useDebouncedValue } from '@/hooks/useDebouncedValue';
import { useMutation } from '@/hooks/useMutation';
import { alertsApi } from '@/api/services';
import type {
  Alert,
  AlertListResponse,
  AlertSeverity,
  AlertState,
} from '@/api/services';
import {
  DEFAULT_ADMIN_LIST_PAGE_SIZE,
  type AdminListPageSize,
} from '@/components/ui';

const AUTO_REFRESH_MS = 15_000;

export function useAlertList() {
  const { t } = useTranslation();

  /* ── Filters ───────────────────────────────────────────────────────────── */
  const [states, setStates] = useState<AlertState[]>([]);
  const [severities, setSeverities] = useState<AlertSeverity[]>([]);
  const [sourceTypes, setSourceTypes] = useState<string[]>([]);
  const [ruleIdInput, setRuleIdInput] = useState('');
  const [since, setSince] = useState('');
  const [until, setUntil] = useState('');
  const [advancedOpen, setAdvancedOpen] = useState(false);
  const [offset, setOffset] = useState(0);
  const [pageLimit, setPageLimit] = useState<AdminListPageSize>(
    DEFAULT_ADMIN_LIST_PAGE_SIZE,
  );

  const debouncedRuleId = useDebouncedValue(ruleIdInput, 300);

  // Convert local datetime strings into ISO-Z so Hub receives UTC regardless
  // of the user's timezone. Empty string → undefined (omit the filter).
  const sinceIso = since ? new Date(since).toISOString() : undefined;
  const untilIso = until ? new Date(until).toISOString() : undefined;

  /* ── List fetch ────────────────────────────────────────────────────────── */
  const { data, loading, error, refetch } = useApi<AlertListResponse>(
    () =>
      alertsApi.list({
        state: states.length ? states : undefined,
        severity: severities.length ? severities : undefined,
        sourceType: sourceTypes.length ? sourceTypes : undefined,
        ruleId: debouncedRuleId || undefined,
        since: sinceIso,
        until: untilIso,
        offset,
        limit: pageLimit,
      }),
    [
      'admin',
      'alerts',
      'inbox',
      states.join(','),
      severities.join(','),
      sourceTypes.join(','),
      debouncedRuleId,
      sinceIso ?? '',
      untilIso ?? '',
      offset,
      pageLimit,
    ],
  );

  // Auto-refresh every 15s. The interval does not debounce user filter
  // changes — React Query dedupes rapidly fired refetches on the same key.
  useEffect(() => {
    const id = setInterval(() => {
      refetch();
    }, AUTO_REFRESH_MS);
    return () => clearInterval(id);
  }, [refetch]);

  /* ── Row-action mutations (refetch on success) ─────────────────────────── */
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [drawerOpen, setDrawerOpen] = useState(false);

  const { mutate: ackAlert, loading: ackLoading } = useMutation<string, Alert>(
    (id) => alertsApi.ack(id),
    {
      onSuccess: () => refetch(),
      successMessage: t('pages:alerts.inbox.ackSuccess'),
    },
  );

  const { mutate: resolveAlert, loading: resolveLoading } = useMutation<
    string,
    Alert
  >((id) => alertsApi.resolve(id), {
    onSuccess: () => refetch(),
    successMessage: t('pages:alerts.inbox.resolveSuccess'),
  });

  const rows = data?.alerts ?? [];
  const total = data?.total ?? 0;

  const stateLabel = useMemo<Record<AlertState, string>>(
    () => ({
      firing: t('pages:alerts.inbox.states.firing'),
      acknowledged: t('pages:alerts.inbox.states.acknowledged'),
      resolved: t('pages:alerts.inbox.states.resolved'),
    }),
    [t],
  );
  const severityLabel = useMemo<Record<AlertSeverity, string>>(
    () => ({
      critical: t('pages:alerts.inbox.severities.critical'),
      high: t('pages:alerts.inbox.severities.high'),
      medium: t('pages:alerts.inbox.severities.medium'),
      low: t('pages:alerts.inbox.severities.low'),
      info: t('pages:alerts.inbox.severities.info'),
    }),
    [t],
  );

  /* ── Filter handlers reset paging ──────────────────────────────────────── */
  const resetPaging = () => setOffset(0);

  const onStatesChange = useCallback((next: string[]) => {
    setStates(next as AlertState[]);
    resetPaging();
  }, []);
  const onSeveritiesChange = useCallback((next: string[]) => {
    setSeverities(next as AlertSeverity[]);
    resetPaging();
  }, []);
  const onSourceTypesChange = useCallback((next: string[]) => {
    setSourceTypes(next);
    resetPaging();
  }, []);
  const onRuleIdChange = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    setRuleIdInput(e.target.value);
    resetPaging();
  }, []);
  const onSinceChange = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    setSince(e.target.value);
    resetPaging();
  }, []);
  const onUntilChange = useCallback((e: React.ChangeEvent<HTMLInputElement>) => {
    setUntil(e.target.value);
    resetPaging();
  }, []);

  const resetAdvancedFilters = useCallback(() => {
    setStates([]);
    setSeverities([]);
    setSourceTypes([]);
    setSince('');
    setUntil('');
    resetPaging();
  }, []);

  const confirmAdvancedFilters = useCallback(() => {
    resetPaging();
    setAdvancedOpen(false);
    refetch();
  }, [refetch]);

  const clearRuleId = useCallback(() => {
    setRuleIdInput('');
    resetPaging();
  }, []);

  const openDrawer = useCallback((row: Alert) => {
    setSelectedId(row.id);
    setDrawerOpen(true);
  }, []);

  const closeDrawer = useCallback(() => {
    setDrawerOpen(false);
  }, []);

  return {
    t,
    // filter state
    states,
    severities,
    sourceTypes,
    ruleIdInput,
    since,
    until,
    advancedOpen,
    setAdvancedOpen,
    // paging
    offset,
    setOffset,
    pageLimit,
    setPageLimit,
    // fetch result
    data,
    loading,
    error,
    refetch,
    rows,
    total,
    // labels
    stateLabel,
    severityLabel,
    // mutations
    ackAlert,
    ackLoading,
    resolveAlert,
    resolveLoading,
    // drawer
    selectedId,
    drawerOpen,
    openDrawer,
    closeDrawer,
    // handlers
    onStatesChange,
    onSeveritiesChange,
    onSourceTypesChange,
    onRuleIdChange,
    onSinceChange,
    onUntilChange,
    resetAdvancedFilters,
    confirmAdvancedFilters,
    clearRuleId,
  };
}
