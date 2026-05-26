import type { HTMLAttributes } from 'react';
import clsx from 'clsx';
import styles from './Card.module.css';

export interface CardProps extends HTMLAttributes<HTMLDivElement> {
  /** Inner padding. @default 'md' */
  padding?: 'sm' | 'md' | 'lg' | 'none';
  /** Enables hover affordance for cards that act like a button/link. */
  interactive?: boolean;
}

export function Card({
  padding = 'md',
  interactive = false,
  className,
  children,
  ...props
}: CardProps) {
  const isInteractive = interactive || Boolean(props.onClick) || props.role === 'button';

  return (
    <div
      className={clsx(styles.card, styles[`pad-${padding}`], className)}
      data-interactive={isInteractive ? 'true' : undefined}
      {...props}
    >
      {children}
    </div>
  );
}
