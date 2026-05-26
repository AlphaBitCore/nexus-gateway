import { forwardRef, type HTMLAttributes } from 'react';
import clsx from 'clsx';
import styles from './Grid.module.css';

export type GridColumns = 1 | 2 | 3 | 4 | 5 | 6;
export type GridGap = 'xs' | 'sm' | 'md' | 'lg' | 'xl';

export interface GridProps extends HTMLAttributes<HTMLDivElement> {
  /** Number of columns (1-6). @default 1 */
  columns?: GridColumns;
  /** Gap between cells. @default 'md' */
  gap?: GridGap;
  /** Responsive strategy. @default 'viewport' */
  responsive?: 'viewport' | 'container';
}

export const Grid = forwardRef<HTMLDivElement, GridProps>(
  (
    {
      columns = 1,
      gap = 'md',
      responsive = 'viewport',
      className,
      ...props
    },
    ref,
  ) => (
    <div
      ref={ref}
      className={clsx(
        styles.grid,
        styles[`cols-${columns}`],
        styles[`gap-${gap}`],
        styles[responsive],
        className,
      )}
      {...props}
    />
  ),
);

Grid.displayName = 'Grid';
