import { useTranslation } from 'react-i18next';
import type { AgentDevice } from '@/api/types';
import { Card } from '@/components/ui';
import { DeviceTagEditor } from '../DeviceTagEditor';
import styles from '../FleetDeviceDetailPage.module.css';

interface IdentityCardProps {
  device: AgentDevice;
  copyToClipboard: (text: string) => void;
  onTagsSaved: () => void;
}

export function IdentityCard({ device, copyToClipboard, onTagsSaved }: IdentityCardProps) {
  const { t } = useTranslation();
  return (
    /* Identity card — first-class natural-key identifiers + currently
        bound user. Replaces the old simple kvGrid. */
    <Card>
      <div className={styles.kvGrid}>
        <div>
          <div className={styles.kvLabel}>{t('pages:devices.identity.hostname')}</div>
          <div className={styles.kvValue}>{device.hostname}</div>
        </div>
        {device.boundUserDisplayName && (
          <div>
            <div className={styles.kvLabel}>{t('pages:devices.identity.boundUser')}</div>
            <div className={styles.kvValue}>
              {device.boundUserDisplayName}
              {device.boundUserEmail && <span style={{ color: 'var(--color-text-muted)' }}>{' · '}{device.boundUserEmail}</span>}
            </div>
          </div>
        )}
        {device.physicalId && (
          <div>
            <div className={styles.kvLabel}>{t('pages:devices.identity.physicalId')}</div>
            <div className={styles.kvValue}>
              <code>{device.physicalId}</code>
              <button
                type="button"
                onClick={() => copyToClipboard(device.physicalId!)}
                title={t('common:copy')}
                aria-label={t('common:copy')}
                data-tooltip={t('common:copy')}
                className={styles.copyButton}
              >
                <svg viewBox="0 0 20 20" fill="none" aria-hidden>
                  <rect x="7" y="6" width="8" height="10" rx="2.5" stroke="currentColor" strokeWidth="1.4" />
                  <rect x="4" y="3" width="8" height="10" rx="2.5" stroke="currentColor" strokeWidth="1.4" />
                </svg>
              </button>
            </div>
          </div>
        )}
        <div>
          <div className={styles.kvLabel}>{t('pages:devices.identity.thingId')}</div>
          <div className={styles.kvValue}>
            <code>{device.id}</code>
            <button
              type="button"
              onClick={() => copyToClipboard(device.id)}
              title={t('common:copy')}
              aria-label={t('common:copy')}
              data-tooltip={t('common:copy')}
              className={styles.copyButton}
            >
              <svg viewBox="0 0 20 20" fill="none" aria-hidden>
                <rect x="7" y="6" width="8" height="10" rx="2.5" stroke="currentColor" strokeWidth="1.4" />
                <rect x="4" y="3" width="8" height="10" rx="2.5" stroke="currentColor" strokeWidth="1.4" />
              </svg>
            </button>
          </div>
        </div>
        {device.primaryIp && (
          <div>
            <div className={styles.kvLabel}>{t('pages:devices.identity.ip')}</div>
            <div className={styles.kvValue}><code>{device.primaryIp}</code></div>
          </div>
        )}
        <div>
          <div className={styles.kvLabel}>{t('pages:fleet.os')}</div>
          <div className={styles.kvValue}>{device.os === 'darwin' ? 'macOS' : device.os} {device.osVersion}</div>
        </div>
        <div>
          <div className={styles.kvLabel}>{t('pages:fleet.agentVersion')}</div>
          <div className={styles.kvValue}>{device.agentVersion}</div>
        </div>
        <div>
          <div className={styles.kvLabel}>{t('pages:fleet.lastHeartbeat')}</div>
          <div className={styles.kvValue}>{device.lastHeartbeat ? new Date(device.lastHeartbeat).toLocaleString() : '—'}</div>
        </div>
        <div>
          <div className={styles.kvLabel}>{t('pages:devices.enrolledAt')}</div>
          <div className={styles.kvValue}>{new Date(device.enrolledAt).toLocaleString()} {device.enrolledBy ? `· ${device.enrolledBy}` : ''}</div>
        </div>
      </div>
      {/* Tag editor — below the kvGrid so it stays inside the Identity card */}
      <div className={styles.tagSection} style={{ marginTop: 'var(--g-space-4)', paddingTop: 'var(--g-space-3)', borderTop: '1px solid var(--color-border)' }}>
        <div style={{ fontSize: 'var(--g-font-size-sm)', fontWeight: 'var(--g-font-weight-semibold)', marginBottom: 'var(--g-space-2)' }}>
          {t('pages:devices.tagsLabel', 'Tags')}
        </div>
        <DeviceTagEditor
          deviceId={device.id}
          initialTags={device.tags ?? []}
          onSaved={onTagsSaved}
        />
      </div>
    </Card>
  );
}
