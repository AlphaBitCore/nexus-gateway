import { forwardRef, type HTMLAttributes } from 'react';
import clsx from 'clsx';
import styles from './Stack.module.css';

export type StackGap = 'xs' | 'sm' | 'md' | 'lg' | 'xl';

export interface StackProps extends HTMLAttributes<HTMLDivElement> {
  /** Layout direction. @default 'vertical' */
  direction?: 'vertical' | 'horizontal';
  /** Gap between children. @default 'md' */
  gap?: StackGap;
  /** Cross-axis alignment. */
  align?: 'start' | 'center' | 'end' | 'stretch';
  /** Main-axis alignment. */
  justify?: 'start' | 'center' | 'end' | 'between';
  /** Expand to fill parent width. */
  fullWidth?: boolean;
}

export const Stack = forwardRef<HTMLDivElement, StackProps>(
  (
    {
      direction = 'vertical',
      gap = 'md',
      align,
      justify,
      fullWidth,
      className,
      ...props
    },
    ref,
  ) => (
    <div
      ref={ref}
      className={clsx(
        styles.stack,
        styles[direction],
        styles[`gap-${gap}`],
        align && styles[`align-${align}`],
        justify && styles[`justify-${justify}`],
        fullWidth && styles.fullWidth,
        className,
      )}
      {...props}
    />
  ),
);

Stack.displayName = 'Stack';
