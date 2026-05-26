import type { HTMLAttributes, ReactNode } from 'react';
import clsx from 'clsx';
import styles from './Badge.module.css';

export type BadgeVariant =
  | 'default'
  | 'success'
  | 'warning'
  | 'danger'
  | 'info'
  | 'outline';

export interface BadgeProps extends HTMLAttributes<HTMLSpanElement> {
  /** Visual variant. @default 'default' */
  variant?: BadgeVariant;
  children: ReactNode;
}

export function Badge({
  variant = 'default',
  className,
  children,
  ...props
}: BadgeProps) {
  return (
    <span
      className={clsx(styles.badge, styles[variant], className)}
      {...props}
    >
      {children}
    </span>
  );
}

/**
 * Maps a semantic status string (e.g. "active", "error") to a BadgeVariant.
 * Returns 'default' for unrecognised strings.
 */
export function statusToVariant(status: string): BadgeVariant {
  const normalized = status.toLowerCase();
  const map: Record<string, BadgeVariant> = {
    active: 'success',
    enabled: 'success',
    healthy: 'success',
    connected: 'success',
    warning: 'warning',
    degraded: 'warning',
    deprecated: 'warning',
    error: 'danger',
    disabled: 'danger',
    unavailable: 'danger',
    failed: 'danger',
    pending: 'info',
    unknown: 'default',
    inactive: 'default',
  };
  return map[normalized] || 'default';
}

