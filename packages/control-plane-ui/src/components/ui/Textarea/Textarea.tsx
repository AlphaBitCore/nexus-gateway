import { forwardRef, type TextareaHTMLAttributes } from 'react';
import clsx from 'clsx';
import styles from './Textarea.module.css';

export interface TextareaProps
  extends TextareaHTMLAttributes<HTMLTextAreaElement> {
  /** Show error styling. */
  error?: boolean;
}

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaProps>(
  ({ error, className, ...props }, ref) => (
    <textarea
      ref={ref}
      className={clsx(styles.textarea, className)}
      data-error={error || undefined}
      aria-invalid={error || undefined}
      {...props}
    />
  ),
);

Textarea.displayName = 'Textarea';
