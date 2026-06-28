import { useTranslation } from 'react-i18next';
import { Button, Stack, Input, FormField } from '@/components/ui';
import type { HeaderRow } from './channelMasking';
import styles from './AlertChannelEditPage.module.css';

interface WebhookConfigProps {
  webhookUrl: string;
  setWebhookUrl: (v: string) => void;
  headers: HeaderRow[];
  setHeaders: (v: HeaderRow[]) => void;
}

export function WebhookConfig({
  webhookUrl,
  setWebhookUrl,
  headers,
  setHeaders,
}: WebhookConfigProps) {
  const { t } = useTranslation();
  return (
    <Stack gap="md">
      <FormField label={t('pages:alerts.channelEditors.webhook.urlLabel')}>
        <Input
          type="url"
          value={webhookUrl}
          onChange={(e) => setWebhookUrl(e.target.value)}
          placeholder="https://hooks.example.com/alert"
        />
      </FormField>

      <div>
        <label>{t('pages:alerts.channelEditors.webhook.headersLabel')}</label>
        <p className={styles.headerHint}>
          {t('pages:alerts.channelEditors.webhook.headersHelp')}
        </p>
        <Stack gap="sm">
          {headers.map((row, idx) => (
            <div key={idx} className={styles.headerRow}>
              <Input
                value={row.key}
                onChange={(e) => {
                  const next = [...headers];
                  next[idx] = { ...next[idx], key: e.target.value };
                  setHeaders(next);
                }}
                placeholder={t('pages:alerts.customHeaderPlaceholder')}
              />
              <div className={styles.secretRow}>
                <Input
                  value={row.value}
                  readOnly={row.masked}
                  placeholder={t('common:enterValue')}
                  onChange={(e) => {
                    const next = [...headers];
                    next[idx] = { ...next[idx], value: e.target.value };
                    setHeaders(next);
                  }}
                />
                {row.masked && (
                  <Button
                    type="button"
                    variant="secondary"
                    size="sm"
                    onClick={() => {
                      const next = [...headers];
                      next[idx] = { ...next[idx], value: '', masked: false };
                      setHeaders(next);
                    }}
                  >
                    {t('pages:alerts.channels.edit.changeSecret')}
                  </Button>
                )}
              </div>
              <div className={styles.headerActionCell}>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  className={styles.deleteHeaderButton}
                  onClick={() => {
                    setHeaders(headers.filter((_, i) => i !== idx));
                  }}
                >
                  {t('common:delete')}
                </Button>
              </div>
            </div>
          ))}
          <div>
            <Button
              type="button"
              variant="secondary"
              size="sm"
              onClick={() =>
                setHeaders([...headers, { key: '', value: '', masked: false }])
              }
            >
              {t('pages:alerts.channelEditors.webhook.addHeader')}
            </Button>
          </div>
        </Stack>
      </div>
    </Stack>
  );
}
