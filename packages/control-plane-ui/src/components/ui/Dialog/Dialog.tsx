import * as RadixDialog from '@radix-ui/react-dialog';
import clsx from 'clsx';
import { useTranslation } from 'react-i18next';
import styles from './Dialog.module.css';

export interface DialogProps {
  /** Whether the dialog is open. */
  open: boolean;
  /** Callback when open state changes (e.g. Escape or overlay click). */
  onOpenChange: (open: boolean) => void;
  /** Dialog heading shown as the accessible title. */
  title: string;
  /** Optional description rendered below the title. */
  description?: string;
  /** Body content. */
  children: React.ReactNode;
  /** Optional footer rendered outside the scrollable drawer body. */
  footer?: React.ReactNode;
  /** Width preset. Drawer variant uses `xl` by default. @default 'md' */
  size?: 'sm' | 'md' | 'lg' | 'xl';
  /**
   * Layout variant.
   *   - `modal` (default): centered, scale-in animation.
   *   - `drawer`: full-height panel anchored to the right edge with a
   *     slide-in animation. Sized by the same `size` prop.
   */
  variant?: 'modal' | 'drawer';
  /** Additional class name for the content panel. */
  className?: string;
  /** Additional class name for the footer panel. */
  footerClassName?: string;
  /**
   * Hide the built-in X close button. Use when the dialog is gated behind
   * a custom condition (e.g. an acknowledgement checkbox) — the X would
   * otherwise dispatch onOpenChange(false) and be silently ignored by the
   * parent, leaving the user clicking a dead control with no feedback.
   * @default false
   */
  hideClose?: boolean;
}

export function Dialog({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
  size,
  variant = 'modal',
  className,
  footerClassName,
  hideClose = false,
}: DialogProps) {
  const { t } = useTranslation();
  const resolvedSize = size ?? (variant === 'drawer' ? 'xl' : 'md');
  return (
    <RadixDialog.Root open={open} onOpenChange={onOpenChange}>
      <RadixDialog.Portal>
        <RadixDialog.Overlay className={styles.overlay} />
        <RadixDialog.Content
          className={clsx(
            styles.content,
            variant === 'drawer' ? styles.drawer : null,
            styles[resolvedSize],
            className,
          )}
        >
          <RadixDialog.Title className={styles.title}>
            {title}
          </RadixDialog.Title>
          {description && (
            <RadixDialog.Description className={styles.description}>
              {description}
            </RadixDialog.Description>
          )}
          <div className={styles.body}>{children}</div>
          {footer ? <div className={clsx(styles.footer, footerClassName)}>{footer}</div> : null}
          {!hideClose && (
            <RadixDialog.Close asChild>
              <button data-design-system-escape="primitive-internal" className={styles.close} aria-label={t('common:close')}>
                &times;
              </button>
            </RadixDialog.Close>
          )}
        </RadixDialog.Content>
      </RadixDialog.Portal>
    </RadixDialog.Root>
  );
}

Dialog.displayName = 'Dialog';
