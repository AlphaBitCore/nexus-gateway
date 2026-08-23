import { useState, useCallback, useId } from 'react';
import { useTranslation } from 'react-i18next';
import { Dialog } from '../Dialog/Dialog';
import { Button } from '@nexus-gateway/ui-shared';
import { Stack } from '../Stack/Stack';
import { Checkbox } from '../Checkbox/Checkbox';
import styles from './SecretDialog.module.css';
import { useTimeouts } from '@/hooks/useTimeouts';

export interface SecretDialogProps {
  open: boolean;
  secret: string | null;
  title: string;
  warning: string;
  onClose: () => void;
  /**
   * Hard-gate the Close button behind an "I have copied and stored this secret
   * securely" checkbox. Opt-in per caller — the personal API key flow leaves
   * it off, the OAuth client create/rotate flow turns it on so the admin
   * can't dismiss the modal without acknowledging the secret is one-shot.
   * Defaults to false for backward compatibility with existing callers.
   */
  requireAcknowledgement?: boolean;
  /** Label shown next to the acknowledgement checkbox. Required when requireAcknowledgement is true. */
  acknowledgementLabel?: string;
}

export function SecretDialog({
  open,
  secret,
  title,
  warning,
  onClose,
  requireAcknowledgement = false,
  acknowledgementLabel,
}: SecretDialogProps) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  const [acknowledged, setAcknowledged] = useState(false);
  const ackId = useId();

  const armTimeout = useTimeouts();
  const handleCopy = useCallback(async () => {
    if (!secret) return;
    await navigator.clipboard.writeText(secret);
    setCopied(true);
    armTimeout(() => setCopied(false), 2000);
  }, [secret, armTimeout]);

  const handleClose = useCallback(() => {
    onClose();
    setCopied(false);
    setAcknowledged(false);
  }, [onClose]);

  const closeDisabled = requireAcknowledgement && !acknowledged;

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => { if (!o && !closeDisabled) { handleClose(); } }}
      title={title}
      size="sm"
      hideClose={closeDisabled}
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
        {requireAcknowledgement && (
          <label htmlFor={ackId} className={styles.ackRow}>
            <Checkbox
              id={ackId}
              checked={acknowledged}
              onCheckedChange={(c) => setAcknowledged(c === true)}
            />
            <span>{acknowledgementLabel}</span>
          </label>
        )}
        <Button variant="secondary" onClick={handleClose} disabled={closeDisabled}>
          {t('common:close')}
        </Button>
      </Stack>
    </Dialog>
  );
}
