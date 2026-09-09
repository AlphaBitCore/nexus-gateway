import { useCallback, useEffect, useState } from 'react';
import type { AlertChannel, AlertSeverity } from '@/api/services';
import {
  headersListToObject,
  headersObjectToList,
  isMasked,
  type HeaderRow,
} from './channelMasking';

/**
 * useChannelForm — owns the entire alert-channel form: common fields plus
 * the four per-type config blocks (webhook / slack / email / pagerduty),
 * seeds state from the fetched channel, and serializes the per-type config
 * blob for save via `buildConfig`.
 */
export function useChannelForm(channel: AlertChannel | null | undefined, isNew: boolean) {
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

  return {
    // Common
    name,
    setName,
    type,
    setType,
    enabled,
    setEnabled,
    severities,
    setSeverities,
    sourceTypes,
    setSourceTypes,
    // Webhook
    webhookUrl,
    setWebhookUrl,
    headers,
    setHeaders,
    // Slack
    slackWebhookUrl,
    setSlackWebhookUrl,
    slackBotToken,
    setSlackBotToken,
    slackBotTokenMasked,
    setSlackBotTokenMasked,
    slackChannel,
    setSlackChannel,
    // Email / SMTP
    smtpHost,
    setSmtpHost,
    smtpPort,
    setSmtpPort,
    smtpFrom,
    setSmtpFrom,
    smtpTo,
    setSmtpTo,
    smtpUsername,
    setSmtpUsername,
    smtpPassword,
    setSmtpPassword,
    smtpPasswordMasked,
    setSmtpPasswordMasked,
    // PagerDuty
    routingKey,
    setRoutingKey,
    routingKeyMasked,
    setRoutingKeyMasked,
    // Serialize
    buildConfig,
  };
}
