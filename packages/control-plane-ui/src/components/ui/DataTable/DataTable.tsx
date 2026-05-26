import {
  useState,
  useMemo,
  useRef,
  useCallback,
  useLayoutEffect,
  type CSSProperties,
} from 'react';
import { useTranslation } from 'react-i18next';
import clsx from 'clsx';
import { useVirtualizer } from '@tanstack/react-virtual';
import { ListPagination } from '../ListPagination';
import styles from './DataTable.module.css';

/** Virtualize when row count exceeds this threshold. */
const VIRTUAL_THRESHOLD = 50;
const VIRTUAL_ROW_HEIGHT = 44;
/** Pixels from edge before we treat horizontal scroll as at start/end. */
const SCROLL_EDGE_EPS = 2;

export interface DataTableColumn<T> {
  key: string;
  label: string;
  render?: (row: T) => React.ReactNode;
  sortable?: boolean;
  /** Merged onto `<td>` (e.g. allow wrapping for long ids). */
  cellStyle?: CSSProperties;
  /** Additional CSS class applied to `<td>` cells in this column. */
  cellClassName?: string;
  /**
   * Tooltip text shown on the column header. Use to convey freshness or
   * computation rules that affect how a column should be interpreted
   * (e.g. "Health refreshes every 5 min"). When omitted on a sortable
   * column the header keeps its "Sort by …" affordance.
   */
  tooltip?: string;
}

export interface ExpandableConfig<T> {
  /** Render the expanded panel content for a given row. */
  renderExpanded: (row: T) => React.ReactNode;
  /** Controlled — set of row IDs currently expanded. */
  expandedIds: ReadonlySet<string>;
  /** Toggle a single row's expansion state. */
  onToggle: (id: string) => void;
  /** Resolve a stable string id from a row. Defaults to `String(row.id)`. */
  getRowId?: (row: T) => string;
}

export interface DataTableProps<T> {
  columns: DataTableColumn<T>[];
  data: T[];
  onRowClick?: (row: T) => void;
  emptyMessage?: string;
  pageSize?: number;
  loading?: boolean;
  hideSearch?: boolean;
  /** When true, no outer border (parent uses `listTableSection`). */
  frameless?: boolean;
  /** When true, `data` is already one server page — skip client slice and inner pagination. */
  serverPaginated?: boolean;
  /** Additional class on the root element. */
  className?: string;
  /**
   * Optional per-row HTML attribute hook. Returned object is spread onto
   * the `<tr>`; intended for `data-*` attributes that downstream CSS or
   * tests need to query. Reserved keys (`key`, `className`, `onClick`,
   * `tabIndex`, `role`, `aria-label`, `onKeyDown`) are owned by the
   * DataTable and are not overridable.
   */
  getRowProps?: (row: T) => Record<string, string | number | boolean | undefined>;
  /**
   * When set, the table renders a leading chevron column and, beneath each
   * expanded row, a colSpan-row hosting `renderExpanded(row)`. Variable row
   * heights are incompatible with @tanstack/react-virtual's fixed-height
   * estimator, so virtualization is forced off whenever this is provided.
   * If `onRowClick` is also set, the row body fires the click and only the
   * chevron toggles expansion; without `onRowClick`, the whole row toggles.
   */
  expandable?: ExpandableConfig<T>;
}

type SortDir = 'asc' | 'desc';
interface SortState {
  key: string;
  dir: SortDir;
}

function compareValues(a: unknown, b: unknown, dir: SortDir): number {
  if (a == null && b == null) return 0;
  if (a == null) return 1;
  if (b == null) return -1;

  let cmp: number;
  if (typeof a === 'number' && typeof b === 'number') {
    cmp = a - b;
  } else if (typeof a === 'boolean' && typeof b === 'boolean') {
    cmp = a === b ? 0 : a ? -1 : 1;
  } else {
    cmp = String(a).localeCompare(String(b));
  }
  return dir === 'desc' ? -cmp : cmp;
}

function getAriaSort(sort: SortState | null, colKey: string): 'ascending' | 'descending' | 'none' {
  if (!sort || sort.key !== colKey) return 'none';
  return sort.dir === 'asc' ? 'ascending' : 'descending';
}

 
export function DataTable<T extends Record<string, any>>({
  columns,
  data,
  onRowClick,
  emptyMessage,
  pageSize = 10,
  loading = false,
  hideSearch = false,
  frameless = false,
  serverPaginated = false,
  className,
  getRowProps,
  expandable,
}: DataTableProps<T>) {
  const { t } = useTranslation('common');
  const [search, setSearch] = useState('');
  const [page, setPage] = useState(0);
  const [sort, setSort] = useState<SortState | null>(null);

  const toggleSort = (key: string) => {
    setSort((prev) => {
      if (!prev || prev.key !== key) return { key, dir: 'asc' };
      if (prev.dir === 'asc') return { key, dir: 'desc' };
      return null;
    });
    setPage(0);
  };

  const filtered = useMemo(() => {
    if (!search.trim()) return data;
    const term = search.toLowerCase();
    return data.filter((row) =>
      columns.some((col) => {
        const val = col.render ? undefined : row[col.key];
        return val != null && String(val).toLowerCase().includes(term);
      }),
    );
  }, [data, search, columns]);

  const sorted = useMemo(() => {
    if (!sort) return filtered;
    return [...filtered].sort((a, b) => compareValues(a[sort.key], b[sort.key], sort.dir));
  }, [filtered, sort]);

  const totalPages = Math.max(1, Math.ceil(sorted.length / pageSize));
  const safePage = Math.min(page, totalPages - 1);
  const paged = serverPaginated ? sorted : sorted.slice(safePage * pageSize, (safePage + 1) * pageSize);

  // Variable row heights from expanded panels break the fixed-height
  // estimator in @tanstack/react-virtual; disable virtualization whenever
  // expandable rows are wired up.
  const useVirtual = !expandable && paged.length > VIRTUAL_THRESHOLD;
  const totalColCount = columns.length + (expandable ? 1 : 0);
  const resolveRowId = expandable?.getRowId ?? ((row: T) => String(row.id));
  const scrollRef = useRef<HTMLDivElement>(null);
  const [scrollEdges, setScrollEdges] = useState({ left: false, right: false });

  const refreshScrollEdges = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    const maxScroll = el.scrollWidth - el.clientWidth;
    if (maxScroll <= SCROLL_EDGE_EPS) {
      setScrollEdges((p) => (p.left || p.right ? { left: false, right: false } : p));
      return;
    }
    const left = el.scrollLeft > SCROLL_EDGE_EPS;
    const right = el.scrollLeft < maxScroll - SCROLL_EDGE_EPS;
    setScrollEdges((p) => (p.left === left && p.right === right ? p : { left, right }));
  }, []);

  useLayoutEffect(() => {
    const el = scrollRef.current;
    if (!el) return;

    refreshScrollEdges();

    const onScroll = () => {
      refreshScrollEdges();
    };
    el.addEventListener('scroll', onScroll, { passive: true });

    let ro: ResizeObserver | null = null;
    if (typeof ResizeObserver !== 'undefined') {
      ro = new ResizeObserver(() => {
        refreshScrollEdges();
      });
      ro.observe(el);
      const table = el.querySelector('table');
      if (table) {
        ro.observe(table);
      }
    } else {
      window.addEventListener('resize', refreshScrollEdges);
    }

    const id = requestAnimationFrame(() => {
      refreshScrollEdges();
    });

    return () => {
      cancelAnimationFrame(id);
      el.removeEventListener('scroll', onScroll);
      ro?.disconnect();
      if (!ro) {
        window.removeEventListener('resize', refreshScrollEdges);
      }
    };
  }, [
    refreshScrollEdges,
    data.length,
    columns.length,
    loading,
    paged.length,
    sorted.length,
    useVirtual,
    safePage,
  ]);

  const virtualizer = useVirtualizer({
    count: paged.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => VIRTUAL_ROW_HEIGHT,
    overscan: 10,
    enabled: useVirtual,
  });

  const containerClass = frameless
    ? styles.tableContainerFrameless
    : styles.tableContainer;

  function renderTh(col: DataTableColumn<T>) {
    const isSortable = col.sortable !== false;
    const indicator =
      sort?.key === col.key ? (sort.dir === 'asc' ? ' \u25B2' : ' \u25BC') : '';

    return (
      <th
        key={col.key}
        scope="col"
        className={clsx(
          styles.th,
          frameless && styles.thFrameless,
          isSortable && styles.thSortable,
        )}
        onClick={isSortable ? () => toggleSort(col.key) : undefined}
        onKeyDown={
          isSortable
            ? (e) => {
                if (e.key === 'Enter' || e.key === ' ') {
                  e.preventDefault();
                  toggleSort(col.key);
                }
              }
            : undefined
        }
        tabIndex={isSortable ? 0 : undefined}
        role="columnheader"
        aria-sort={isSortable ? getAriaSort(sort, col.key) : undefined}
        title={col.tooltip ?? (isSortable ? `Sort by ${col.label}` : undefined)}
      >
        {col.label}
        {indicator}
      </th>
    );
  }

  /* -- loading skeleton ------------------------------------------------- */
  if (data.length === 0 && loading) {
    return (
      <div className={clsx(containerClass, className)}>
        <div
          ref={scrollRef}
          className={styles.tableScroll}
          data-scroll-left={scrollEdges.left ? 'true' : undefined}
          data-scroll-right={scrollEdges.right ? 'true' : undefined}
        >
          <table className={styles.table}>
            <thead>
              <tr>
                {expandable && <th className={clsx(styles.th, styles.expandToggleCell)} aria-hidden="true" />}
                {columns.map((col) => renderTh(col))}
              </tr>
            </thead>
            <tbody>
              {Array.from({ length: 5 }).map((_, rowIdx) => (
                <tr key={rowIdx} className={styles.row}>
                  {expandable && <td className={clsx(styles.td, styles.expandToggleCell)} />}
                  {columns.map((col) => (
                    <td key={col.key} className={clsx(styles.td, col.cellClassName)} style={col.cellStyle}>
                      <div
                        className={styles.skeleton}
                        style={{
                          width: `${60 + ((rowIdx * 17 + columns.indexOf(col) * 23) % 30)}%`,
                        }}
                      />
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
    );
  }

  /* -- empty state ------------------------------------------------------ */
  if (data.length === 0) {
    return (
      <div className={clsx(containerClass, className)}>
        <div className={styles.emptyContainer}>
          <span className={styles.emptyText}>{emptyMessage ?? t('noData')}</span>
        </div>
      </div>
    );
  }

  function renderRow(row: T, i: number) {
    // getRowProps lets the consumer attach data-* attributes (e.g.
    // data-killswitch-bypass) without owning the entire <tr>. We strip
    // out keys that DataTable manages so a misuse can't override the
    // row's accessibility / event behaviour.
    const extraProps = getRowProps ? getRowProps(row) : {};
    const safeExtraProps: Record<string, string | number | boolean | undefined> = {};
    for (const [k, v] of Object.entries(extraProps)) {
      if (
        k === 'key' || k === 'className' || k === 'onClick' ||
        k === 'tabIndex' || k === 'role' || k === 'aria-label' ||
        k === 'onKeyDown'
      ) continue;
      safeExtraProps[k] = v;
    }

    const rowId = expandable ? resolveRowId(row) : '';
    const isExpanded = expandable ? expandable.expandedIds.has(rowId) : false;
    // Whole-row click toggles expansion only when the consumer didn't claim
    // the row click for something else. With both wired, the body fires
    // onRowClick and the chevron is the sole expand affordance.
    const rowToggles = expandable && !onRowClick;
    const handleRowClick = onRowClick
      ? () => onRowClick(row)
      : rowToggles
        ? () => expandable!.onToggle(rowId)
        : undefined;
    const rowIsInteractive = Boolean(handleRowClick);

    const mainTr = (
      <tr
        key={`row-${i}`}
        className={clsx(
          styles.row,
          rowIsInteractive && styles.rowClickable,
          isExpanded && styles.rowExpanded,
        )}
        onClick={handleRowClick}
        tabIndex={rowIsInteractive ? 0 : undefined}
        role={rowIsInteractive ? 'button' : undefined}
        aria-label={
          rowIsInteractive ? `View ${String(row[columns[0]?.key] ?? '')}` : undefined
        }
        aria-expanded={expandable ? isExpanded : undefined}
        onKeyDown={
          handleRowClick
            ? (e) => {
                if (e.key === 'Enter' || e.key === ' ') {
                  e.preventDefault();
                  handleRowClick();
                }
              }
            : undefined
        }
        {...safeExtraProps}
      >
        {expandable && (
          <td className={clsx(styles.td, styles.expandToggleCell)}>
            <button data-design-system-escape="primitive-internal"
              type="button"
              className={clsx(styles.expandToggle, isExpanded && styles.expandToggleOpen)}
              aria-label={isExpanded ? 'Collapse row' : 'Expand row'}
              aria-expanded={isExpanded}
              onClick={(e) => { e.stopPropagation(); expandable.onToggle(rowId); }}
            >
              <span aria-hidden="true">{isExpanded ? '▼' : '▶'}</span>
            </button>
          </td>
        )}
        {columns.map((col) => {
          const cellContent = col.render
            ? col.render(row)
            : String(row[col.key] ?? '');
          const textValue =
            typeof cellContent === 'string'
              ? cellContent
              : String(row[col.key] ?? '');
          return (
            <td
              key={col.key}
              className={clsx(styles.td, col.cellClassName)}
              style={col.cellStyle}
              title={textValue}
            >
              {cellContent}
            </td>
          );
        })}
      </tr>
    );
    if (!expandable || !isExpanded) return mainTr;
    return [
      mainTr,
      <tr key={`expanded-${i}`} className={styles.expandedPanelRow}>
        <td colSpan={totalColCount} className={styles.expandedPanelCell}>
          {expandable.renderExpanded(row)}
        </td>
      </tr>,
    ];
  }

  return (
    <div className={clsx(styles.root, className)}>
      {/* Search toolbar */}
      {!hideSearch && (
        <div className={styles.searchToolbar}>
          <input
            type="text"
            placeholder={t('filterPlaceholder')}
            value={search}
            onChange={(e) => {
              setSearch(e.target.value);
              setPage(0);
            }}
            className={styles.searchInput}
          />
        </div>
      )}

      {/* Table */}
      <div className={containerClass}>
        <div
          ref={scrollRef}
          className={styles.tableScroll}
          style={useVirtual ? { maxHeight: '600px', overflow: 'auto' } : undefined}
          data-scroll-left={scrollEdges.left ? 'true' : undefined}
          data-scroll-right={scrollEdges.right ? 'true' : undefined}
        >
          <table className={styles.table}>
            <thead>
              <tr>
                {expandable && <th className={clsx(styles.th, styles.expandToggleCell)} aria-hidden="true" />}
                {columns.map((col) => renderTh(col))}
              </tr>
            </thead>
            {useVirtual ? (
              <tbody style={{ height: `${virtualizer.getTotalSize()}px`, position: 'relative' }}>
                {virtualizer.getVirtualItems().map((virtualRow) => {
                  const row = paged[virtualRow.index];
                  return (
                    <tr
                      key={virtualRow.index}
                      className={clsx(styles.row, onRowClick && styles.rowClickable)}
                      style={{
                        position: 'absolute',
                        top: 0,
                        left: 0,
                        width: '100%',
                        height: `${virtualRow.size}px`,
                        transform: `translateY(${virtualRow.start}px)`,
                        display: 'table-row',
                      }}
                      onClick={onRowClick ? () => onRowClick(row) : undefined}
                      tabIndex={onRowClick ? 0 : undefined}
                      role={onRowClick ? 'button' : undefined}
                      aria-label={
                        onRowClick ? `View ${String(row[columns[0]?.key] ?? '')}` : undefined
                      }
                      onKeyDown={
                        onRowClick
                          ? (e) => {
                              if (e.key === 'Enter' || e.key === ' ') {
                                e.preventDefault();
                                onRowClick(row);
                              }
                            }
                          : undefined
                      }
                    >
                      {columns.map((col) => {
                        const cellContent = col.render
                          ? col.render(row)
                          : String(row[col.key] ?? '');
                        const textValue =
                          typeof cellContent === 'string'
                            ? cellContent
                            : String(row[col.key] ?? '');
                        return (
                          <td
                            key={col.key}
                            className={clsx(styles.td, col.cellClassName)}
                            style={col.cellStyle}
                            title={textValue}
                          >
                            {cellContent}
                          </td>
                        );
                      })}
                    </tr>
                  );
                })}
              </tbody>
            ) : (
              <tbody>
                {paged.length === 0 ? (
                  <tr>
                    <td colSpan={totalColCount} className={styles.noResultsCell}>
                      {t('noResultsMatching', { term: search })}
                    </td>
                  </tr>
                ) : (
                  paged.map((row, i) => renderRow(row, i))
                )}
              </tbody>
            )}
          </table>
        </div>
      </div>

      {/* Client-side slice only: when data fits one slice, parent owns the footer (e.g. server offset/limit). */}
      {!serverPaginated && sorted.length > pageSize ? (
        <ListPagination
          showLimitSelect={false}
          offset={safePage * pageSize}
          limit={pageSize}
          total={sorted.length}
          pageSizeOptions={[pageSize]}
          onOffsetChange={(next) => setPage(Math.floor(next / pageSize))}
          onLimitChange={() => {}}
        />
      ) : null}
    </div>
  );
}
