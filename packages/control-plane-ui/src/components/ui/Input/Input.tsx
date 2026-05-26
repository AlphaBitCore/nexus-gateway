import { forwardRef, type InputHTMLAttributes } from 'react';
import clsx from 'clsx';
import styles from './Input.module.css';

export type InputSize = 'sm' | 'md' | 'lg';

export interface InputProps extends InputHTMLAttributes<HTMLInputElement> {
  /** Show error styling. */
  error?: boolean;
  /** Size preset. @default 'md' */
  inputSize?: InputSize;
}

export const Input = forwardRef<HTMLInputElement, InputProps>(
  ({ error, inputSize = 'md', className, ...props }, ref) => (
    <input
      ref={ref}
      className={clsx(styles.input, styles[inputSize], className)}
      data-error={error || undefined}
      aria-invalid={error || undefined}
      {...props}
    />
  ),
);

Input.displayName = 'Input';
