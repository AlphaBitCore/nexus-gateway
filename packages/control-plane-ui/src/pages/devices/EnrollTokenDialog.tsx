import { useState, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { Dialog, Button, Stack } from '@/components/ui';
import styles from './EnrollTokenDialog.module.css';
import { useMutation } from '@/hooks/useMutation';
import { devicesApi } from '@/api/services';
import { useZodForm, FormInput } from '@/lib/forms';
import { z } from 'zod';

const schema = z.object({ hostname: z.string().optional().default('') });

export function EnrollTokenDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (v: boolean) => void }) {
  const { t } = useTranslation();
  const [token, setToken] = useState<string | null>(null);
  const [expiresAt, setExpiresAt] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const form = useZodForm({ schema, defaultValues: { hostname: '' } });

  const { mutate: generate, loading } = useMutation(
    (data: { hostname?: string }) => devicesApi.generateEnrollToken(data.hostname),
    { onSuccess: (result) => { setToken(result.token); setExpiresAt(result.expiresAt ?? null); } },
  );

  const handleClose = useCallback(() => {
    setToken(null);
    setExpiresAt(null);
    setCopied(false);
    form.reset();
    onOpenChange(false);
  }, [form, onOpenChange]);

  const copyToken = useCallback(async () => {
    if (token) {
      await navigator.clipboard.writeText(token);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    }
  }, [token]);

  return (
    <Dialog open={open} onOpenChange={handleClose} title={t('pages:devices.enrollDevice')} size="md" className={styles.enrollDialog}>
      {!token ? (
        <form onSubmit={form.handleSubmit((data) => generate(data))}>
          <Stack gap="md">
            <FormInput form={form} name="hostname" label={t('pages:devices.hostnameHint')} />
            <p className={styles.helpTextSecondary}>
              {t('pages:devices.tokenWarning')}
            </p>
            <Button type="submit" loading={loading}>{t('pages:devices.generateToken')}</Button>
          </Stack>
        </form>
      ) : (
        <Stack gap="md">
          <div className={styles.tokenDisplay}>
            {token}
          </div>
          {expiresAt && (
            <p className={styles.helpTextSecondary}>
              {t('pages:devices.tokenExpiresAt', { time: new Date(expiresAt).toLocaleString() })}
            </p>
          )}
          <p className={styles.warningText}>
            {t('pages:devices.tokenShowOnce')}
          </p>
          <Stack direction="horizontal" gap="sm">
            <Button type="button" onClick={copyToken}>{copied ? t('pages:devices.copied') : t('pages:devices.copyToken')}</Button>
            <Button type="button" variant="secondary" onClick={handleClose}>{t('common:close')}</Button>
          </Stack>
        </Stack>
      )}
    </Dialog>
  );
}
