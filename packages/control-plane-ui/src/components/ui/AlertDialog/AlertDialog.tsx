import * as RadixAlertDialog from '@radix-ui/react-alert-dialog';
import clsx from 'clsx';
import styles from './AlertDialog.module.css';

export interface AlertDialogProps {
  /** Whether the alert dialog is open. */
  open: boolean;
  /** Callback when open state changes. */
  onOpenChange: (open: boolean) => void;
  /** Dialog heading shown as the accessible title. */
  title: string;
  /** Description of the action being confirmed. */
  description: string;
  /** Label for the confirm button. @default 'Confirm' */
  confirmLabel?: string;
  /** Label for the cancel button. @default 'Cancel' */
  cancelLabel?: string;
  /** Callback invoked when the user confirms. */
  onConfirm: () => void;
  /** Visual variant. Use 'danger' for destructive confirmations. @default 'default' */
  variant?: 'danger' | 'default';
  /** Disable the confirm button and show loading state. */
  loading?: boolean;
}

export function AlertDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmLabel = 'Confirm',
  cancelLabel = 'Cancel',
  onConfirm,
  variant = 'default',
  loading = false,
}: AlertDialogProps) {
  return (
    <RadixAlertDialog.Root open={open} onOpenChange={onOpenChange}>
      <RadixAlertDialog.Portal>
        <RadixAlertDialog.Overlay className={styles.overlay} />
        <RadixAlertDialog.Content className={styles.content}>
          <RadixAlertDialog.Title className={styles.title}>
            {title}
          </RadixAlertDialog.Title>
          <RadixAlertDialog.Description className={styles.description}>
            {description}
          </RadixAlertDialog.Description>
          <div className={styles.actions}>
            <RadixAlertDialog.Cancel asChild>
              <button data-design-system-escape="primitive-internal" className={styles.cancelButton}>{cancelLabel}</button>
            </RadixAlertDialog.Cancel>
            <RadixAlertDialog.Action asChild>
              <button data-design-system-escape="primitive-internal"
                className={clsx(
                  styles.confirmButton,
                  variant === 'danger' && styles.danger,
                )}
                onClick={onConfirm}
                disabled={loading}
              >
                {loading ? '...' : confirmLabel}
              </button>
            </RadixAlertDialog.Action>
          </div>
        </RadixAlertDialog.Content>
      </RadixAlertDialog.Portal>
    </RadixAlertDialog.Root>
  );
}

AlertDialog.displayName = 'AlertDialog';
