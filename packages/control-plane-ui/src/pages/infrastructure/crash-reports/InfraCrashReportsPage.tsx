/**
 * InfraCrashReportsPage — version × OS cohort view of agent crashes.
 *
 * The page reads two endpoints:
 *   1. `/api/admin/diag-events/crash-cohorts` for the cohort table.
 *   2. `/api/admin/diag-events?level=fatal&from=...&to=...` (lazily, only
 *      when a cohort row is expanded) and filters client-side by
 *      `agentVersion + osInfo.os + osInfo.version` — the CP list endpoint
 *      currently exposes neither `eventType` nor `agentVersion` filters, so
 *      we keep the API surface stable and slice the result locally.
 *
 * Click an event row → modal with the full stack trace and attrs.
 */
import { Fragment, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { diagEventsApi } from '@/api/services/infrastructure/diag/diagevents';
import type {
  CrashCohort,
  DiagEvent,
  DiagEventListResponse,
} from '@/api/services/infrastructure/diag/diagevents';
import {
  PageHeader, Stack, Card, Select, Dialog, LoadingSpinner, ErrorBanner,
} from '@/components/ui';
import styles from '../recent-errors/InfraRecentErrorsPage.module.css';

const TIME_RANGE_MS: Record<string, number> = {
  '24h': 24 * 60 * 60 * 1000,
  '7d': 7 * 24 * 60 * 60 * 1000,
  '30d': 30 * 24 * 60 * 60 * 1000,
};

function rangeBounds(rangeKey: string): { from: string; to: string } {
  const ms = TIME_RANGE_MS[rangeKey] ?? TIME_RANGE_MS['7d'];
  const now = Date.now();
  return {
    from: new Date(now - ms).toISOString(),
    to: new Date(now).toISOString(),
  };
}

function fmtTime(iso: string): string {
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

function truncate(s: string, max = 160): string {
  if (s.length <= max) return s;
  return s.slice(0, max - 1) + '…';
}

/** Stable key per cohort (agentVersion / os / osVersion). */
function cohortKey(c: Pick<CrashCohort, 'agentVersion' | 'os' | 'osVersion'>): string {
  return `${c.agentVersion}|${c.os}|${c.osVersion}`;
}

/**
 * Filter the FATAL stream down to events that match a cohort. The Hub diag
 * pipeline writes `osInfo` as `{ os, version, ... }`; we accept both that
 * shape and a flat `os` / `osVersion` fallback so older fixtures don't
 * disappear from the cohort drilldown.
 */
function eventMatchesCohort(ev: DiagEvent, cohort: CrashCohort): boolean {
  if ((ev.agentVersion ?? '') !== cohort.agentVersion) return false;
  const info = (ev.osInfo ?? {}) as Record<string, unknown>;
  const evOs = String(info.os ?? '');
  const evOsVersion = String(info.version ?? info.osVersion ?? '');
  return evOs === cohort.os && evOsVersion === cohort.osVersion;
}

export default function InfraCrashReportsPage() {
  const { t } = useTranslation('pages');

  const [timeRange, setTimeRange] = useState('7d');
  const [expandedKey, setExpandedKey] = useState<string | null>(null);
  const [detailEvent, setDetailEvent] = useState<DiagEvent | null>(null);

  const { from, to } = useMemo(() => rangeBounds(timeRange), [timeRange]);

  const cohorts = useApi<CrashCohort[]>(
    () => diagEventsApi.crashCohorts({ from, to }),
    ['admin', 'diag-events', 'crash-cohorts', from, to],
  );

  const expandedCohort = useMemo<CrashCohort | null>(() => {
    if (!expandedKey) return null;
    return cohorts.data?.find((c) => cohortKey(c) === expandedKey) ?? null;
  }, [expandedKey, cohorts.data]);

  // Fatal events for the current time window — fetched once an expansion
  // happens, then sliced client-side per the cohort key.
  const fatalEvents = useApi<DiagEventListResponse>(
    () =>
      diagEventsApi.list({
        level: 'fatal',
        from,
        to,
        limit: 200,
      }),
    ['admin', 'diag-events', 'fatal-stream', from, to],
    { skip: !expandedCohort },
  );

  const cohortRows = cohorts.data ?? [];
  const cohortEvents: DiagEvent[] = expandedCohort
    ? (fatalEvents.data?.data ?? []).filter((ev) => eventMatchesCohort(ev, expandedCohort))
    : [];

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('infrastructure.crashReports.title')}
        subtitle={t('infrastructure.crashReports.description')}
      />

      <div className={styles.filterBar}>
        <div className={styles.filterField}>
          <Select
            value={timeRange}
            onValueChange={setTimeRange}
            options={[
              { value: '24h', label: t('infrastructure.crashReports.range24h') },
              { value: '7d', label: t('infrastructure.crashReports.range7d') },
              { value: '30d', label: t('infrastructure.crashReports.range30d') },
            ]}
          />
        </div>
      </div>

      <Card>
        <Stack gap="sm">
          <h3 className={styles.sectionTitle}>
            {t('infrastructure.crashReports.cohortHeading')}
          </h3>

          {cohorts.error ? (
            <ErrorBanner message={cohorts.error.message} onRetry={cohorts.refetch} />
          ) : cohorts.loading && cohortRows.length === 0 ? (
            <LoadingSpinner />
          ) : cohortRows.length === 0 ? (
            <div className={styles.empty}>{t('infrastructure.crashReports.empty')}</div>
          ) : (
            <table className={styles.expandedTable}>
              <thead>
                <tr>
                  <th>{t('infrastructure.crashReports.colAgentVersion')}</th>
                  <th>{t('infrastructure.crashReports.colOs')}</th>
                  <th>{t('infrastructure.crashReports.colOsVersion')}</th>
                  <th>{t('infrastructure.crashReports.colCrashes')}</th>
                  <th>{t('infrastructure.crashReports.colAffected')}</th>
                  <th>{t('infrastructure.crashReports.colFirstSeen')}</th>
                  <th>{t('infrastructure.crashReports.colLastSeen')}</th>
                </tr>
              </thead>
              <tbody>
                {cohortRows.map((c) => {
                  const key = cohortKey(c);
                  const isOpen = expandedKey === key;
                  return (
                    <Fragment key={key}>
                      <tr
                        onClick={() => setExpandedKey(isOpen ? null : key)}
                        style={{ cursor: 'pointer' }}
                        aria-expanded={isOpen}
                      >
                        <td>{c.agentVersion}</td>
                        <td>{c.os}</td>
                        <td>{c.osVersion}</td>
                        <td>{c.totalCrashes}</td>
                        <td>{c.affectedNodes}</td>
                        <td>{fmtTime(c.firstSeenAt)}</td>
                        <td>{fmtTime(c.lastSeenAt)}</td>
                      </tr>
                      {isOpen && (
                        <tr>
                          <td colSpan={7} className={styles.expandRowCell}>
                            <div className={styles.expandedSubTable}>
                              <p className={styles.expandedHeading}>
                                {t('infrastructure.crashReports.expandHeading')}
                              </p>
                              {fatalEvents.error ? (
                                <ErrorBanner message={fatalEvents.error.message} />
                              ) : fatalEvents.loading ? (
                                <LoadingSpinner />
                              ) : cohortEvents.length === 0 ? (
                                <div className={styles.empty}>
                                  {t('infrastructure.crashReports.noCohortEvents')}
                                </div>
                              ) : (
                                <table className={styles.expandedTable}>
                                  <thead>
                                    <tr>
                                      <th>{t('infrastructure.crashReports.colTime')}</th>
                                      <th>{t('infrastructure.crashReports.colThing')}</th>
                                      <th>{t('infrastructure.crashReports.colMessage')}</th>
                                    </tr>
                                  </thead>
                                  <tbody>
                                    {cohortEvents.map((ev) => (
                                      <tr
                                        key={ev.id}
                                        onClick={(e) => {
                                          e.stopPropagation();
                                          setDetailEvent(ev);
                                        }}
                                        style={{ cursor: 'pointer' }}
                                      >
                                        <td>{fmtTime(ev.occurredAt)}</td>
                                        <td className={styles.codeCell}>{ev.nodeId}</td>
                                        <td>
                                          <span className={styles.messageCell}>
                                            {truncate(ev.message)}
                                          </span>
                                        </td>
                                      </tr>
                                    ))}
                                  </tbody>
                                </table>
                              )}
                            </div>
                          </td>
                        </tr>
                      )}
                    </Fragment>
                  );
                })}
              </tbody>
            </table>
          )}
        </Stack>
      </Card>

      {detailEvent && (
        <Dialog
          open={!!detailEvent}
          onOpenChange={(open) => {
            if (!open) setDetailEvent(null);
          }}
          title={t('infrastructure.crashReports.detailTitle')}
          size="lg"
        >
          <Stack gap="sm">
            <dl className={styles.detailMeta}>
              <dt>{t('infrastructure.crashReports.colTime')}</dt>
              <dd>{fmtTime(detailEvent.occurredAt)}</dd>
              <dt>{t('infrastructure.crashReports.colThing')}</dt>
              <dd className={styles.codeCell}>{detailEvent.nodeId}</dd>
              <dt>{t('infrastructure.crashReports.colAgentVersion')}</dt>
              <dd>{detailEvent.agentVersion ?? '—'}</dd>
              <dt>{t('infrastructure.crashReports.messageHash')}</dt>
              <dd className={styles.codeCell}>{detailEvent.messageHash}</dd>
            </dl>
            <div>
              <h4 className={styles.expandedHeading}>
                {t('infrastructure.crashReports.colMessage')}
              </h4>
              <pre className={styles.detailJson}>{detailEvent.message}</pre>
            </div>
            {detailEvent.stackTrace && (
              <div>
                <h4 className={styles.expandedHeading}>
                  {t('infrastructure.crashReports.stackTrace')}
                </h4>
                <pre className={styles.detailStack}>{detailEvent.stackTrace}</pre>
              </div>
            )}
            {detailEvent.attrs && Object.keys(detailEvent.attrs).length > 0 && (
              <div>
                <h4 className={styles.expandedHeading}>
                  {t('infrastructure.crashReports.attrs')}
                </h4>
                <pre className={styles.detailJson}>
                  {JSON.stringify(detailEvent.attrs, null, 2)}
                </pre>
              </div>
            )}
          </Stack>
        </Dialog>
      )}
    </Stack>
  );
}
