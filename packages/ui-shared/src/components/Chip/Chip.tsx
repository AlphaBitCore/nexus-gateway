import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from 'react';
import clsx from 'clsx';
import styles from './Chip.module.css';

export type ChipSize = 'sm' | 'md';

export interface ChipProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  children: ReactNode;
  /**
   * Whether the chip is currently selected. Drives the active visual
   * state (filled background + bold text). When omitted the chip
   * renders as a plain pill that lights up on hover only.
   */
  active?: boolean;
  /** @default 'md' */
  size?: ChipSize;
}

/**
 * Chip — a pill-shaped toggle / selector. Acts like a `<button>` but is
 * styled as a compact pill so a row of them reads as a segmented control
 * or a tag list.
 *
 * Use for: time-range presets (1h / 24h / 7d), filter toggles, single-
 * choice selectors where rendering a full `<Select>` would feel heavy.
 *
 * For exclusive selection across a row, the parent manages which chip
 * has `active={true}` — Chip itself is stateless about siblings.
 *
 * Theme integration: active state uses `--color-primary` background +
 * `--color-primary-foreground` text; resting uses `--color-surface` +
 * `--color-text-muted`. All theme-token-driven so a customer brand
 * recolours every chip automatically.
 */
export const Chip = forwardRef<HTMLButtonElement, ChipProps>(
  ({ active = false, size = 'md', type = 'button', className, children, ...props }, ref) => (
    <button
      ref={ref}
      type={type}
      aria-pressed={active}
      className={clsx(
        styles.chip,
        styles[size],
        active && styles.active,
        className,
      )}
      {...props}
    >
      {children}
    </button>
  ),
);

Chip.displayName = 'Chip';
