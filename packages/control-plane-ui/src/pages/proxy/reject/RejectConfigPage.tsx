import { useState, useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import { useApi } from '../../../hooks/useApi';
import { proxyApi } from '../../../api/services/infrastructure/misc/proxy';
import type { RejectConfig } from '../../../api/services/infrastructure/misc/proxy';
import {
  PageHeader, LoadingSpinner, ErrorBanner, Card, Button, Stack,
  RadioGroup, RadioGroupItem, Input, FormField,
} from '@/components/ui';
import { useToast } from '../../../context/ToastContext';
import styles from './RejectConfigPage.module.css';

type RejectLevel = 0 | 1 | 2;

const LEVEL_DESCRIPTIONS: Record<RejectLevel, string> = {
  0: 'pages:proxy.rejectConfig.level0Desc',
  1: 'pages:proxy.rejectConfig.level1Desc',
  2: 'pages:proxy.rejectConfig.level2Desc',
};

export function RejectConfigPage() {
  const { t } = useTranslation();
  const { addToast } = useToast();

  const [defaultLevel, setDefaultLevel] = useState<RejectLevel>(0);
  const [contactInfo, setContactInfo] = useState('');
  const [saving, setSaving] = useState(false);
  const [showConfirm, setShowConfirm] = useState(false);
  const [dirty, setDirty] = useState(false);

  const {
    data: config,
    loading,
    error,
    refetch,
  } = useApi<RejectConfig>(
    () => proxyApi.getRejectConfig(),
    ['proxy', 'reject-config'],
  );

  useEffect(() => {
    if (config) {
      setDefaultLevel(config.defaultLevel);
      setContactInfo(config.contactInfo);
      setDirty(false);
    }
  }, [config]);

  if (loading && !config) return <LoadingSpinner />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const handleLevelChange = (value: string) => {
    setDefaultLevel(Number(value) as RejectLevel);
    setDirty(true);
  };

  const handleContactChange = (value: string) => {
    setContactInfo(value);
    setDirty(true);
  };

  const handleSave = async () => {
    setSaving(true);
    try {
      await proxyApi.updateRejectConfig({ defaultLevel, contactInfo });
      refetch();
      addToast(t('pages:proxy.rejectConfig.saved'), 'success');
      setDirty(false);
    } catch (err) {
      addToast(err instanceof Error ? err.message : t('pages:proxy.rejectConfig.saveFailed'), 'error');
    } finally {
      setSaving(false);
      setShowConfirm(false);
    }
  };

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('pages:proxy.rejectConfig.title')}
        subtitle={t('pages:proxy.rejectConfig.subtitle')}
      />

      <Card>
        <div className={styles.sectionTitle}>{t('pages:proxy.rejectConfig.defaultLevel')}</div>
        <RadioGroup value={String(defaultLevel)} onValueChange={handleLevelChange}>
          {([0, 1, 2] as RejectLevel[]).map((level) => (
            <div key={level} className={styles.radioRow}>
              <RadioGroupItem value={String(level)} id={`level-${level}`} />
              <label htmlFor={`level-${level}`} className={styles.radioLabel}>
                <span className={styles.radioTitle}>
                  {t('pages:proxy.rejectConfig.levelLabel', { level })}
                </span>
                <span className={styles.radioDesc}>
                  {t(LEVEL_DESCRIPTIONS[level])}
                </span>
              </label>
            </div>
          ))}
        </RadioGroup>
      </Card>

      <Card>
        <FormField label={t('pages:proxy.rejectConfig.contactInfoLabel')}>
          <Input
            id="contact-info"
            value={contactInfo}
            onChange={(e) => handleContactChange(e.target.value)}
            placeholder={t('pages:proxy.rejectConfig.contactInfoPlaceholder')}
          />
        </FormField>
        <p className={styles.helperText}>
          {t('pages:proxy.rejectConfig.contactInfoHint')}
        </p>
      </Card>

      {config?.updatedAt && (
        <p className={styles.metaText}>
          {t('pages:proxy.rejectConfig.lastUpdated', {
            date: new Date(config.updatedAt).toLocaleString(),
            user: config.updatedBy ?? 'system',
          })}
        </p>
      )}

      <div className={styles.actions}>
        <Button
          variant="primary"
          loading={saving}
          disabled={!dirty}
          onClick={() => setShowConfirm(true)}
        >
          {t('pages:proxy.rejectConfig.save')}
        </Button>
      </div>

      {/* Confirmation dialog */}
      {showConfirm && (
        <div
          className={styles.confirmOverlay}
          role="presentation"
          onClick={() => setShowConfirm(false)}
        >
          <div
            className={styles.confirmDialog}
            role="alertdialog"
            aria-modal="true"
            aria-labelledby="reject-confirm-title"
            onClick={(e) => e.stopPropagation()}
          >
            <h3 id="reject-confirm-title" className={styles.confirmTitle}>
              {t('pages:proxy.rejectConfig.confirmTitle')}
            </h3>
            <p className={styles.confirmText}>
              {t('pages:proxy.rejectConfig.confirmDesc', { level: defaultLevel })}
            </p>
            <div className={styles.confirmActions}>
              <Button variant="ghost" onClick={() => setShowConfirm(false)}>
                {t('pages:proxy.rejectConfig.cancel')}
              </Button>
              <Button variant="primary" loading={saving} onClick={() => void handleSave()}>
                {t('pages:proxy.rejectConfig.confirmSave')}
              </Button>
            </div>
          </div>
        </div>
      )}
    </Stack>
  );
}
