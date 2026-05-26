import { useTranslation } from 'react-i18next';
import { countLiveTrafficFilters, describeLiveTrafficFilters, type LiveTrafficFiltersState } from './liveTrafficFilters';
import css from './LiveTrafficActiveFiltersBar.module.css';

export function LiveTrafficActiveFiltersBar({ applied }: { applied: LiveTrafficFiltersState }) {
  const { t } = useTranslation();
  const n = countLiveTrafficFilters(applied);
  if (n === 0) return null;
  const lines = describeLiveTrafficFilters(applied);
  return (
    <div className={css.wrapper}>
      <div className={css.heading}>
        {t('pages:traffic.activeFilters', { count: n })}
      </div>
      <div className={css.chipList}>
        {lines.map((line) => (
          <span key={line} className={css.chip} title={line}>
            {line}
          </span>
        ))}
      </div>
    </div>
  );
}
