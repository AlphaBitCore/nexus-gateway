/**
 * AlertChannelEditPage.ConfigPanel — the per-type config section of the alert
 * channel editor (webhook | slack | email | pagerduty).
 *
 * Receives the whole form object from `useChannelForm` and fans the relevant
 * fields out to the matching config subcomponent. Behavior and rendered output
 * are identical to the inline block that previously lived in
 * AlertChannelEditPage; this file only relocates that block.
 */
import { useTranslation } from 'react-i18next';
import { Card } from '@/components/ui';
import { useChannelForm } from './useChannelForm';
import { WebhookConfig } from './WebhookConfig';
import { SlackConfig } from './SlackConfig';
import { EmailConfig } from './EmailConfig';
import { PagerDutyConfig } from './PagerDutyConfig';
import styles from './AlertChannelEditPage.module.css';

type ChannelForm = ReturnType<typeof useChannelForm>;

export function AlertChannelConfigPanel({ form }: { form: ChannelForm }) {
  const { t } = useTranslation();
  const {
    type,
    webhookUrl,
    setWebhookUrl,
    headers,
    setHeaders,
    slackWebhookUrl,
    setSlackWebhookUrl,
    slackBotToken,
    setSlackBotToken,
    slackBotTokenMasked,
    setSlackBotTokenMasked,
    slackChannel,
    setSlackChannel,
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
    routingKey,
    setRoutingKey,
    routingKeyMasked,
    setRoutingKeyMasked,
  } = form;

  return (
    <section className={styles.contentSection}>
      <h3 className={styles.sectionTitle}>
        {t(`pages:alerts.channelEditors.${type}.sectionTitle`)}
      </h3>
      <Card>

      {type === 'webhook' && (
        <WebhookConfig
          webhookUrl={webhookUrl}
          setWebhookUrl={setWebhookUrl}
          headers={headers}
          setHeaders={setHeaders}
        />
      )}

      {type === 'slack' && (
        <SlackConfig
          slackWebhookUrl={slackWebhookUrl}
          setSlackWebhookUrl={setSlackWebhookUrl}
          slackBotToken={slackBotToken}
          setSlackBotToken={setSlackBotToken}
          slackBotTokenMasked={slackBotTokenMasked}
          setSlackBotTokenMasked={setSlackBotTokenMasked}
          slackChannel={slackChannel}
          setSlackChannel={setSlackChannel}
        />
      )}

      {type === 'email' && (
        <EmailConfig
          smtpHost={smtpHost}
          setSmtpHost={setSmtpHost}
          smtpPort={smtpPort}
          setSmtpPort={setSmtpPort}
          smtpFrom={smtpFrom}
          setSmtpFrom={setSmtpFrom}
          smtpTo={smtpTo}
          setSmtpTo={setSmtpTo}
          smtpUsername={smtpUsername}
          setSmtpUsername={setSmtpUsername}
          smtpPassword={smtpPassword}
          setSmtpPassword={setSmtpPassword}
          smtpPasswordMasked={smtpPasswordMasked}
          setSmtpPasswordMasked={setSmtpPasswordMasked}
        />
      )}

      {type === 'pagerduty' && (
        <PagerDutyConfig
          routingKey={routingKey}
          setRoutingKey={setRoutingKey}
          routingKeyMasked={routingKeyMasked}
          setRoutingKeyMasked={setRoutingKeyMasked}
        />
      )}
      </Card>
    </section>
  );
}
