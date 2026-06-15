import clsx from 'clsx';
import { useTranslation } from 'react-i18next';
import { Input } from '@/components/ui/Input';
import styles from './ListFilterToolbar.module.css';

export interface ListFilterToolbarProps {
  searchPlaceholder: string;
  searchValue: string;
  onSearchChange: (value: string) => void;
  searchAriaLabel?: string;
  /** When true, omit the search field (filters-only toolbar). */
  hideSearch?: boolean;
  /** Extra filter controls (selects, etc.) */
  children?: React.ReactNode;
  /** Optional line below filters */
  meta?: React.ReactNode;
  className?: string;
  variant?: 'default' | 'boxed';
}

export function ListFilterToolbar({
  searchPlaceholder,
  searchValue,
  onSearchChange,
  searchAriaLabel,
  hideSearch = false,
  children,
  meta,
  className,
  variant = 'default',
}: ListFilterToolbarProps) {
  const { t } = useTranslation();
  const hasSearch = !hideSearch && searchValue.trim().length > 0;

  return (
    <div
      className={clsx(styles.toolbar, variant === 'boxed' && styles.boxed, className)}
      role={hideSearch ? 'group' : 'search'}
    >
      <div className={styles.row}>
        {!hideSearch && (
          <>
            <div className={styles.searchBox}>
              <span className={styles.searchIcon} aria-hidden="true" />
              <Input
                type="search"
                enterKeyHint="search"
                autoComplete="off"
                aria-label={searchAriaLabel ?? searchPlaceholder}
                placeholder={searchPlaceholder}
                value={searchValue}
                onChange={(e) => onSearchChange(e.target.value)}
                className={styles.searchInput}
              />
            </div>
            {hasSearch && (
              <button data-design-system-escape="primitive-internal"
                type="button"
                onClick={() => onSearchChange('')}
                className={styles.clearButton}
                aria-label={t('common:clear')}
              >
                {t('common:clear')}
              </button>
            )}
          </>
        )}
        {children}
      </div>
      {meta != null && meta !== false && (
        <div className={styles.meta}>{meta}</div>
      )}
    </div>
  );
}
