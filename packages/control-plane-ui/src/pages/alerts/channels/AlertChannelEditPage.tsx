/**
 * AlertChannelEditPage — create / edit a unified alert channel.
 *
 * Layout:
 *   - Breadcrumb + PageHeader (title swaps between "Create channel" and
 *     "Edit channel: <name>")
 *   - Card 1: common fields (name, type, enabled, severities, sourceTypes)
 *   - Card 2: per-type config panel (webhook | slack | email | pagerduty)
 *   - Footer: Save / Cancel
 *
 * Masked secrets:
 *   Hub redacts sensitive config values on GET with the literal prefix
 *   `xxxx-••••-<last4>`. On PUT, Hub's `mergeMaskedSecrets` restores any
 *   value the UI forwards verbatim. The UI therefore:
 *     - Renders masked fields read-only with a "Change" button.
 *     - Clicking "Change" clears the input and lets the user type a new
 *       secret. Saving sends the new value; Hub stores it plainly.
 *     - If the user never touches the field, the masked token is PUT back
 *       as-is and Hub substitutes the original.
 *
 * Route params: `/alerts/channels/:id` where `id === 'new'` means create.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { alertsApi } from '@/api/services';
import type { AlertChannel, AlertSeverity } from '@/api/services';
import {
  PageHeader,
  Breadcrumb,
  Button,
  Stack,
  Card,
  Skeleton,
  ErrorBanner,
  Switch,
  Select,
  Input,
  FormField,
  MultiSelectDropdown,
} from '@/components/ui';
import styles from './AlertChannelEditPage.module.css';

/**
 * Literal prefix that Hub writes when masking a sensitive config value
 * on GET. Values starting with this string were redacted server-side; the
 * UI should treat them as "do not touch unless the user explicitly edits".
 * Note: the middle dots are Unicode bullets (U+2022), not ASCII asterisks.
 */
export const MASK_PREFIX = 'xxxx-••••-';

const CHANNEL_TYPES: AlertChannel['type'][] = ['webhook', 'slack', 'email', 'pagerduty'];
const SEVERITIES: AlertSeverity[] = ['critical', 'high', 'medium', 'low', 'info'];
const SOURCE_TYPES = ['quota', 'proxy', 'thing', 'provider', 'auth', 'system'];

interface HeaderRow {
  key: string;
  value: string;
  /** True when the value is a masked token returned by Hub. */
  masked: boolean;
}

function isMasked(value: unknown): boolean {
  return typeof value === 'string' && value.startsWith(MASK_PREFIX);
}

/**
 * Convert Hub's `config.headers` object into an ordered editable list. We
 * track each entry's `masked` flag so the UI can render a "Change" affordance
 * for header values Hub redacted (Authorization, Token, Secret substrings).
 */
function headersObjectToList(obj: unknown): HeaderRow[] {
  if (!obj || typeof obj !== 'object') return [];
  return Object.entries(obj as Record<string, unknown>).map(([k, v]) => ({
    key: k,
    value: String(v ?? ''),
    masked: isMasked(v),
  }));
}

function headersListToObject(list: HeaderRow[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (const row of list) {
    const k = row.key.trim();
    if (!k) continue;
    out[k] = row.value;
  }
  return out;
}

export function AlertChannelEditPage() {
  const { id: rawId } = useParams<{ id: string }>();
  const id = rawId ?? '';
  const isNew = id === 'new' || id === '';
  const { t } = useTranslation();
  const navigate = useNavigate();

  const { data: channel, loading, error, refetch } = useApi<AlertChannel>(
    () => alertsApi.getChannel(id),
    ['admin', 'alerts', 'channels', 'detail', id],
    { skip: isNew },
  );

  /* ── Form state ────────────────────────────────────────────────────────── */
  const [name, setName] = useState('');
  const [type, setType] = useState<AlertChannel['type']>('webhook');
  const [enabled, setEnabled] = useState(true);
  const [severities, setSeverities] = useState<AlertSeverity[]>([]);
  const [sourceTypes, setSourceTypes] = useState<string[]>([]);

  // Webhook
  const [webhookUrl, setWebhookUrl] = useState('');
  const [headers, setHeaders] = useState<HeaderRow[]>([]);

  // Slack (either webhookUrl OR botToken + channel)
  const [slackWebhookUrl, setSlackWebhookUrl] = useState('');
  const [slackBotToken, setSlackBotToken] = useState('');
  const [slackBotTokenMasked, setSlackBotTokenMasked] = useState(false);
  const [slackChannel, setSlackChannel] = useState('');

  // Email / SMTP
  const [smtpHost, setSmtpHost] = useState('');
  const [smtpPort, setSmtpPort] = useState('587');
  const [smtpFrom, setSmtpFrom] = useState('');
  const [smtpTo, setSmtpTo] = useState('');
  const [smtpUsername, setSmtpUsername] = useState('');
  const [smtpPassword, setSmtpPassword] = useState('');
  const [smtpPasswordMasked, setSmtpPasswordMasked] = useState(false);

  // PagerDuty
  const [routingKey, setRoutingKey] = useState('');
  const [routingKeyMasked, setRoutingKeyMasked] = useState(false);

  /* ── Seed form state from fetched channel ──────────────────────────────── */
  useEffect(() => {
    if (isNew || !channel) return;
    setName(channel.name);
    setType(channel.type);
    setEnabled(channel.enabled);
    setSeverities(channel.severities);
    setSourceTypes(channel.sourceTypes);

    const cfg = (channel.config ?? {}) as Record<string, unknown>;

    // Webhook
    setWebhookUrl(typeof cfg.url === 'string' ? cfg.url : '');
    setHeaders(headersObjectToList(cfg.headers));

    // Slack
    setSlackWebhookUrl(typeof cfg.webhookUrl === 'string' ? cfg.webhookUrl : '');
    const bot = typeof cfg.botToken === 'string' ? cfg.botToken : '';
    setSlackBotToken(bot);
    setSlackBotTokenMasked(isMasked(bot));
    setSlackChannel(typeof cfg.channel === 'string' ? cfg.channel : '');

    // Email / SMTP
    setSmtpHost(typeof cfg.smtpHost === 'string' ? cfg.smtpHost : '');
    setSmtpPort(
      typeof cfg.smtpPort === 'number'
        ? String(cfg.smtpPort)
        : typeof cfg.smtpPort === 'string'
          ? cfg.smtpPort
          : '587',
    );
    setSmtpFrom(typeof cfg.smtpFrom === 'string' ? cfg.smtpFrom : '');
    setSmtpTo(typeof cfg.smtpTo === 'string' ? cfg.smtpTo : '');
    setSmtpUsername(typeof cfg.smtpUsername === 'string' ? cfg.smtpUsername : '');
    const pwd = typeof cfg.smtpPassword === 'string' ? cfg.smtpPassword : '';
    setSmtpPassword(pwd);
    setSmtpPasswordMasked(isMasked(pwd));

    // PagerDuty
    const rk = typeof cfg.routingKey === 'string' ? cfg.routingKey : '';
    setRoutingKey(rk);
    setRoutingKeyMasked(isMasked(rk));
  }, [channel, isNew]);

  /* ── Build config blob for save ────────────────────────────────────────── */
  const buildConfig = useCallback((): Record<string, unknown> => {
    switch (type) {
      case 'webhook':
        return {
          url: webhookUrl.trim(),
          headers: headersListToObject(headers),
        };
      case 'slack':
        return {
          webhookUrl: slackWebhookUrl.trim(),
          botToken: slackBotToken,
          channel: slackChannel.trim(),
        };
      case 'email': {
        const port = parseInt(smtpPort, 10);
        return {
          smtpHost: smtpHost.trim(),
          smtpPort: Number.isFinite(port) ? port : 0,
          smtpFrom: smtpFrom.trim(),
          smtpTo: smtpTo.trim(),
          smtpUsername: smtpUsername.trim(),
          smtpPassword: smtpPassword,
        };
      }
      case 'pagerduty':
        return {
          routingKey: routingKey,
        };
    }
  }, [
    type,
    webhookUrl,
    headers,
    slackWebhookUrl,
    slackBotToken,
    slackChannel,
    smtpHost,
    smtpPort,
    smtpFrom,
    smtpTo,
    smtpUsername,
    smtpPassword,
    routingKey,
  ]);

  /* ── Mutations ─────────────────────────────────────────────────────────── */
  const { mutate: saveChannel, loading: saving } = useMutation<void, AlertChannel>(
    () => {
      const body = {
        name: name.trim(),
        type,
        enabled,
        severities,
        sourceTypes,
        config: buildConfig(),
      };
      if (isNew) return alertsApi.createChannel(body);
      return alertsApi.updateChannel(id, body);
    },
    {
      onSuccess: () => {
        navigate('/alerts/channels');
      },
      successMessage: isNew
        ? t('pages:alerts.channels.edit.createSuccess')
        : t('pages:alerts.channels.edit.saveSuccess'),
    },
  );

  const onSave = useCallback(() => {
    void saveChannel();
  }, [saveChannel]);
  const onCancel = useCallback(() => navigate('/alerts/channels'), [navigate]);

  /* ── Options for selects ───────────────────────────────────────────────── */
  const typeOptions = useMemo(
    () =>
      CHANNEL_TYPES.map((v) => ({
        value: v,
        label: t(`pages:alerts.channels.types.${v}`),
      })),
    [t],
  );

  const severityOptions = useMemo(
    () =>
      SEVERITIES.map((v) => ({
        value: v,
        label: t(`pages:alerts.channels.severities.${v}`),
      })),
    [t],
  );

  const sourceTypeOptions = useMemo(
    () => SOURCE_TYPES.map((v) => ({ value: v, label: v })),
    [],
  );

  if (!isNew && loading && !channel) return <Skeleton.DetailPageSkeleton />;
  if (!isNew && error) return <ErrorBanner message={error.message} onRetry={refetch} />;

  const title = isNew
    ? t('pages:alerts.channels.edit.createTitle')
    : t('pages:alerts.channels.edit.editTitle', { name: channel?.name ?? '' });

  return (
    <Stack gap="md">
      <Breadcrumb
        items={[
          { label: t('pages:alerts.channels.title'), to: '/alerts/channels' },
          { label: isNew ? t('pages:alerts.channels.edit.createTitle') : channel?.name ?? '' },
        ]}
      />

      <PageHeader
        title={title}
        subtitle={t('pages:alerts.channels.edit.subtitle')}
      />

      {/* Common fields */}
      <Card>
        <h3 className={styles.sectionTitle}>
          {t('pages:alerts.channels.edit.generalSection')}
        </h3>
        <Stack gap="md">
          <FormField label={t('pages:alerts.channels.edit.nameLabel')}>
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={t('pages:alerts.channels.edit.namePlaceholder')}
            />
          </FormField>
          <FormField label={t('pages:alerts.channels.edit.typeLabel')}>
            <Select
              value={type}
              onValueChange={(v) => setType(v as AlertChannel['type'])}
              options={typeOptions}
            />
          </FormField>
          <div className={styles.switchRow}>
            <label>{t('pages:alerts.channels.edit.enabledLabel')}</label>
            <Switch checked={enabled} onCheckedChange={setEnabled} />
          </div>
          <MultiSelectDropdown
            label={t('pages:alerts.channels.edit.severitiesLabel')}
            emptyLabel={t('pages:alerts.channels.edit.severitiesAll')}
            options={severityOptions}
            value={severities}
            onChange={(next) => setSeverities(next as AlertSeverity[])}
          />
          <MultiSelectDropdown
            label={t('pages:alerts.channels.edit.sourceTypesLabel')}
            emptyLabel={t('pages:alerts.channels.edit.sourceTypesAll')}
            options={sourceTypeOptions}
            value={sourceTypes}
            onChange={setSourceTypes}
          />
          <p className={styles.hint}>
            {t('pages:alerts.channels.edit.sourceTypesHelp')}
          </p>
        </Stack>
      </Card>

      {/* Per-type config panel */}
      <Card>
        <h3 className={styles.sectionTitle}>
          {t(`pages:alerts.channelEditors.${type}.sectionTitle`)}
        </h3>

        {type === 'webhook' && (
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
              <p className={styles.hint}>
                {t('pages:alerts.channelEditors.webhook.headersHelp')}
              </p>
              {headers.length > 0 && (
                <div className={styles.headerRowHeader}>
                  <span>{t('pages:alerts.channelEditors.webhook.headerKey')}</span>
                  <span>{t('pages:alerts.channelEditors.webhook.headerValue')}</span>
                  <span />
                </div>
              )}
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
                      placeholder="X-Custom-Header"
                    />
                    <div className={styles.secretRow}>
                      <Input
                        value={row.value}
                        readOnly={row.masked}
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
        )}

        {type === 'slack' && (
          <Stack gap="md">
            <p className={styles.hint}>
              {t('pages:alerts.channelEditors.slack.modeHelp')}
            </p>
            <FormField
              label={t('pages:alerts.channelEditors.slack.webhookUrlLabel')}
              helpText={t('pages:alerts.channelEditors.slack.webhookUrlHelp')}
            >
              <Input
                type="url"
                value={slackWebhookUrl}
                onChange={(e) => setSlackWebhookUrl(e.target.value)}
                placeholder="https://hooks.slack.com/services/…"
              />
            </FormField>
            <FormField label={t('pages:alerts.channelEditors.slack.botTokenLabel')}>
              <div className={styles.secretRow}>
                <Input
                  type={slackBotTokenMasked ? 'text' : 'password'}
                  value={slackBotToken}
                  readOnly={slackBotTokenMasked}
                  onChange={(e) => setSlackBotToken(e.target.value)}
                  placeholder="xoxb-…"
                />
                {slackBotTokenMasked && (
                  <Button
                    type="button"
                    variant="secondary"
                    size="sm"
                    onClick={() => {
                      setSlackBotToken('');
                      setSlackBotTokenMasked(false);
                    }}
                  >
                    {t('pages:alerts.channels.edit.changeSecret')}
                  </Button>
                )}
              </div>
            </FormField>
            <FormField label={t('pages:alerts.channelEditors.slack.channelLabel')}>
              <Input
                value={slackChannel}
                onChange={(e) => setSlackChannel(e.target.value)}
                placeholder="#alerts"
              />
            </FormField>
          </Stack>
        )}

        {type === 'email' && (
          <Stack gap="md">
            <div className={styles.twoColumn}>
              <FormField label={t('pages:alerts.channelEditors.email.smtpHostLabel')}>
                <Input
                  value={smtpHost}
                  onChange={(e) => setSmtpHost(e.target.value)}
                  placeholder="smtp.example.com"
                />
              </FormField>
              <FormField label={t('pages:alerts.channelEditors.email.smtpPortLabel')}>
                <Input
                  type="number"
                  value={smtpPort}
                  onChange={(e) => setSmtpPort(e.target.value)}
                  min={1}
                  max={65535}
                />
              </FormField>
            </div>
            <div className={styles.twoColumn}>
              <FormField label={t('pages:alerts.channelEditors.email.fromLabel')}>
                <Input
                  type="email"
                  value={smtpFrom}
                  onChange={(e) => setSmtpFrom(e.target.value)}
                  placeholder="alerts@example.com"
                />
              </FormField>
              <FormField label={t('pages:alerts.channelEditors.email.toLabel')}>
                <Input
                  type="email"
                  value={smtpTo}
                  onChange={(e) => setSmtpTo(e.target.value)}
                  placeholder="oncall@example.com"
                />
              </FormField>
            </div>
            <div className={styles.twoColumn}>
              <FormField label={t('pages:alerts.channelEditors.email.usernameLabel')}>
                <Input
                  value={smtpUsername}
                  onChange={(e) => setSmtpUsername(e.target.value)}
                  autoComplete="off"
                />
              </FormField>
              <FormField label={t('pages:alerts.channelEditors.email.passwordLabel')}>
                <div className={styles.secretRow}>
                  <Input
                    type={smtpPasswordMasked ? 'text' : 'password'}
                    value={smtpPassword}
                    readOnly={smtpPasswordMasked}
                    onChange={(e) => setSmtpPassword(e.target.value)}
                    autoComplete="new-password"
                  />
                  {smtpPasswordMasked && (
                    <Button
                      type="button"
                      variant="secondary"
                      size="sm"
                      onClick={() => {
                        setSmtpPassword('');
                        setSmtpPasswordMasked(false);
                      }}
                    >
                      {t('pages:alerts.channels.edit.changeSecret')}
                    </Button>
                  )}
                </div>
              </FormField>
            </div>
          </Stack>
        )}

        {type === 'pagerduty' && (
          <Stack gap="md">
            <FormField
              label={t('pages:alerts.channelEditors.pagerduty.routingKeyLabel')}
              helpText={t('pages:alerts.channelEditors.pagerduty.routingKeyHelp')}
            >
              <div className={styles.secretRow}>
                <Input
                  type={routingKeyMasked ? 'text' : 'password'}
                  value={routingKey}
                  readOnly={routingKeyMasked}
                  onChange={(e) => setRoutingKey(e.target.value)}
                  autoComplete="off"
                />
                {routingKeyMasked && (
                  <Button
                    type="button"
                    variant="secondary"
                    size="sm"
                    onClick={() => {
                      setRoutingKey('');
                      setRoutingKeyMasked(false);
                    }}
                  >
                    {t('pages:alerts.channels.edit.changeSecret')}
                  </Button>
                )}
              </div>
            </FormField>
          </Stack>
        )}
      </Card>

      {/* Footer */}
      <Stack direction="horizontal" gap="sm" className={styles.footerActions}>
        <Button variant="secondary" onClick={onCancel}>
          {t('common:cancel')}
        </Button>
        <Button onClick={onSave} disabled={saving} loading={saving}>
          {t('common:save')}
        </Button>
      </Stack>
    </Stack>
  );
}
