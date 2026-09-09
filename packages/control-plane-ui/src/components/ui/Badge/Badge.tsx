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
 *
 * Tolerates a missing value on purpose. This is a shared primitive on dozens
 * of badge sites, and it is usually handed a field straight off an API
 * response. `status.toLowerCase()` on an absent field throws, and a throw
 * inside a DataTable cell render unmounts the WHOLE page, not the one badge:
 * a single field an endpoint stopped sending would blank the screen. A grey
 * badge is the right failure.
 */
export function statusToVariant(status: string | null | undefined): BadgeVariant {
  const normalized = (status ?? '').toLowerCase();
  const map: Record<string, BadgeVariant> = {
    active: 'success',
    enabled: 'success',
    healthy: 'success',
    connected: 'success',
    warning: 'warning',
    degraded: 'warning',
    deprecated: 'warning',
    rotating: 'warning',
    error: 'danger',
    disabled: 'danger',
    unavailable: 'danger',
    expired: 'danger',
    // An API key's terminal states. `expired` reads like its `disabled`
    // sibling because the consequence is the same -- the key no longer
    // authenticates -- and `rotating` is a transient the owner should notice
    // without alarm.
    failed: 'danger',
    pending: 'info',
    unknown: 'default',
    inactive: 'default',
  };
  return map[normalized] || 'default';
}

