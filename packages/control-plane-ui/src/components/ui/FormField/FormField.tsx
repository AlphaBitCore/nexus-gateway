import { useId, cloneElement, type ReactElement, type ReactNode, type HTMLAttributes } from 'react';
import clsx from 'clsx';
import { useTranslation } from 'react-i18next';
import { Tooltip } from '../Tooltip/Tooltip';
import styles from './FormField.module.css';

export interface FormFieldProps {
  /** Label text displayed above the input. */
  label: string;
  /** Error message. When present, takes visual precedence over helpText. */
  error?: string;
  /** Assistive text shown below the input (hidden when error is present). */
  helpText?: string;
  /** Tooltip content shown when hovering the "?" icon next to the label. */
  tooltip?: ReactNode;
  /** Whether the field is required. Displays an asterisk next to the label. */
  required?: boolean;
  /** A single form control element (Input, Select, Textarea, etc.). */
  children: ReactElement<HTMLAttributes<HTMLElement>>;
  /** Optional additional class name on the wrapper. */
  className?: string;
}

export function FormField({
  label,
  error,
  helpText,
  tooltip,
  required,
  children,
  className,
}: FormFieldProps) {
  const { t } = useTranslation();
  const baseId = useId();
  const inputId = `${baseId}-input`;
  const errorId = `${baseId}-error`;
  const helpId = `${baseId}-help`;

  const describedByParts: string[] = [];
  if (error) describedByParts.push(errorId);
  if (helpText && !error) describedByParts.push(helpId);
  const describedBy = describedByParts.length > 0 ? describedByParts.join(' ') : undefined;

  return (
    <div className={clsx(styles.field, className)}>
      <label htmlFor={inputId} className={styles.label}>
        {label}
        {required && (
          <span className={styles.required} aria-hidden="true">
            {' *'}
          </span>
        )}
        {tooltip && (
          <Tooltip content={tooltip} side="top">
            <span className={styles.tooltipIcon} aria-label={t('common:help')}>?</span>
          </Tooltip>
        )}
      </label>
      {cloneElement(children, {
        id: inputId,
        'aria-describedby': describedBy,
        ...(required ? { 'aria-required': true } : {}),
        ...(error ? { 'aria-invalid': true } : {}),
      })}
      {error && (
        <p id={errorId} className={styles.error} role="alert">
          {error}
        </p>
      )}
      {helpText && !error && (
        <p id={helpId} className={styles.helpText}>
          {helpText}
        </p>
      )}
    </div>
  );
}

FormField.displayName = 'FormField';
