import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from 'react';
import clsx from 'clsx';
import styles from './IconButton.module.css';

export type IconButtonVariant = 'ghost' | 'subtle';
export type IconButtonSize = 'sm' | 'md' | 'lg';

export interface IconButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  /**
   * The icon node. Typically a small inline SVG. Sized via CSS — the
   * button enforces a square hit target sized by the `size` prop so
   * different icons stay visually consistent.
   */
  children: ReactNode;
  /**
   * Accessible label for screen readers. Icon-only buttons MUST set
   * this — there is no text content to fall back on.
   */
  'aria-label': string;
  /** @default 'ghost' */
  variant?: IconButtonVariant;
  /** @default 'md' (32×32) */
  size?: IconButtonSize;
}

/**
 * IconButton — a single-icon button with a square hit target.
 *
 * Use for: dismiss-on-toast (×), close-on-dialog (×), header user menu
 * trigger, sidebar collapse toggle, any "click this glyph" affordance
 * with no accompanying text.
 *
 * Don't use for: text-labelled actions (use `Button`), `?`-style help
 * buttons (use `HelpIconButton`), or link-styled inline actions
 * (use `LinkButton`).
 *
 * Theme integration: variant `ghost` (default) is transparent + uses
 * `--color-text-muted` for the icon, hovering to `--color-text` + a
 * `--color-bg-hover` background. Variant `subtle` adds a faint
 * `--color-surface-2` resting background. Both are token-driven so any
 * theme can restyle without touching this file.
 */
export const IconButton = forwardRef<HTMLButtonElement, IconButtonProps>(
  ({ variant = 'ghost', size = 'md', type = 'button', className, children, ...props }, ref) => (
    <button
      ref={ref}
      type={type}
      className={clsx(styles.iconButton, styles[variant], styles[size], className)}
      {...props}
    >
      {children}
    </button>
  ),
);

IconButton.displayName = 'IconButton';
