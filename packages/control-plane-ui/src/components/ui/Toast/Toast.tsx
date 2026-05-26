import * as RadixToast from '@radix-ui/react-toast';
import clsx from 'clsx';
import { useTranslation } from 'react-i18next';
import styles from './Toast.module.css';

export interface ToastProps {
  /** Whether the toast is visible. */
  open: boolean;
  /** Callback when open state changes (e.g. auto-dismiss or swipe). */
  onOpenChange: (open: boolean) => void;
  /** Toast heading. */
  title: string;
  /** Optional description text. */
  description?: string;
  /** Visual variant. @default 'default' */
  variant?: 'default' | 'success' | 'error' | 'warning';
  /** Auto-dismiss duration in ms. @default 5000 */
  duration?: number;
}

/**
 * Wrap the app root with `ToastProvider` to enable toasts.
 * Renders the Radix provider and a fixed-position viewport.
 */
export function ToastProvider({ children }: { children: React.ReactNode }) {
  return (
    <RadixToast.Provider swipeDirection="right">
      {children}
      <RadixToast.Viewport className={styles.viewport} />
    </RadixToast.Provider>
  );
}

ToastProvider.displayName = 'ToastProvider';

/** Individual toast notification. */
export function Toast({
  open,
  onOpenChange,
  title,
  description,
  variant = 'default',
  duration = 5000,
}: ToastProps) {
  const { t } = useTranslation();
  return (
    <RadixToast.Root
      open={open}
      onOpenChange={onOpenChange}
      duration={duration}
      className={clsx(styles.toast, styles[variant])}
    >
      <RadixToast.Title className={styles.title}>{title}</RadixToast.Title>
      {description && (
        <RadixToast.Description className={styles.description}>
          {description}
        </RadixToast.Description>
      )}
      <RadixToast.Close className={styles.close} aria-label={t('common:close')}>
        &times;
      </RadixToast.Close>
    </RadixToast.Root>
  );
}

Toast.displayName = 'Toast';
