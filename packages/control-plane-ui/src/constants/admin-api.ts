/**
 * Admin list APIs default to a small page size. Use this when a screen needs an unfiltered
 * full set for dropdowns or comboboxes (server maximum is 200 per request).
 */
export const ADMIN_LIST_FULL_PAGE_PARAMS: Record<string, string> = {
  limit: '200',
  offset: '0',
};

/** Allowed page sizes for dashboard tables (server caps at 200). */
export const ADMIN_LIST_PAGE_SIZE_OPTIONS = [5, 10, 20, 50, 100] as const;

export type AdminListPageSize = (typeof ADMIN_LIST_PAGE_SIZE_OPTIONS)[number];

export const DEFAULT_ADMIN_LIST_PAGE_SIZE: AdminListPageSize = 10;

export function clampAdminListPageSize(raw: number): AdminListPageSize {
  const n = Number(raw);
  if ((ADMIN_LIST_PAGE_SIZE_OPTIONS as readonly number[]).includes(n)) {
    return n as AdminListPageSize;
  }
  return DEFAULT_ADMIN_LIST_PAGE_SIZE;
}
