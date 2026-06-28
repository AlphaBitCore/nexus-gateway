/**
 * LogsTab — Nodes detail "Logs" tab.
 *
 * The selected time range lives in URL query params `from` / `to` so it
 * survives tab switches and is shared with the Metrics tab.
 */
import { Fragment, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useSearchParams } from 'react-router-dom';

import { useApi } from '@/hooks/useApi';
import { diagEventsApi } from '@/api/services/infrastructure/diag/diagevents';
import type {
  DiagEvent, DiagEventListResponse, DiagLevel,
} from '@/api/services/infrastructure/diag/diagevents';
import {
  Card, Stack, Button, Badge, Select, Input, Dialog,
  LoadingSpinner, ErrorBanner, Popover, PopoverTrigger, PopoverContent,
} from '@/components/ui';
import styles from './LogsTab.module.css';

const ALL = '__all__';
const PAGE_LIMIT = 50;

const LEVEL_OPTIONS: ReadonlyArray<{ value: string; key: string }> = [
  { value: 'error+fatal', key: 'levelErrorFatal' },
  { value: 'error', key: 'levelError' },
  { value: 'fatal', key: 'levelFatal' },
  { value: 'warn', key: 'levelWarn' },
  { value: 'info', key: 'levelInfo' },
  { value: 'debug', key: 'levelDebug' },
];

const SOURCE_OPTIONS = [
  'agent', 'ai-gateway', 'compliance-proxy', 'control-plane', 'nexus-hub', 'relay', 'main',
];

const RANGE_PRESETS: ReadonlyArray<{ key: string; ms: number; labelKey: string }> = [
  { key: '1h', ms: 60 * 60 * 1000, labelKey: 'range1h' },
  { key: '6h', ms: 6 * 60 * 60 * 1000, labelKey: 'range6h' },
  { key: '1d', ms: 24 * 60 * 60 * 1000, labelKey: 'range1d' },
  { key: '7d', ms: 7 * 24 * 60 * 60 * 1000, labelKey: 'range7d' },
  { key: '30d', ms: 30 * 24 * 60 * 60 * 1000, labelKey: 'range30d' },
];

const DEFAULT_RANGE = RANGE_PRESETS[0];

interface CursorState {
  errorCursor: string | null;
  fatalCursor: string | null;
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
  return `${s.slice(0, max - 1)}...`;
}

function mergeEvents(a: DiagEvent[], b: DiagEvent[]): DiagEvent[] {
  const seen = new Set<string>();
  const out: DiagEvent[] = [];
  for (const event of [...a, ...b]) {
    if (seen.has(event.id)) continue;
    seen.add(event.id);
    out.push(event);
  }
  out.sort((x, y) => (x.occurredAt < y.occurredAt ? 1 : x.occurredAt > y.occurredAt ? -1 : 0));
  return out;
}

export interface LogsTabProps {
  thingId: string;
}

export function LogsTab({ thingId }: LogsTabProps) {
  const { t } = useTranslation('pages');
  const [searchParams, setSearchParams] = useSearchParams();

  const fromParam = searchParams.get('from');
  const toParam = searchParams.get('to');
  const { from, to } = useMemo(() => {
    if (fromParam && toParam) return { from: fromParam, to: toParam };
    const now = Date.now();
    return {
      from: new Date(now - DEFAULT_RANGE.ms).toISOString(),
      to: new Date(now).toISOString(),
    };
  }, [fromParam, toParam]);

  const [activeRangeKey, setActiveRangeKey] = useState<string>(DEFAULT_RANGE.key);

  const setRange = (preset: typeof RANGE_PRESETS[number]) => {
    const now = Date.now();
    const next = new URLSearchParams(searchParams);
    next.set('from', new Date(now - preset.ms).toISOString());
    next.set('to', new Date(now).toISOString());
    setSearchParams(next, { replace: true });
    setActiveRangeKey(preset.key);
  };

  const [level, setLevel] = useState<string>('error+fatal');
  const [sourceFilter, setSourceFilter] = useState<string>('');
  const [searchInput, setSearchInput] = useState('');
  const [search, setSearch] = useState('');
  const [advancedOpen, setAdvancedOpen] = useState(false);

  const [cursors, setCursors] = useState<CursorState>({ errorCursor: null, fatalCursor: null });
  const [streamPages, setStreamPages] = useState<DiagEvent[]>([]);
  const [loadingMore, setLoadingMore] = useState(false);
  const [streamError, setStreamError] = useState<string | null>(null);
  const [doneError, setDoneError] = useState(false);
  const [doneFatal, setDoneFatal] = useState(false);

  const firstPage = useApi<DiagEvent[]>(
    async () => {
      const isCombined = level === 'error+fatal';
      const baseParams = {
        nodeId: thingId,
        from,
        to,
        source: sourceFilter || undefined,
        q: search || undefined,
        limit: PAGE_LIMIT,
      };

      if (isCombined) {
        const [errResp, fatResp] = await Promise.all([
          diagEventsApi.list({ ...baseParams, level: 'error' }),
          diagEventsApi.list({ ...baseParams, level: 'fatal' }),
        ]);
        setCursors({
          errorCursor: errResp.nextCursor || null,
          fatalCursor: fatResp.nextCursor || null,
        });
        setDoneError(!errResp.nextCursor);
        setDoneFatal(!fatResp.nextCursor);
        const merged = mergeEvents(errResp.data, fatResp.data);
        setStreamPages(merged);
        return merged;
      }

      const resp = await diagEventsApi.list({ ...baseParams, level: level as DiagLevel });
      setCursors({ errorCursor: resp.nextCursor || null, fatalCursor: null });
      setDoneError(!resp.nextCursor);
      setDoneFatal(true);
      setStreamPages(resp.data);
      return resp.data;
    },
    ['admin', 'diag-events', 'thing-list', thingId, from, to, level, sourceFilter, search],
    { skip: !thingId },
  );

  const loadMore = async () => {
    setLoadingMore(true);
    setStreamError(null);
    try {
      const isCombined = level === 'error+fatal';
      const baseParams = {
        nodeId: thingId,
        from,
        to,
        source: sourceFilter || undefined,
        q: search || undefined,
        limit: PAGE_LIMIT,
      };

      if (isCombined) {
        const promises: Array<Promise<DiagEventListResponse | null>> = [
          cursors.errorCursor
            ? diagEventsApi.list({ ...baseParams, level: 'error', cursor: cursors.errorCursor })
            : Promise.resolve(null),
          cursors.fatalCursor
            ? diagEventsApi.list({ ...baseParams, level: 'fatal', cursor: cursors.fatalCursor })
            : Promise.resolve(null),
        ];
        const [errResp, fatResp] = await Promise.all(promises);
        const next: CursorState = { ...cursors };
        const incoming: DiagEvent[] = [];
        if (errResp) {
          next.errorCursor = errResp.nextCursor || null;
          if (!errResp.nextCursor) setDoneError(true);
          incoming.push(...errResp.data);
        }
        if (fatResp) {
          next.fatalCursor = fatResp.nextCursor || null;
          if (!fatResp.nextCursor) setDoneFatal(true);
          incoming.push(...fatResp.data);
        }
        setCursors(next);
        setStreamPages((prev) => mergeEvents(prev, incoming));
      } else if (cursors.errorCursor) {
        const resp = await diagEventsApi.list({
          ...baseParams,
          level: level as DiagLevel,
          cursor: cursors.errorCursor,
        });
        setCursors({ errorCursor: resp.nextCursor || null, fatalCursor: null });
        if (!resp.nextCursor) setDoneError(true);
        setStreamPages((prev) => mergeEvents(prev, resp.data));
      }
    } catch (err) {
      setStreamError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoadingMore(false);
    }
  };

  const [detailEvent, setDetailEvent] = useState<DiagEvent | null>(null);

  const submitSearch = (event: React.FormEvent) => {
    event.preventDefault();
    setSearch(searchInput);
  };

  const resetFilters = () => {
    setRange(DEFAULT_RANGE);
    setLevel('error+fatal');
    setSourceFilter('');
    setSearchInput('');
    setSearch('');
  };

  const confirmSearch = () => {
    setSearch(searchInput);
    setAdvancedOpen(false);
  };

  const events = streamPages.length > 0 ? streamPages : (firstPage.data ?? []);
  const hasMore = level === 'error+fatal'
    ? !(doneError && doneFatal)
    : !doneError;

  return (
    <Stack gap="lg">
      <form onSubmit={submitSearch}>
        <div className={styles.filterBar}>
          <div className={`${styles.filterField} ${styles.searchField}`}>
            <div className={styles.searchBox}>
              <span className={styles.searchIcon} aria-hidden="true" />
              <Input
                type="search"
                placeholder={t('infrastructure.logsTab.searchPlaceholder')}
                value={searchInput}
                onChange={(event) => setSearchInput(event.target.value)}
                aria-label={t('infrastructure.logsTab.filterSearch')}
                className={styles.searchInput}
              />
              <Popover open={advancedOpen} onOpenChange={setAdvancedOpen}>
                <PopoverTrigger asChild>
                  <button type="button" className={styles.advancedTrigger}>
                    {t('common:advancedFilters')}
                  </button>
                </PopoverTrigger>
                <PopoverContent align="end" sideOffset={8} className={styles.advancedPopover}>
                  <div className={styles.advancedFilters}>
                    <div className={styles.filterField}>
                      <span className={styles.filterLabel}>{t('infrastructure.logsTab.filterTimeRange')}</span>
                      <div className={styles.rangeButtons}>
                        {RANGE_PRESETS.map((range) => (
                          <Button
                            key={range.key}
                            type="button"
                            size="sm"
                            variant={range.key === activeRangeKey ? 'primary' : 'secondary'}
                            aria-pressed={range.key === activeRangeKey}
                            onClick={() => setRange(range)}
                          >
                            {t(`infrastructure.logsTab.${range.labelKey}`)}
                          </Button>
                        ))}
                      </div>
                    </div>

                    <div className={styles.filterField}>
                      <span className={styles.filterLabel}>{t('infrastructure.logsTab.filterLevel')}</span>
                      <Select
                        value={level}
                        onValueChange={setLevel}
                        options={LEVEL_OPTIONS.map((opt) => ({
                          value: opt.value,
                          label: t(`infrastructure.logsTab.${opt.key}`),
                        }))}
                      />
                    </div>

                    <div className={styles.filterField}>
                      <span className={styles.filterLabel}>{t('infrastructure.logsTab.filterSource')}</span>
                      <Select
                        value={sourceFilter || ALL}
                        onValueChange={(value) => setSourceFilter(value === ALL ? '' : value)}
                        options={[
                          { value: ALL, label: t('infrastructure.filterAll') },
                          ...SOURCE_OPTIONS.map((source) => ({ value: source, label: source })),
                        ]}
                      />
                    </div>

                    <div className={styles.advancedActions}>
                      <Button type="button" variant="secondary" size="sm" className={styles.advancedActionButton} onClick={resetFilters}>
                        {t('common:reset')}
                      </Button>
                      <Button type="button" variant="primary" size="sm" className={styles.advancedActionButton} onClick={confirmSearch}>
                        {t('common:confirmSearch')}
                      </Button>
                    </div>
                  </div>
                </PopoverContent>
              </Popover>
            </div>
          </div>
          <Button type="button" variant="secondary" size="sm" className={styles.refreshButton} onClick={() => firstPage.refetch()}>
            {t('common:refresh')}
          </Button>
        </div>
      </form>

      <Card>
        <Stack gap="sm">
          {firstPage.error ? (
            <ErrorBanner error={firstPage.error} onRetry={firstPage.refetch} />
          ) : firstPage.loading && events.length === 0 ? (
            <LoadingSpinner />
          ) : events.length === 0 ? (
            <div className={styles.empty}>{t('infrastructure.logsTab.empty')}</div>
          ) : (
            <table className={styles.eventsTable}>
              <thead>
                <tr>
                  <th>{t('infrastructure.logsTab.colTime')}</th>
                  <th>{t('infrastructure.logsTab.colLevel')}</th>
                  <th>{t('infrastructure.logsTab.colEventType')}</th>
                  <th>{t('infrastructure.logsTab.colSource')}</th>
                  <th>{t('infrastructure.logsTab.colMessage')}</th>
                  <th>{t('infrastructure.logsTab.colAttrs')}</th>
                </tr>
              </thead>
              <tbody>
                {events.map((event) => {
                  const levelValue = String(event.level).toLowerCase();
                  return (
                    <Fragment key={event.id}>
                      <tr
                        className={styles.eventRow}
                        onClick={() => setDetailEvent(event)}
                      >
                        <td>{fmtTime(event.occurredAt)}</td>
                        <td>
                          <Badge variant={levelValue === 'fatal' || levelValue === 'error' ? 'danger' : 'warning'}>
                            {String(event.level).toUpperCase()}
                          </Badge>
                        </td>
                        <td>{event.eventType}</td>
                        <td>{event.source}</td>
                        <td>
                          <span className={styles.messageCell}>{truncate(event.message)}</span>
                        </td>
                        <td>
                          <Button
                            type="button"
                            variant="ghost"
                            size="sm"
                            onClick={(clickEvent) => {
                              clickEvent.stopPropagation();
                              setDetailEvent(event);
                            }}
                          >
                            {t('infrastructure.logsTab.viewDetail')}
                          </Button>
                        </td>
                      </tr>
                    </Fragment>
                  );
                })}
              </tbody>
            </table>
          )}

          {streamError && <ErrorBanner message={streamError} />}

          {hasMore && events.length > 0 && (
            <div className={styles.loadMoreRow}>
              <Button
                type="button"
                variant="secondary"
                size="sm"
                loading={loadingMore}
                onClick={loadMore}
              >
                {t('infrastructure.logsTab.loadMore')}
              </Button>
            </div>
          )}
        </Stack>
      </Card>

      {detailEvent && (
        <Dialog
          open={!!detailEvent}
          onOpenChange={(open) => {
            if (!open) setDetailEvent(null);
          }}
          title={t('infrastructure.logsTab.detailTitle')}
          size="lg"
        >
          <Stack gap="sm">
            <dl className={styles.detailMeta}>
              <dt>{t('infrastructure.logsTab.colTime')}</dt>
              <dd>{fmtTime(detailEvent.occurredAt)}</dd>
              <dt>{t('infrastructure.logsTab.colLevel')}</dt>
              <dd>{String(detailEvent.level).toUpperCase()}</dd>
              <dt>{t('infrastructure.logsTab.colSource')}</dt>
              <dd>{detailEvent.source}</dd>
              <dt>{t('infrastructure.logsTab.eventType')}</dt>
              <dd>{detailEvent.eventType}</dd>
              <dt>{t('infrastructure.logsTab.messageHash')}</dt>
              <dd className={styles.codeCell}>{detailEvent.messageHash}</dd>
              <dt>{t('infrastructure.logsTab.repeatCount')}</dt>
              <dd>{detailEvent.repeatCount}</dd>
              {detailEvent.agentVersion && (
                <>
                  <dt>{t('infrastructure.logsTab.agentVersion')}</dt>
                  <dd>{detailEvent.agentVersion}</dd>
                </>
              )}
            </dl>

            <div>
              <h4 className={styles.subHeading}>{t('infrastructure.logsTab.colMessage')}</h4>
              <pre className={styles.detailJson}>{detailEvent.message}</pre>
            </div>

            {detailEvent.attrs && Object.keys(detailEvent.attrs).length > 0 && (
              <div>
                <h4 className={styles.subHeading}>{t('infrastructure.logsTab.attrs')}</h4>
                <pre className={styles.detailJson}>
                  {JSON.stringify(detailEvent.attrs, null, 2)}
                </pre>
              </div>
            )}

            {detailEvent.stackTrace && (
              <div>
                <h4 className={styles.subHeading}>{t('infrastructure.logsTab.stackTrace')}</h4>
                <pre className={styles.detailStack}>{detailEvent.stackTrace}</pre>
              </div>
            )}
          </Stack>
        </Dialog>
      )}
    </Stack>
  );
}

export default LogsTab;
