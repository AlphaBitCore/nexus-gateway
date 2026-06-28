/**
 * ExemptionCreateDialog — self-contained create dialog for the Exemptions page.
 * Owns the create form state (source IP, target host, duration, reason,
 * submit-as-pending), the `creating` busy flag, validation, and the
 * create-grant / create-pending-request mutation. Resets the form whenever the
 * dialog closes (both via the dialog scrim/escape and the Cancel button).
 */
import { useCallback, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useAuth } from '@/auth/context/AuthContext';
import { complianceApi } from '@/api/services/compliance/compliance';
import type { CreateExemptionRequest } from '@/api/services/compliance/compliance';
import {
  Button,
  Checkbox,
  Dialog,
  FormField,
  Input,
  Select,
  Stack,
  Textarea,
} from '@/components/ui';
import { useToast } from '@/context/ToastContext';
import styles from './ExemptionCreateDialog.module.css';

interface ExemptionCreateDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  refetch: () => void;
}

export function ExemptionCreateDialog({ open, onOpenChange, refetch }: ExemptionCreateDialogProps) {
  const { t } = useTranslation();
  const { addToast } = useToast();
  const { email, keyName } = useAuth();

  const [creating, setCreating] = useState(false);
  const [formSourceIp, setFormSourceIp] = useState('');
  const [formTargetHost, setFormTargetHost] = useState('');
  const [formDuration, setFormDuration] = useState('1440');
  const [formReason, setFormReason] = useState('');
  const [submitAsPending, setSubmitAsPending] = useState(false);

  const durationOptions = useMemo(
    () =>
      [
        { value: '60', labelKey: 'duration.1h' },
        { value: '240', labelKey: 'duration.4h' },
        { value: '720', labelKey: 'duration.12h' },
        { value: '1440', labelKey: 'duration.24h' },
        { value: '2880', labelKey: 'duration.48h' },
        { value: '10080', labelKey: 'duration.7d' },
      ].map((o) => ({
        value: o.value,
        label: t(`pages:compliance.exemptions.${o.labelKey}`),
      })),
    [t],
  );

  const resetForm = useCallback(() => {
    setFormSourceIp('');
    setFormTargetHost('');
    setFormDuration('1440');
    setFormReason('');
    setSubmitAsPending(false);
  }, []);

  const handleCreate = useCallback(async () => {
    if (!formSourceIp.trim() || !formTargetHost.trim()) {
      addToast(t('pages:compliance.exemptions.validation.sourceTargetRequired'), 'error');
      return;
    }
    const durationMinutes = parseInt(formDuration, 10);
    if (Number.isNaN(durationMinutes) || durationMinutes <= 0) {
      addToast(t('pages:compliance.exemptions.validation.durationPositive'), 'error');
      return;
    }
    const reasonTrim = formReason.trim();
    if (reasonTrim.length < 4 || reasonTrim.length > 500) {
      addToast(t('pages:compliance.exemptions.validation.reasonLengthAdmin'), 'error');
      return;
    }
    setCreating(true);
    try {
      if (submitAsPending) {
        await complianceApi.createPendingExemptionRequest({
          transactionId: crypto.randomUUID(),
          sourceIp: formSourceIp.trim(),
          targetHost: formTargetHost.trim(),
          reason: reasonTrim,
          durationMinutes,
          requestedBy: (email && email.trim()) || keyName || 'admin-ui',
        });
        addToast(t('pages:compliance.exemptions.createPendingSuccess'), 'success');
      } else {
        const req: CreateExemptionRequest = {
          sourceIp: formSourceIp.trim(),
          targetHost: formTargetHost.trim(),
          durationMinutes,
          reason: reasonTrim,
        };
        await complianceApi.createExemptionGrant(req);
        addToast(t('pages:compliance.exemptions.createSuccess', 'Exemption created'), 'success');
      }
      onOpenChange(false);
      resetForm();
      void refetch();
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'unknown error';
      addToast(
        submitAsPending
          ? t('pages:compliance.exemptions.createPendingError', { error: msg })
          : t('pages:compliance.exemptions.createError', { error: msg }),
        'error',
      );
    } finally {
      setCreating(false);
    }
  }, [formSourceIp, formTargetHost, formDuration, formReason, submitAsPending, email, keyName, addToast, t, resetForm, onOpenChange, refetch]);

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        onOpenChange(next);
        if (!next) resetForm();
      }}
      title={t('pages:compliance.exemptions.createTitle', 'Create temporary exemption')}
      description={t(
        'pages:compliance.exemptions.createDesc',
        'Exempted traffic will still be TLS-bumped but will skip compliance hooks.',
      )}
      className={styles.dialog}
    >
      <div className={styles.formShell}>
        <Stack gap="md" className={styles.formContent}>
          <FormField label={t('pages:compliance.exemptions.sourceIpLabel', 'Source IP or CIDR')}>
            <Input
              value={formSourceIp}
              onChange={(e) => setFormSourceIp(e.target.value)}
              placeholder={t(
                'pages:compliance.exemptions.placeholder.sourceIp',
                'e.g. 10.0.0.0/24 or 10.0.0.5',
              )}
            />
          </FormField>

        <FormField label={t('pages:compliance.exemptions.targetHostLabel', 'Target host')}>
          <Input
            value={formTargetHost}
            onChange={(e) => setFormTargetHost(e.target.value)}
            placeholder={t(
              'pages:compliance.exemptions.placeholder.targetHost',
              'e.g. api.openai.com or *.openai.com',
            )}
          />
        </FormField>

        <Stack direction="horizontal" gap="sm" align="center">
          <Checkbox
            id="exemption-submit-pending"
            checked={submitAsPending}
            onCheckedChange={(v) => setSubmitAsPending(v === true)}
          />
          <label
            htmlFor="exemption-submit-pending"
            style={{ cursor: 'pointer', fontSize: 'var(--g-font-size-sm)' }}
          >
            {t('pages:compliance.exemptions.submitAsPendingLabel')}
          </label>
        </Stack>
        <p style={{ margin: 'var(--g-space-0)', fontSize: 'var(--g-font-size-xs)', color: 'var(--color-text-secondary)' }}>
          {t('pages:compliance.exemptions.submitAsPendingHint')}
        </p>

        <FormField label={t('pages:compliance.exemptions.durationLabel', 'Duration')}>
          <Select value={formDuration} onValueChange={setFormDuration} options={durationOptions} />
        </FormField>

        <FormField label={t('pages:compliance.exemptions.reasonLabel', 'Reason')}>
          <Textarea
            value={formReason}
            onChange={(e) => setFormReason(e.target.value)}
            placeholder={t(
              'pages:compliance.exemptions.placeholder.reason',
              'e.g. false positive investigation',
            )}
            rows={3}
          />
        </FormField>

        </Stack>
        <Stack direction="horizontal" gap="sm" justify="end" className={styles.actions}>
          <Button
            variant="secondary"
            onClick={() => {
              onOpenChange(false);
              resetForm();
            }}
            disabled={creating}
          >
            {t('common:cancel', 'Cancel')}
          </Button>
          <Button
            variant="primary"
            onClick={handleCreate}
            disabled={creating || !formSourceIp.trim() || !formTargetHost.trim()}
          >
            {creating
              ? t('pages:compliance.exemptions.creating', 'Creating…')
              : t('pages:compliance.exemptions.createBtn', 'Create')}
          </Button>
        </Stack>
      </div>
    </Dialog>
  );
}
