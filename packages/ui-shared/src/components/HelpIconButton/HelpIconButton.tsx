import { forwardRef, type ButtonHTMLAttributes } from 'react';
import clsx from 'clsx';
import styles from './HelpIconButton.module.css';

export interface HelpIconButtonProps extends Omit<ButtonHTMLAttributes<HTMLButtonElement>, 'children'> {
  /**
   * Accessible label describing what the help button explains.
   * Surfaces the help target to screen readers and via tooltip-on-hover
   * (`title` attribute). REQUIRED — a bare "?" with no description is
   * useless to assistive tech.
   */
  'aria-label': string;
}

/**
 * HelpIconButton — the small circular "?" affordance that opens (or
 * hovers to reveal) help context for an adjacent form field.
 *
 * Replaces the ad-hoc `<button className="helpBtn">?</button>` pattern
 * that appears in 10+ places across the routing wizard, retry policy,
 * conditional routing editor, fallback chain, etc. Centralising it as a
 * primitive guarantees the help affordance looks identical everywhere
 * the user sees one — without each form field re-implementing the
 * shape, size, hover behaviour, and accessibility attributes.
 *
 * Tooltips are out of scope here — pair with `<Tooltip>` if the help
 * content is short, or open a side drawer / modal for longer content.
 */
export const HelpIconButton = forwardRef<HTMLButtonElement, HelpIconButtonProps>(
  ({ type = 'button', className, ...props }, ref) => (
    <button
      ref={ref}
      type={type}
      className={clsx(styles.helpIconButton, className)}
      {...props}
    >
      ?
    </button>
  ),
);

HelpIconButton.displayName = 'HelpIconButton';
