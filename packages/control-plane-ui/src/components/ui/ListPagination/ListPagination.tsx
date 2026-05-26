import { useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '../Button';
import { ADMIN_LIST_PAGE_SIZE_OPTIONS } from '@/constants/admin-api';
import type { AdminListPageSize } from '@/constants/admin-api';
import css from './ListPagination.module.css';

export { ADMIN_LIST_PAGE_SIZE_OPTIONS, DEFAULT_ADMIN_LIST_PAGE_SIZE } from '@/constants/admin-api';
export type { AdminListPageSize } from '@/constants/admin-api';

export type ListPaginationProps = {
  offset: number;
  limit: number;
  total: number;
  onOffsetChange: (nextOffset: number) => void;
  /** When the user picks a new page size, parent should update limit and typically reset offset (handled here via onOffsetChange(0)). */
  onLimitChange: (nextLimit: AdminListPageSize) => void;
  /** Override allowed page sizes (must match values the backend accepts, typically ≤ 200). */
  pageSizeOptions?: readonly number[];
  /** When false, hides the rows-per-page selector (e.g. client-side tables with a fixed slice size). @default true */
  showLimitSelect?: boolean;
};

function fmtInt(n: number): string {
  return n.toLocaleString();
}

function buildPageItems(page: number, pageCount: number): Array<number | 'ellipsis'> {
  if (pageCount <= 7) {
    return Array.from({ length: pageCount }, (_, idx) => idx + 1);
  }
  if (page <= 4) {
    return [1, 2, 3, 4, 5, 'ellipsis', pageCount];
  }
  if (page >= pageCount - 3) {
    return [1, 'ellipsis', pageCount - 4, pageCount - 3, pageCount - 2, pageCount - 1, pageCount];
  }
  return [1, 'ellipsis', page - 1, page, page + 1, 'ellipsis', pageCount];
}

/**
 * Unified list footer: row range, page count, rows-per-page, and First / Previous / Next / Last.
 * Used as the template pattern for admin list pages (e.g. Audit Logs).
 */
export function ListPagination({
  offset,
  limit,
  total,
  onOffsetChange,
  onLimitChange,
  pageSizeOptions = ADMIN_LIST_PAGE_SIZE_OPTIONS,
  showLimitSelect = true,
}: ListPaginationProps) {
  const { t } = useTranslation('common');

  const { start, end, page, pageCount, lastOffset } = useMemo(() => {
    const safeLimit = Math.max(1, limit);
    const startIdx = offset + 1;
    const endIdx = Math.min(offset + safeLimit, total);
    const pc = Math.max(1, Math.ceil(total / safeLimit));
    const p = Math.min(pc, Math.floor(offset / safeLimit) + 1);
    const last = Math.max(0, (Math.ceil(total / safeLimit) - 1) * safeLimit);
    return { start: startIdx, end: endIdx, page: p, pageCount: pc, lastOffset: last };
  }, [offset, limit, total]);
  const pageItems = useMemo(() => buildPageItems(page, pageCount), [page, pageCount]);

  // Bail-out happens AFTER all hooks so the hook count stays stable across
  // total=0 ⇄ total>0 transitions (Rules of Hooks).
  if (total === 0) return null;

  const atFirst = offset <= 0;
  const atLast = offset + limit >= total;

  return (
    <div className={css.bar} role="navigation" aria-label={t('listPaginationNav')}>
      <div className={css.summary}>
        <span className={css.range}>
          {t('paginationRowRange', {
            start: fmtInt(start),
            end: fmtInt(end),
            total: fmtInt(total),
          })}
        </span>
        <span className={css.pageOf}>
          {t('paginationPageOf', { page: fmtInt(page), pageCount: fmtInt(pageCount) })}
        </span>
      </div>
      <div className={css.controls}>
        {showLimitSelect ? (
          <label className={css.pageSizeLabel}>
            <span>{t('rowsPerPage')}</span>
            <select
              aria-label={t('rowsPerPage')}
              className={css.pageSizeSelect}
              value={limit}
              onChange={(e) => {
                const next = Number(e.target.value) as AdminListPageSize;
                onLimitChange(next);
                onOffsetChange(0);
              }}
            >
              {pageSizeOptions.map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </select>
          </label>
        ) : null}
        <div className={css.pagePills} aria-label={t('paginationPageNumbers')}>
          {pageItems.map((item, idx) =>
            item === 'ellipsis' ? (
              <span key={`ellipsis-${idx}`} className={css.ellipsis}>
                ...
              </span>
            ) : (
              <button data-design-system-escape="primitive-internal"
                key={item}
                type="button"
                className={item === page ? css.pagePillActive : css.pagePill}
                onClick={() => onOffsetChange((item - 1) * limit)}
                aria-current={item === page ? 'page' : undefined}
                aria-label={t('paginationGoToPage', { page: fmtInt(item) })}
              >
                {fmtInt(item)}
              </button>
            ),
          )}
        </div>
        <div className={css.navCluster}>
          <Button
            type="button"
            variant="secondary"
            size="sm"
            disabled={atFirst}
            onClick={() => onOffsetChange(0)}
            aria-label={t('firstPage')}
          >
            {t('paginationFirst')}
          </Button>
          <Button
            type="button"
            variant="secondary"
            size="sm"
            disabled={atFirst}
            onClick={() => onOffsetChange(Math.max(0, offset - limit))}
            aria-label={t('previousPage')}
          >
            {t('previous')}
          </Button>
          <Button
            type="button"
            variant="secondary"
            size="sm"
            disabled={atLast}
            onClick={() => onOffsetChange(offset + limit)}
            aria-label={t('nextPage')}
          >
            {t('next')}
          </Button>
          <Button
            type="button"
            variant="secondary"
            size="sm"
            disabled={atLast}
            onClick={() => onOffsetChange(lastOffset)}
            aria-label={t('lastPage')}
          >
            {t('paginationLast')}
          </Button>
        </div>
      </div>
    </div>
  );
}
