/**
 * Breadcrumb — shows the user's location in the page hierarchy.
 *
 * Usage:
 *   <Breadcrumb items={[
 *     { label: 'Providers', to: '/config/providers' },
 *     { label: provider.name },
 *   ]} />
 */
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import styles from './Breadcrumb.module.css';

export interface BreadcrumbItem {
  label: string;
  /** When provided, renders as a link. Last item (no `to`) renders as current page. */
  to?: string;
}

export interface BreadcrumbProps {
  items: BreadcrumbItem[];
}

export function Breadcrumb({ items }: BreadcrumbProps) {
  const { t } = useTranslation();
  if (items.length === 0) return null;

  return (
    <nav aria-label={t('common:breadcrumb')} className={styles.nav}>
      {items.map((item, i) => {
        const isLast = i === items.length - 1;
        return (
          <span key={item.to ?? item.label}>
            {i > 0 && <span className={styles.separator} aria-hidden="true">/</span>}
            {' '}
            {isLast || !item.to ? (
              <span className={styles.current} aria-current="page">{item.label}</span>
            ) : (
              <Link to={item.to} className={styles.link}>{item.label}</Link>
            )}
          </span>
        );
      })}
    </nav>
  );
}
