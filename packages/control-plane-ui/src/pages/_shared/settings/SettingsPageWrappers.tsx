/**
 * Thin page wrappers for the settings IA refactor.
 *
 * Before the refactor each settings concern lived as a tab inside SettingsPage.
 * After the refactor each concern is its own route under the natural product
 * section (IAM / Devices / AI Gateway / Compliance / Infrastructure).
 * Rather than copy the form logic into eight new files, we wrap the
 * existing Settings*Tab component (which is self-contained) in a
 * PageHeader so it reads as a proper standalone page in the new nav.
 *
 * The Settings*Tab files themselves are unchanged; this file is the
 * only new code surface for the route refactor.
 */
import { useTranslation } from 'react-i18next';
import { PageHeader, Stack } from '@/components/ui';
import { SettingsAgentTab } from '../../devices/agent-defaults/SettingsAgentTab';
import { SettingsObservabilityTab } from '../../infrastructure/observability/SettingsObservabilityTab';
import { SettingsPayloadCaptureTab } from '../../compliance/payload-capture/SettingsPayloadCaptureTab';
import { SettingsStreamingComplianceTab } from '../../compliance/streaming-compliance/SettingsStreamingComplianceTab';
import { SettingsSiemTab } from '../../infrastructure/siem/SettingsSiemTab';
import { SettingsCredentialReliabilityTab } from '../../ai-gateway/credentials/reliability/SettingsCredentialReliabilityTab';

interface WrapperProps {
  titleKey: string;
  subtitleKey?: string;
  defaultTitle: string;
  defaultSubtitle?: string;
  children: React.ReactNode;
}

function PageWrapper({ titleKey, subtitleKey, defaultTitle, defaultSubtitle, children }: WrapperProps) {
  const { t } = useTranslation();
  return (
    <Stack gap="lg">
      <PageHeader
        title={t(titleKey, defaultTitle)}
        subtitle={subtitleKey ? t(subtitleKey, defaultSubtitle ?? '') : defaultSubtitle}
      />
      {children}
    </Stack>
  );
}

export function AgentSettingsPage() {
  return (
    <PageWrapper
      titleKey="nav:deviceDefaults"
      subtitleKey="pages:settings.pageSubtitles.deviceDefaults"
      defaultTitle="Device Defaults"
      defaultSubtitle="Runtime defaults applied to enrolled devices — audit policy, forensics, shutdown warning copy."
    >
      <SettingsAgentTab />
    </PageWrapper>
  );
}

export function ObservabilityConfigPage() {
  return (
    <PageWrapper
      titleKey="nav:observabilityConfig"
      subtitleKey="pages:settings.pageSubtitles.observabilityConfig"
      defaultTitle="Observability"
      defaultSubtitle="Traffic-event sampling and retention."
    >
      <SettingsObservabilityTab />
    </PageWrapper>
  );
}

export function PayloadCaptureSettingsPage() {
  return (
    <PageWrapper
      titleKey="nav:payloadCapture"
      subtitleKey="pages:settings.pageSubtitles.payloadCapture"
      defaultTitle="Payload Capture"
      defaultSubtitle="Request / response body capture defaults and spill thresholds."
    >
      <SettingsPayloadCaptureTab />
    </PageWrapper>
  );
}

export function StreamingComplianceSettingsPage() {
  return (
    <PageWrapper
      titleKey="nav:streamingCompliance"
      subtitleKey="pages:settings.pageSubtitles.streamingCompliance"
      defaultTitle="Streaming Compliance"
      defaultSubtitle="Stream-mode safety hooks and buffering policy."
    >
      <SettingsStreamingComplianceTab />
    </PageWrapper>
  );
}

export function SiemSettingsPage() {
  return (
    <PageWrapper
      titleKey="nav:siem"
      subtitleKey="pages:settings.pageSubtitles.siem"
      defaultTitle="SIEM"
      defaultSubtitle="External SIEM forwarder configuration and event filters."
    >
      <SettingsSiemTab />
    </PageWrapper>
  );
}

export function CredentialReliabilitySettingsPage() {
  return (
    <PageWrapper
      titleKey="nav:credentialReliability"
      subtitleKey="pages:settings.pageSubtitles.credentialReliability"
      defaultTitle="Credential Reliability"
      defaultSubtitle="Circuit-breaker thresholds and retry defaults for AI provider credentials."
    >
      <SettingsCredentialReliabilityTab />
    </PageWrapper>
  );
}
