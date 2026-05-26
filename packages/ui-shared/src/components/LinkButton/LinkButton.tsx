import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from 'react';
import clsx from 'clsx';
import styles from './LinkButton.module.css';

export interface LinkButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  children: ReactNode;
}

/**
 * LinkButton — a text affordance that looks like a hyperlink but
 * triggers a JS action (no navigation, no `<a href>`).
 *
 * Use for: "Skip this step", "Add another", "Browse all templates",
 * "Show advanced", and other inline secondary actions where rendering a
 * full `<Button>` would visually outweigh the action's importance.
 *
 * Don't use for: real navigation (use react-router `<Link>`), primary
 * form submits (use `<Button>`), or icon-only triggers (use
 * `<IconButton>`).
 *
 * Theme integration: colour is `--color-link` resting → `--color-primary`
 * on hover, both come from the active theme's Layer 2 tokens.
 */
export const LinkButton = forwardRef<HTMLButtonElement, LinkButtonProps>(
  ({ type = 'button', className, children, ...props }, ref) => (
    <button
      ref={ref}
      type={type}
      className={clsx(styles.linkButton, className)}
      {...props}
    >
      {children}
    </button>
  ),
);

LinkButton.displayName = 'LinkButton';
