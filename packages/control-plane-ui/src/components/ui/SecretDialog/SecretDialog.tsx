import { useState, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { Dialog } from '../Dialog/Dialog';
import { Button } from '@nexus-gateway/ui-shared';
import { Stack } from '../Stack/Stack';
import styles from './SecretDialog.module.css';

export interface SecretDialogProps {
  open: boolean;
  secret: string | null;
  title: string;
  warning: string;
  onClose: () => void;
}

export function SecretDialog({ open, secret, title, warning, onClose }: SecretDialogProps) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);

  const handleCopy = useCallback(async () => {
    if (!secret) return;
    await navigator.clipboard.writeText(secret);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  }, [secret]);

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => { if (!o) { onClose(); setCopied(false); } }}
      title={title}
      size="sm"
    >
      <Stack gap="md">
        <p className={styles.warning}>{warning}</p>
        <div className={styles.secretRow}>
          <code className={styles.secretCode}>{secret}</code>
          <button data-design-system-escape="primitive-internal"
            type="button"
            className={styles.copyBtn}
            onClick={handleCopy}
            aria-label={t('common:copy')}
          >
            {copied ? (
              <svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true">
                <path d="M3 8.5L6 11.5L13 4.5" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
              </svg>
            ) : (
              <svg width="16" height="16" viewBox="0 0 16 16" fill="none" aria-hidden="true">
                <rect x="5.5" y="5.5" width="8" height="8" rx="1.5" stroke="currentColor" strokeWidth="1.5" />
                <path d="M10.5 5.5V3.5C10.5 2.67 9.83 2 9 2H3.5C2.67 2 2 2.67 2 3.5V9C2 9.83 2.67 10.5 3.5 10.5H5.5" stroke="currentColor" strokeWidth="1.5" />
              </svg>
            )}
          </button>
        </div>
        {copied && <p className={styles.copiedHint}>{t('common:copied')}</p>}
        <Button variant="secondary" onClick={onClose}>
          {t('common:close')}
        </Button>
      </Stack>
    </Dialog>
  );
}
