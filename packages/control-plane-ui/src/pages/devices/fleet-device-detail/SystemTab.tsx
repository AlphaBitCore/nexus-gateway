import { useTranslation } from 'react-i18next';
import { Card, Stack } from '@/components/ui';
import styles from '../FleetDeviceDetailPage.module.css';

interface SystemTabProps {
  // Loose JSON blob (machineId / osName / cpuModel / cpuCores / totalMemMB /
  // networkInterfaces[]…); typed as `any` to match the JSX below.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  sysinfo: any;
}

export function SystemTab({ sysinfo }: SystemTabProps) {
  const { t } = useTranslation();
  return (
    <Card>
      {sysinfo ? (
        <Stack gap="md">
          <div className={styles.kvGrid}>
            <div>
              <div className={styles.kvLabel}>{t('pages:fleet.machineId')}</div>
              <div className={styles.kvValue}>{sysinfo.machineId ?? '—'}</div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:fleet.osName')}</div>
              <div className={styles.kvValue}>{sysinfo.osName} {sysinfo.osVersion}</div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:fleet.cpuModel')}</div>
              <div className={styles.kvValue}>{sysinfo.cpuModel ?? '—'}</div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:fleet.cpuCores')}</div>
              <div className={styles.kvValue}>{sysinfo.cpuCores}</div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:fleet.totalMemMB')}</div>
              <div className={styles.kvValue}>{sysinfo.totalMemMB?.toLocaleString() ?? '—'}</div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:fleet.serialNumber')}</div>
              <div className={styles.kvValue}>{sysinfo.serialNumber ?? '—'}</div>
            </div>
            <div>
              <div className={styles.kvLabel}>{t('pages:fleet.modelName')}</div>
              <div className={styles.kvValue}>{sysinfo.modelName ?? '—'}</div>
            </div>
          </div>
          {sysinfo.networkInterfaces?.length > 0 && (
            <>
              <h4 className={styles.sectionTitle}>{t('pages:fleet.networkInterfaces')}</h4>
              <table className={styles.table}>
                <thead>
                  <tr>
                    <th className={styles.th}>{t('pages:fleet.ifName')}</th>
                    <th className={styles.th}>{t('pages:fleet.macAddress')}</th>
                    <th className={styles.th}>{t('pages:fleet.ips')}</th>
                  </tr>
                </thead>
                <tbody>
                  {sysinfo.networkInterfaces.map((nif: { name: string; macAddress: string; ips: string[] }, i: number) => (
                    <tr key={i}>
                      <td className={styles.td}>{nif.name}</td>
                      <td className={styles.td}>{nif.macAddress}</td>
                      <td className={styles.td}>{nif.ips?.join(', ')}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </>
          )}
        </Stack>
      ) : (
        <p className={styles.empty}>{t('pages:fleet.noSysinfo')}</p>
      )}
    </Card>
  );
}
