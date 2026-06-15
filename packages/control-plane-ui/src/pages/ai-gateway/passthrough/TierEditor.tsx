import { useTranslation } from 'react-i18next';
import {
  PASSTHROUGH_MIN_REASON_LEN,
  PASSTHROUGH_MAX_EXPIRY_HOURS,
} from '@/api/services';
import { Stack, Switch, Input, FormField } from '@/components/ui';
import { defaultExpiresAt, maxExpiresAt, type TierFormState } from './passthroughForm';
import styles from './PassthroughPage.module.css';

export function TierEditor({
  form,
  setForm,
  disabled,
  showEnabledByline,
  enabledBy,
}: {
  form: TierFormState;
  setForm: (next: TierFormState) => void;
  disabled?: boolean;
  showEnabledByline?: boolean;
  enabledBy?: string;
}) {
  const { t } = useTranslation();
  // Cross-constraint: bypassNormalize requires bypassCache.
  const setBypass = (key: 'bypassHooks' | 'bypassCache' | 'bypassNormalize', v: boolean) => {
    const next = { ...form, [key]: v };
    if (key === 'bypassNormalize' && v) next.bypassCache = true;
    if (key === 'bypassCache' && !v) next.bypassNormalize = false;
    setForm(next);
  };
  return (
    <Stack gap="md">
      <FormField label={t('pages:passthrough.fields.enabled')} helpText={t('pages:passthrough.fields.enabledHint')}>
        <Switch
          checked={form.enabled}
          disabled={disabled}
          onCheckedChange={v => {
            const next = { ...form, enabled: v };
            // First enable in this session: prefill expires + reset reason char counter.
            if (v && !form.expiresAt) next.expiresAt = defaultExpiresAt();
            setForm(next);
          }}
        />
      </FormField>

      <div className={styles.flagGrid}>
        <FormField label={t('pages:passthrough.fields.bypassHooks')} helpText={t('pages:passthrough.fields.bypassHooksHint')}>
          <Switch checked={form.bypassHooks} disabled={disabled} onCheckedChange={v => setBypass('bypassHooks', v)} />
        </FormField>
        <FormField label={t('pages:passthrough.fields.bypassCache')} helpText={t('pages:passthrough.fields.bypassCacheHint')}>
          <Switch checked={form.bypassCache} disabled={disabled || form.bypassNormalize} onCheckedChange={v => setBypass('bypassCache', v)} />
        </FormField>
        <FormField label={t('pages:passthrough.fields.bypassNormalize')} helpText={t('pages:passthrough.fields.bypassNormalizeHint')}>
          <Switch checked={form.bypassNormalize} disabled={disabled} onCheckedChange={v => setBypass('bypassNormalize', v)} />
        </FormField>
      </div>

      <FormField
        label={t('pages:passthrough.fields.expiresAt')}
        helpText={t('pages:passthrough.fields.expiresAtHint', { hours: PASSTHROUGH_MAX_EXPIRY_HOURS })}
      >
        <Input
          type="datetime-local"
          value={form.expiresAt}
          max={maxExpiresAt()}
          disabled={disabled || !form.enabled}
          onChange={e => setForm({ ...form, expiresAt: e.target.value })}
        />
      </FormField>

      <FormField
        label={t('pages:passthrough.fields.reason', { count: PASSTHROUGH_MIN_REASON_LEN })}
        helpText={t('pages:passthrough.fields.reasonHint', { count: form.reason.length, min: PASSTHROUGH_MIN_REASON_LEN })}
      >
        <textarea
          className={styles.reasonInput}
          value={form.reason}
          disabled={disabled || !form.enabled}
          placeholder={t('pages:passthrough.fields.reasonPlaceholder')}
          rows={3}
          onChange={e => setForm({ ...form, reason: e.target.value })}
        />
      </FormField>

      {showEnabledByline && enabledBy && (
        <div className={styles.byline}>{t('pages:passthrough.enabledByLine', { user: enabledBy })}</div>
      )}
    </Stack>
  );
}
