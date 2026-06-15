import { useState } from 'react';
import { useTranslation } from 'react-i18next';
import { PageHeader, Stack, Card, Button } from '@/components/ui';
import { useApi } from '@/hooks/useApi';
import { serviceUrlsApi, type ServicePublicURLs } from '@/api/services';
import styles from './InfraCliSetupPage.module.css';

// The nexus operator toolkit ("nexus") is a single static Go binary with
// two faces (TUI / CLI). Unlike the agent it needs no system
// extension, kernel driver, or device enrollment — so this page stays lean:
// download → install → quickstart → command reference. No FAQ/live-status.

type Platform = 'macos' | 'linux' | 'windows';
const PLATFORMS: Platform[] = ['macos', 'linux', 'windows'];

const PLATFORM_LABEL_KEYS: Record<Platform, string> = {
  macos: 'infrastructure.platformMacOS',
  linux: 'infrastructure.platformLinux',
  windows: 'infrastructure.platformWindows',
};

// Download files served from /downloads/ on the CP origin (uploaded by the
// prod-deploy flow). macOS ships two arch builds; the binaries are unsigned,
// so the macOS install steps include the Gatekeeper un-quarantine command.
interface CliAsset {
  labelKey: string;
  url: string;
}
const DOWNLOADS: Record<Platform, CliAsset[]> = {
  macos: [
    { labelKey: 'infrastructure.cliSetup.archAppleSilicon', url: '/downloads/nexus-cli-darwin-arm64-latest' },
    { labelKey: 'infrastructure.cliSetup.archIntel', url: '/downloads/nexus-cli-darwin-amd64-latest' },
  ],
  linux: [{ labelKey: 'infrastructure.cliSetup.archLinuxAmd64', url: '/downloads/nexus-cli-linux-amd64-latest' }],
  windows: [{ labelKey: 'infrastructure.cliSetup.archWindowsAmd64', url: '/downloads/nexus-cli-windows-amd64-latest.exe' }],
};

// Per-platform install step keys. macOS includes the xattr quarantine-clear
// step (binaries are unsigned); Windows is rename + PATH; Linux is chmod + mv.
const INSTALL_STEPS: Record<Platform, string[]> = {
  macos: [
    'infrastructure.cliSetup.installMacOSStep1',
    'infrastructure.cliSetup.installMacOSStep2',
    'infrastructure.cliSetup.installMacOSStep3',
  ],
  linux: [
    'infrastructure.cliSetup.installLinuxStep1',
    'infrastructure.cliSetup.installLinuxStep2',
  ],
  windows: [
    'infrastructure.cliSetup.installWindowsStep1',
    'infrastructure.cliSetup.installWindowsStep2',
  ],
};

// Command reference — sourced verbatim from `nexus --help`.
// `completion` and `help` (cobra built-ins) are intentionally omitted.
const COMMANDS = [
  'setup', 'login', 'env', 'chat', 'models', 'vk', 'route', 'traffic',
  'cost', 'health', 'slo', 'simulate', 'resource', 'killswitch',
  'passthrough',
] as const;

export default function InfraCliSetupPage() {
  const { t } = useTranslation('pages');
  const [platform, setPlatform] = useState<Platform>('macos');

  // Source the download host from the CP's own registered publicURL; fall
  // back to window.origin while the lookup is in flight. Mirrors the agent
  // setup page so the install commands show a real, copy-pasteable host.
  const { data: services } = useApi<ServicePublicURLs>(
    serviceUrlsApi.publicURLs,
    ['admin', 'services', 'public-urls'],
  );
  const downloadBase =
    services?.controlPlane?.replace(/\/+$/, '') ||
    (typeof window !== 'undefined' ? window.location.origin : '');

  return (
    <Stack gap="lg">
      <PageHeader
        title={t('infrastructure.cliSetupTitle')}
        subtitle={t('infrastructure.cliSetupDescription')}
      />

      {/* ─── Install card — platform-scoped download → install → quickstart ─── */}
      <Card>
        <Stack gap="md">
          <div className={styles.platformRow}>
            {PLATFORMS.map((p) => (
              <Button
                key={p}
                variant={platform === p ? 'primary' : 'secondary'}
                size="sm"
                onClick={() => setPlatform(p)}
              >
                {t(PLATFORM_LABEL_KEYS[p])}
              </Button>
            ))}
          </div>

          {/* 1. Download */}
          <section>
            <h3 className={styles.sectionHeading}>{t('infrastructure.cliSetup.downloadTitle')}</h3>
            <p>{t('infrastructure.cliSetup.downloadHint')}</p>
            <div className={styles.downloadList}>
              {DOWNLOADS[platform].map((asset) => (
                <div key={asset.url} className={styles.downloadRow}>
                  <Button
                    variant="primary"
                    size="sm"
                    onClick={() => {
                      window.location.href = asset.url;
                    }}
                  >
                    {t('infrastructure.cliSetup.downloadButton', { arch: t(asset.labelKey) })}
                  </Button>
                  <span className={styles.downloadUrl}>{asset.url}</span>
                </div>
              ))}
            </div>
          </section>

          {/* 2. Install */}
          <section>
            <h3 className={styles.sectionHeading}>{t('infrastructure.cliSetup.installTitle')}</h3>
            <ol className={styles.steps}>
              {INSTALL_STEPS[platform].map((key) => (
                <li key={key}>
                  <pre className={styles.codeBlock}>
                    <code>{t(key, { downloadBase })}</code>
                  </pre>
                </li>
              ))}
            </ol>
          </section>

          {/* 3. Quickstart — from packages/nexus-cli/README.md */}
          <section>
            <h3 className={styles.sectionHeading}>{t('infrastructure.cliSetup.quickstartTitle')}</h3>
            <p>{t('infrastructure.cliSetup.quickstartHint')}</p>
            <pre className={styles.codeBlock}>
              <code>{t('infrastructure.cliSetup.quickstartCode')}</code>
            </pre>
          </section>
        </Stack>
      </Card>

      {/* ─── Command reference card ─── */}
      <Card>
        <Stack gap="md">
          <div>
            <h3 className={styles.sectionHeading}>{t('infrastructure.cliSetup.commandsTitle')}</h3>
            <p className={styles.muted}>{t('infrastructure.cliSetup.commandsHint')}</p>
          </div>
          <table className={styles.cmdTable}>
            <thead>
              <tr>
                <th>{t('infrastructure.cliSetup.colCommand')}</th>
                <th>{t('infrastructure.cliSetup.colDescription')}</th>
              </tr>
            </thead>
            <tbody>
              {COMMANDS.map((cmd) => (
                <tr key={cmd}>
                  <td className={styles.cmdName}>nexus {cmd}</td>
                  <td>{t(`infrastructure.cliSetup.cmd.${cmd}`)}</td>
                </tr>
              ))}
            </tbody>
          </table>
          <p className={styles.muted}>{t('infrastructure.cliSetup.commandsFooter')}</p>
        </Stack>
      </Card>
    </Stack>
  );
}
