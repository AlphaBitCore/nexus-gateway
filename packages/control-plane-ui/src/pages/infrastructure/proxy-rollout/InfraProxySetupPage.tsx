import { useState, useEffect } from 'react';
import { useParams } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import styles from './InfraProxySetupPage.module.css';
import { useApi } from '@/hooks/useApi';
import { useMutation } from '@/hooks/useMutation';
import { hubApi } from '@/api/services/infrastructure/nodes/hub';
import {
  downloadCACert,
  downloadMDMProfile,
  downloadPACFile,
  patchOnboarding,
} from '@/api/services/system/setup';
import {
  Breadcrumb,
  Button,
  Card,
  Checkbox,
  Divider,
  ErrorBanner,
  FormField,
  Input,
  PageHeader,
  Stack,
  Switch,
  Badge,
  Tabs,
  TabsList,
  TabsTrigger,
  TabsContent,
} from '@/components/ui';

// ── Section card with numbered header ────────────────────────────────────────

function SetupSection({
  step,
  icon,
  title,
  description,
  children,
}: {
  step: number;
  icon: string;
  title: string;
  description: string;
  children: React.ReactNode;
}) {
  return (
    <Card className={styles.setupSectionCard}>
      <Stack gap="md">
        <div className={styles.sectionHeader}>
          <div className={styles.stepCircle}>{step}</div>
          <div className={styles.sectionContent}>
            <div className={styles.sectionTitleRow}>
              <span style={{ fontSize: 'var(--g-font-size-lg)' }}>{icon}</span>
              <span style={{ fontWeight: 'var(--g-font-weight-semibold)', fontSize: 'var(--g-font-size-md)' }}>{title}</span>
            </div>
            <p className={styles.sectionDescription}>{description}</p>
          </div>
        </div>

        <div className={styles.sectionBody}>{children}</div>
      </Stack>
    </Card>
  );
}

// ── Main page ─────────────────────────────────────────────────────────────────

export default function InfraProxySetupPage() {
  const { t } = useTranslation('pages');
  const { id: thingId = '' } = useParams<{ id: string }>();

  // Fetch node to get its display name and current onboarding desired state.
  // Uses a 'setup' suffix in the queryKey so React Query doesn't collide with
  // the node-detail page's own cache entry for the same thingId.
  const { data: node } = useApi(
    () => hubApi.getNode(thingId),
    ['admin', 'nodes', 'detail', thingId, 'setup'],
  );

  const nodeName = node?.name ?? thingId;
  const online = node?.status === 'online';

  // ── MDM state ──────────────────────────────────────────────────────────────
  const [organization, setOrganization] = useState('');

  // ── PAC state ──────────────────────────────────────────────────────────────
  const [proxyHost, setProxyHost] = useState('');
  const [proxyPort, setProxyPort] = useState('3128');
  const [failOpen, setFailOpen] = useState(false);

  // ── Onboarding state ───────────────────────────────────────────────────────
  const [onboardingEnabled, setOnboardingEnabled] = useState(false);
  const [onboardingPushedAt, setOnboardingPushedAt] = useState<string | null>(null);

  // Initialize onboarding toggle from node's target config once loaded.
  useEffect(() => {
    if (!node?.targetConfig) return;
    const cfg = node.targetConfig as Record<string, Record<string, unknown>>;
    const enabled = cfg['onboarding']?.['enabled'];
    if (typeof enabled === 'boolean') setOnboardingEnabled(enabled);
  }, [node]);

  // ── Mutations ──────────────────────────────────────────────────────────────

  const caCertMutation = useMutation(
    (_: void) => downloadCACert(thingId),
    { successMessage: undefined },
  );

  const mdmMutation = useMutation(
    (_: void) => downloadMDMProfile(thingId, organization || undefined),
    { successMessage: undefined },
  );

  const pacMutation = useMutation(
    (_: void) => downloadPACFile(thingId, { proxyHost, proxyPort, failOpen }),
    { successMessage: undefined },
  );

  const onboardingMutation = useMutation(
    (nextEnabled: boolean) => patchOnboarding(thingId, nextEnabled),
    {
      successMessage: undefined,
      onSuccess: (result) => {
        setOnboardingEnabled(result.enabled);
        setOnboardingPushedAt(result.pushedAt);
      },
    },
  );

  const handleOnboardingToggle = (next: boolean) => {
    void onboardingMutation.mutate(next);
  };

  const pacCanDownload = proxyHost.trim() !== '' && proxyPort.trim() !== '';

  return (
    <Stack gap="lg">
      <Breadcrumb
        items={[
          { label: t('infrastructure.nodesTitle'), to: '/infrastructure/nodes' },
          { label: nodeName, to: `/infrastructure/nodes/${thingId}` },
          { label: t('infrastructure.proxySetupTitle') },
        ]}
      />

      <PageHeader
        title={t('infrastructure.proxySetupTitle')}
        subtitle={nodeName !== thingId ? nodeName : undefined}
      />

      {node && !online && (
        <ErrorBanner message={t('infrastructure.offlineSetupWarning')} />
      )}

      {/* ── Step 1: CA Certificate ── */}
      <SetupSection
        step={1}
        icon="🔐"
        title={t('infrastructure.caTitle')}
        description={t('infrastructure.caDescription')}
      >
        <Stack gap="md">
          {caCertMutation.error && <ErrorBanner message={caCertMutation.error.message} />}
          <Button
            variant="primary"
            loading={caCertMutation.loading}
            onClick={() => void caCertMutation.mutate()}
          >
            {caCertMutation.loading ? t('infrastructure.downloading') : t('infrastructure.downloadCACert')}
          </Button>

          <Divider />

          <div>
            <p className={styles.manualInstallTitle}>{t('infrastructure.caManualTitle')}</p>
            <p className={styles.manualInstallSubtitle}>{t('infrastructure.caManualSubtitle')}</p>
            <Tabs defaultValue="macos">
              <TabsList>
                <TabsTrigger value="macos">{t('infrastructure.caMacTitle')}</TabsTrigger>
                <TabsTrigger value="windows">{t('infrastructure.caWinTitle')}</TabsTrigger>
                <TabsTrigger value="linux">{t('infrastructure.caLinuxTitle')}</TabsTrigger>
              </TabsList>
              <TabsContent value="macos">
                <Stack gap="sm">
                  <p style={{ fontSize: 'var(--g-font-size-base)', margin: 'var(--g-space-0)' }}>{t('infrastructure.caMacStep1')}</p>
                  <code className={styles.codeBlock}>{t('infrastructure.caMacCommand')}</code>
                  <p style={{ fontSize: 'var(--g-font-size-base)', margin: 'var(--g-space-0)' }}>{t('infrastructure.caMacStep2')}</p>
                </Stack>
              </TabsContent>
              <TabsContent value="windows">
                <ol className={styles.stepList}>
                  <li>{t('infrastructure.caWinStep1')}</li>
                  <li>{t('infrastructure.caWinStep2')}</li>
                  <li>{t('infrastructure.caWinStep3')}</li>
                  <li>{t('infrastructure.caWinStep4')}</li>
                  <li>{t('infrastructure.caWinStep5')}</li>
                  <li>{t('infrastructure.caWinStep6')}</li>
                </ol>
              </TabsContent>
              <TabsContent value="linux">
                <Stack gap="sm">
                  <p style={{ fontSize: 'var(--g-font-size-base)', margin: 'var(--g-space-0)' }}>{t('infrastructure.caLinuxStep1')}</p>
                  <p style={{ fontSize: 'var(--g-font-size-base)', margin: 'var(--g-space-0)', fontWeight: 'var(--g-font-weight-medium)' }}>{t('infrastructure.caLinuxDebianTitle')}</p>
                  <code className={styles.codeBlock}>{t('infrastructure.caLinuxDebianCommand')}</code>
                  <p style={{ fontSize: 'var(--g-font-size-base)', margin: 'var(--g-space-0)', fontWeight: 'var(--g-font-weight-medium)' }}>{t('infrastructure.caLinuxRhelTitle')}</p>
                  <code className={styles.codeBlock}>{t('infrastructure.caLinuxRhelCommand')}</code>
                  <p style={{ fontSize: 'var(--g-font-size-base)', margin: 'var(--g-space-0)' }}>{t('infrastructure.caLinuxStep2')}</p>
                </Stack>
              </TabsContent>
            </Tabs>
          </div>
        </Stack>
      </SetupSection>

      {/* ── Step 2: MDM Profile ── */}
      <SetupSection
        step={2}
        icon="📱"
        title={t('infrastructure.mdmTitle')}
        description={t('infrastructure.mdmDescription')}
      >
        <Stack gap="sm">
          <FormField label={t('infrastructure.mdmOrgLabel')}>
            <Input
              value={organization}
              onChange={(e) => setOrganization(e.target.value)}
              placeholder={t('pages:proxySetup.orgExamplePlaceholder')}
            />
          </FormField>
          {mdmMutation.error && <ErrorBanner message={mdmMutation.error.message} />}
          <Button
            variant="primary"
            loading={mdmMutation.loading}
            onClick={() => void mdmMutation.mutate()}
          >
            {mdmMutation.loading ? t('infrastructure.downloading') : t('infrastructure.downloadMDMProfile')}
          </Button>
        </Stack>
      </SetupSection>

      {/* ── Step 3: PAC File ── */}
      <SetupSection
        step={3}
        icon="🌐"
        title={t('infrastructure.pacTitle')}
        description={t('infrastructure.pacDescription')}
      >
        <Stack gap="sm">
          <div style={{ display: 'flex', gap: 'var(--g-space-3)', flexWrap: 'wrap' }}>
            <div style={{ flex: '1 1 220px', minWidth: 0 }}>
              <FormField label={t('infrastructure.proxyHostLabel')}>
                <Input
                  value={proxyHost}
                  onChange={(e) => setProxyHost(e.target.value)}
                  placeholder="proxy.example.com"
                />
              </FormField>
            </div>
            <div style={{ flex: '0 0 110px' }}>
              <FormField label={t('infrastructure.proxyPortLabel')}>
                <Input
                  value={proxyPort}
                  onChange={(e) => setProxyPort(e.target.value)}
                  placeholder="3128"
                />
              </FormField>
            </div>
          </div>
          <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-2)' }}>
            <Checkbox
              checked={failOpen}
              onCheckedChange={(v) => setFailOpen(v === true)}
            />
            <span style={{ fontSize: 'var(--g-font-size-base)' }}>{t('infrastructure.failOpenLabel')}</span>
          </div>
          {pacMutation.error && <ErrorBanner message={pacMutation.error.message} />}
          <Button
            variant="primary"
            loading={pacMutation.loading}
            disabled={!pacCanDownload}
            onClick={() => void pacMutation.mutate()}
          >
            {pacMutation.loading ? t('infrastructure.downloading') : t('infrastructure.downloadPACFile')}
          </Button>

          <Divider />

          <div>
            <p className={styles.manualInstallTitle}>{t('infrastructure.manualProxyTitle')}</p>
            <p className={styles.manualInstallSubtitle}>{t('infrastructure.manualProxySubtitle')}</p>
            <div className={styles.manualProxyValues}>
              <span><strong>{t('infrastructure.manualProxyHostLabel')}:</strong> <code>{proxyHost.trim() || t('infrastructure.manualProxyHostPlaceholder')}</code></span>
              <span><strong>{t('infrastructure.manualProxyPortLabel')}:</strong> <code>{proxyPort.trim() || '3128'}</code></span>
            </div>
            <Tabs defaultValue="macos">
              <TabsList>
                <TabsTrigger value="macos">{t('infrastructure.manualProxyMacTitle')}</TabsTrigger>
                <TabsTrigger value="windows">{t('infrastructure.manualProxyWinTitle')}</TabsTrigger>
                <TabsTrigger value="linux">{t('infrastructure.manualProxyLinuxTitle')}</TabsTrigger>
                <TabsTrigger value="ios">{t('infrastructure.manualProxyIosTitle')}</TabsTrigger>
                <TabsTrigger value="android">{t('infrastructure.manualProxyAndroidTitle')}</TabsTrigger>
              </TabsList>
              <TabsContent value="macos">
                <ol className={styles.stepList}>
                  <li>{t('infrastructure.manualProxyMacStep1')}</li>
                  <li>{t('infrastructure.manualProxyMacStep2')}</li>
                  <li>{t('infrastructure.manualProxyMacStep3')}</li>
                </ol>
              </TabsContent>
              <TabsContent value="windows">
                <ol className={styles.stepList}>
                  <li>{t('infrastructure.manualProxyWinStep1')}</li>
                  <li>{t('infrastructure.manualProxyWinStep2')}</li>
                  <li>{t('infrastructure.manualProxyWinStep3')}</li>
                </ol>
              </TabsContent>
              <TabsContent value="linux">
                <ol className={styles.stepList}>
                  <li>{t('infrastructure.manualProxyLinuxStep1')}</li>
                  <li>{t('infrastructure.manualProxyLinuxStep2')}</li>
                </ol>
              </TabsContent>
              <TabsContent value="ios">
                <ol className={styles.stepList}>
                  <li>{t('infrastructure.manualProxyIosStep1')}</li>
                  <li>{t('infrastructure.manualProxyIosStep2')}</li>
                  <li>{t('infrastructure.manualProxyIosStep3')}</li>
                </ol>
              </TabsContent>
              <TabsContent value="android">
                <ol className={styles.stepList}>
                  <li>{t('infrastructure.manualProxyAndroidStep1')}</li>
                  <li>{t('infrastructure.manualProxyAndroidStep2')}</li>
                  <li>{t('infrastructure.manualProxyAndroidStep3')}</li>
                </ol>
              </TabsContent>
            </Tabs>
          </div>
        </Stack>
      </SetupSection>

      {/* ── Step 4: Onboarding Mode ── */}
      <SetupSection
        step={4}
        icon="🚀"
        title={t('infrastructure.onboardingTitle')}
        description={t('infrastructure.onboardingDescription')}
      >
        <Stack gap="sm">
          <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--g-space-3)' }}>
            <Switch
              checked={onboardingEnabled}
              onCheckedChange={handleOnboardingToggle}
              disabled={onboardingMutation.loading}
              aria-label={t('infrastructure.onboardingTitle')}
            />
            <div>
              <Badge variant={onboardingEnabled ? 'success' : 'default'}>
                {onboardingEnabled ? t('infrastructure.onboardingOn') : t('infrastructure.onboardingOff')}
              </Badge>
            </div>
            {onboardingMutation.loading && (
              <span className={styles.savingText}>
                {t('infrastructure.saving')}
              </span>
            )}
          </div>
          {onboardingPushedAt && (
            <p className={styles.lastPushedText}>
              {t('infrastructure.onboardingLastPushed')}: {new Date(onboardingPushedAt).toLocaleString()}
            </p>
          )}
          {onboardingMutation.error && <ErrorBanner message={onboardingMutation.error.message} />}
        </Stack>
      </SetupSection>
    </Stack>
  );
}
