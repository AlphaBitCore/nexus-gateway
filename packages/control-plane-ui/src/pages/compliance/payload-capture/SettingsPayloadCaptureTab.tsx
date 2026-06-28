import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { systemApi, type PayloadCaptureConfig } from '@/api/services/infrastructure/misc/system';
import styles from './SettingsPayloadCaptureTab.module.css';
import {
  AlertDialog,
  Button,
  Card,
  ErrorBanner,
  FormField,
  Input,
  Skeleton,
  Stack,
  Switch,
} from '@/components/ui';

// pendingFlip describes which capture flag the user has just turned on
// and for which we need to raise the compliance confirmation dialog
// (Q5=B). 'none' means no dialog is open. The tab keeps the UI state
// eager (the switch visually flips as soon as the user clicks it) but
// defers persistence until the admin confirms.
type PendingFlip = 'none' | 'request' | 'response';

export function SettingsPayloadCaptureTab() {
  const { t } = useTranslation();

  const [storeRequestBody, setStoreRequestBody] = useState(false);
  const [storeResponseBody, setStoreResponseBody] = useState(false);
  const [maxInlineBodyBytes, setMaxInlineBodyBytes] = useState('262144');
  const [maxRequestBytes, setMaxRequestBytes] = useState('10485760');
  const [maxResponseBytes, setMaxResponseBytes] = useState('10485760');

  // pendingFlip is non-'none' while the confirmation dialog is open —
  // the user has optimistically flipped a store flag from off to on
  // and must click the confirm CTA before we accept the change.
  const [pendingFlip, setPendingFlip] = useState<PendingFlip>('none');

  const { data, loading, error, refetch } = useApi<PayloadCaptureConfig>(
    () => systemApi.getPayloadCaptureConfig(),
    ['admin', 'settings', 'payload-capture'],
  );

  useEffect(() => {
    if (data) {
      setStoreRequestBody(data.storeRequestBody);
      setStoreResponseBody(data.storeResponseBody);
      setMaxInlineBodyBytes(String(data.maxInlineBodyBytes));
      setMaxRequestBytes(String(data.maxRequestBytes));
      setMaxResponseBytes(String(data.maxResponseBytes));
    }
  }, [data]);

  const { mutate: save, loading: saving } = useMutation(
    () =>
      systemApi.updatePayloadCaptureConfig({
        storeRequestBody,
        storeResponseBody,
        // parseInt handles the empty string case by yielding NaN, which
        // the server clamps back to the default anyway; guard here so
        // the wire payload always carries a valid integer.
        maxInlineBodyBytes: Number.parseInt(maxInlineBodyBytes, 10) || 0,
        maxRequestBytes: Number.parseInt(maxRequestBytes, 10) || 0,
        maxResponseBytes: Number.parseInt(maxResponseBytes, 10) || 0,
      }),
    {
      invalidateQueries: [['admin', 'settings', 'payload-capture']],
      onSuccess: () => refetch(),
    },
  );

  // handleToggleRequest / handleToggleResponse intercept the switch's
  // onCheckedChange. Turning a flag OFF is low-risk and applies
  // immediately; turning it ON raises the confirmation modal so the
  // admin must actively acknowledge the compliance consequence (Q5=B).
  const handleToggleRequest = (next: boolean) => {
    if (next && !storeRequestBody) {
      setPendingFlip('request');
      return;
    }
    setStoreRequestBody(next);
  };
  const handleToggleResponse = (next: boolean) => {
    if (next && !storeResponseBody) {
      setPendingFlip('response');
      return;
    }
    setStoreResponseBody(next);
  };

  const confirmFlip = () => {
    if (pendingFlip === 'request') {
      setStoreRequestBody(true);
    } else if (pendingFlip === 'response') {
      setStoreResponseBody(true);
    }
    setPendingFlip('none');
  };
  const cancelFlip = () => setPendingFlip('none');

  if (loading && !data) return <Skeleton.ListPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!data) return null;

  return (
    <Stack gap="md">
      <div className={styles.sectionHeader}>
        <h2 className={styles.sectionTitle}>{t('pages:settingsPayloadCapture.title')}</h2>
        <p className={styles.sectionSubtitle}>
          {t('pages:settingsPayloadCapture.subtitle')}
        </p>
      </div>

      <Card>
        <Stack gap="md">
          <div className={styles.formGrid}>
            <div className={styles.switchField}>
              <div className={styles.switchTitle}>{t('pages:settingsPayloadCapture.storeRequest')}</div>
              <div className={styles.switchRow}>
                <Switch
                  checked={storeRequestBody}
                  onCheckedChange={handleToggleRequest}
                  aria-label={t('pages:settingsPayloadCapture.storeRequest')}
                />
              </div>
            </div>

            <div className={styles.switchField}>
              <div className={styles.switchTitle}>{t('pages:settingsPayloadCapture.storeResponse')}</div>
              <div className={styles.switchRow}>
                <Switch
                  checked={storeResponseBody}
                  onCheckedChange={handleToggleResponse}
                  aria-label={t('pages:settingsPayloadCapture.storeResponse')}
                />
              </div>
              <p className={styles.switchHelp}>{t('pages:settingsPayloadCapture.streamingNote')}</p>
            </div>

            <FormField
              label={t('pages:settingsPayloadCapture.maxBytes')}
              helpText={t('pages:settingsPayloadCapture.maxBytesHelp')}
            >
              <Input
                type="number"
                value={maxInlineBodyBytes}
                onChange={e => setMaxInlineBodyBytes(e.target.value)}
                min={0}
                step={1024}
              />
            </FormField>

            <FormField
              label={t('pages:settingsPayloadCapture.maxRequestBytes')}
              helpText={t('pages:settingsPayloadCapture.maxRequestBytesHelp')}
            >
              <Input
                type="number"
                value={maxRequestBytes}
                onChange={e => setMaxRequestBytes(e.target.value)}
                min={0}
                step={1024 * 1024}
              />
            </FormField>

            <FormField
              label={t('pages:settingsPayloadCapture.maxResponseBytes')}
              helpText={t('pages:settingsPayloadCapture.maxResponseBytesHelp')}
            >
              <Input
                type="number"
                value={maxResponseBytes}
                onChange={e => setMaxResponseBytes(e.target.value)}
                min={0}
                step={1024 * 1024}
              />
            </FormField>
          </div>

          <Stack direction="horizontal" gap="sm">
            <Button className={styles.saveButton} onClick={() => save(undefined)} loading={saving}>
              {t('common:save')}
            </Button>
          </Stack>
        </Stack>
      </Card>

      <AlertDialog
        open={pendingFlip !== 'none'}
        onOpenChange={open => {
          if (!open) cancelFlip();
        }}
        title={t('pages:settingsPayloadCapture.confirmEnableTitle')}
        description={t('pages:settingsPayloadCapture.confirmEnableDesc')}
        confirmLabel={t('pages:settingsPayloadCapture.confirmEnableCta')}
        cancelLabel={t('pages:settingsPayloadCapture.cancel')}
        variant="danger"
        onConfirm={confirmFlip}
      />
    </Stack>
  );
}
