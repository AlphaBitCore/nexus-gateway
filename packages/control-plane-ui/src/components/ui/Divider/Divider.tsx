import { forwardRef, type HTMLAttributes } from 'react';
import clsx from 'clsx';
import styles from './Divider.module.css';

export interface DividerProps extends HTMLAttributes<HTMLHRElement> {
  /** Line orientation. @default 'horizontal' */
  orientation?: 'horizontal' | 'vertical';
  /** Spacing around the divider. @default 'md' */
  spacing?: 'sm' | 'md' | 'lg' | 'none';
}

export const Divider = forwardRef<HTMLHRElement, DividerProps>(
  ({ orientation = 'horizontal', spacing = 'md', className, ...props }, ref) => (
    <hr
      ref={ref}
      aria-hidden="true"
      className={clsx(
        styles.divider,
        styles[orientation],
        styles[`spacing-${spacing}`],
        className,
      )}
      {...props}
    />
  ),
);

Divider.displayName = 'Divider';
