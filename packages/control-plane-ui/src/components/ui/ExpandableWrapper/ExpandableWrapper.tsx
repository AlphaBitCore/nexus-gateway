import { useState, useEffect, useRef, type ReactNode } from 'react';
import { useTranslation } from 'react-i18next';
import styles from './ExpandableWrapper.module.css';
import { clsx } from 'clsx';

export interface ExpandableWrapperProps {
  children: ReactNode;
  className?: string;
}

export function ExpandableWrapper({ children, className }: ExpandableWrapperProps) {
  const { t } = useTranslation();
  const [expanded, setExpanded] = useState(false);
  const closeBtnRef = useRef<HTMLButtonElement>(null);
  const expandBtnRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (!expanded) return;
    // Focus close button when expanded
    closeBtnRef.current?.focus();
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setExpanded(false);
    };
    document.addEventListener('keydown', handler);
    return () => document.removeEventListener('keydown', handler);
  }, [expanded]);

  const close = () => {
    setExpanded(false);
    // Return focus to expand button
    requestAnimationFrame(() => expandBtnRef.current?.focus());
  };

  return (
    <>
      <div className={clsx(styles.wrapper, className)}>
        {children}
        <button data-design-system-escape="primitive-internal"
          ref={expandBtnRef}
          type="button"
          className={styles.expandBtn}
          onClick={() => setExpanded(true)}
          aria-label={t('common:expandFullScreen')}
        >
          <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
            <polyline points="15 3 21 3 21 9" />
            <polyline points="9 21 3 21 3 15" />
            <line x1="21" y1="3" x2="14" y2="10" />
            <line x1="3" y1="21" x2="10" y2="14" />
          </svg>
        </button>
      </div>
      {expanded && (
        <div
          className={styles.overlay}
          role="dialog"
          aria-modal="true"
          aria-label={t('common:expandedView')}
          onClick={close}
        >
          <div className={styles.fullScreenCard} onClick={(e) => e.stopPropagation()}>
            <button data-design-system-escape="primitive-internal"
              ref={closeBtnRef}
              type="button"
              className={styles.closeBtn}
              onClick={close}
              aria-label={t('common:closeExpandedView')}
            >
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                <line x1="18" y1="6" x2="6" y2="18" />
                <line x1="6" y1="6" x2="18" y2="18" />
              </svg>
            </button>
            <div className={styles.fullScreenContent}>
              {children}
            </div>
          </div>
        </div>
      )}
    </>
  );
}
