import type { ReactNode } from 'react';
import { cn } from '@/lib/utils';
import styles from './PageHeader.module.css';

export interface PageHeaderProps {
  title: string;
  subtitle?: string;
  action?: ReactNode;
}

/** Page title block aligned with the prime-console text hierarchy. */
export function PageHeader({ title, subtitle, action }: PageHeaderProps) {
  return (
    <div
      className={cn(
        'flex flex-wrap items-start justify-between gap-3',
      )}
    >
      <div className="min-w-0">
        <h1
          className={cn(
            'm-0 text-[28px] font-extrabold leading-9 tracking-normal',
            'w-fit bg-[image:var(--color-title-gradient)] bg-clip-text text-transparent',
            '[-webkit-text-fill-color:transparent]',
          )}
        >
          {title}
        </h1>
        {subtitle ? (
          <p
            className={cn(
              'm-0 mt-1.5 max-w-3xl text-sm leading-normal',
              'text-[var(--color-text-tertiary)]',
            )}
          >
            {subtitle}
          </p>
        ) : null}
      </div>
      {action ? (
        <div className={cn('flex shrink-0 items-center gap-2', styles.action)}>
          {action}
        </div>
      ) : null}
    </div>
  );
}
