import { useState, useEffect, useRef, useCallback } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { alertsApi } from '@/api/services/alerts/alerts';
import styles from './AlertBell.module.css';

const POLL_INTERVAL_MS = 60_000;

export function AlertBell() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const [count, setCount] = useState(0);
  const timerRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const fetchCount = useCallback(async () => {
    try {
      const res = await alertsApi.list({ state: ['firing'], limit: 1 });
      setCount(res.total ?? 0);
    } catch {
      // Silently ignore — bell is non-critical UI
    }
  }, []);

  useEffect(() => {
    void fetchCount();
    timerRef.current = setInterval(() => void fetchCount(), POLL_INTERVAL_MS);
    return () => {
      if (timerRef.current) clearInterval(timerRef.current);
    };
  }, [fetchCount]);

  return (
    <button data-design-system-escape="primitive-internal"
      className={styles.bellButton}
      onClick={() => navigate('/alerts')}
      aria-label={count > 0 ? t('common:alertBell.active', { count }) : t('common:alertBell.idle')}
    >
      <svg
        width="18"
        height="18"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
      >
        <path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9" />
        <path d="M13.73 21a2 2 0 0 1-3.46 0" />
      </svg>
      {count > 0 && (
        <span className={styles.badge}>
          {count > 99 ? '99+' : count}
        </span>
      )}
    </button>
  );
}
