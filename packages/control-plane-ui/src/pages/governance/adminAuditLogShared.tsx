import { useState, useCallback, useMemo } from 'react';
import { useTranslation } from 'react-i18next';
import type { AdminAuditEntry } from '../../api/types';
import { DataTable } from '@/components/ui';
import type { DataTableColumn } from '@/components/ui';
import { formatDateTime } from '@/lib/format';
import styles from './adminAuditLogShared.module.css';

export const DRAWER_MS = 240;
export const DRAWER_WIDTH = 'min(480px, 100vw)';

export function relativeTime(dateStr: string): string {
  const now = Date.now();
  const then = new Date(dateStr).getTime();
  const diffMs = now - then;
  if (diffMs < 0) return 'just now';
  const seconds = Math.floor(diffMs / 1000);
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  return `${Math.floor(days / 30)}mo ago`;
}

export function CopyJsonButton({ json }: { json: string }) {
  const { t } = useTranslation();
  const [copied, setCopied] = useState(false);
  const handleCopy = useCallback(() => {
    void navigator.clipboard.writeText(json);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  }, [json]);
  return (
    <button
      type="button"
      onClick={handleCopy}
      className={copied ? styles.copyBtnCopied : styles.copyBtn}
    >
      {copied ? t('pages:audit.copied') : t('pages:audit.copyJson')}
    </button>
  );
}

function LongIdCell({ value }: { value: string | null | undefined }) {
  if (!value) return <>—</>;
  return <span title={value}>{value}</span>;
}

export function buildAdminAuditLogColumns(t: (key: string) => string, options?: { hideActor?: boolean }): DataTableColumn<AdminAuditEntry>[] {
  const hideActor = options?.hideActor === true;

  const actorCol: DataTableColumn<AdminAuditEntry> = {
    key: 'actorLabel',
    label: t('pages:audit.colUser'),
    cellClassName: styles.actorLabelCell,
  };
  const entityTypeCol: DataTableColumn<AdminAuditEntry> = {
    key: 'entityType',
    label: t('pages:audit.colEntityType'),
    cellClassName: styles.normalCell,
  };
  const entityIdCol: DataTableColumn<AdminAuditEntry> = {
    key: 'entityId',
    label: t('pages:audit.colEntityId'),
    cellClassName: styles.longIdCell,
    render: (r) => <LongIdCell value={r.entityId} />,
  };
  const actionCol: DataTableColumn<AdminAuditEntry> = {
    key: 'action',
    label: t('pages:audit.colAction'),
    cellClassName: styles.normalCell,
  };
  const timeCol: DataTableColumn<AdminAuditEntry> = {
    key: 'timestamp',
    label: t('pages:audit.colTime'),
    cellClassName: styles.normalCell,
    render: (r) => <span title={formatDateTime(r.timestamp)}>{relativeTime(r.timestamp)}</span>,
  };

  if (hideActor) {
    return [entityTypeCol, entityIdCol, actionCol, timeCol];
  }
  return [actorCol, entityTypeCol, entityIdCol, actionCol, timeCol];
}

function JsonStateBlock({ label, value }: { label: string; value: unknown }) {
  const text = JSON.stringify(value, null, 2);
  return (
    <div className={styles.stateBlockWrapper}>
      <div className={styles.stateBlockHeader}>
        <strong className={styles.stateBlockLabel}>{label}</strong>
        <CopyJsonButton json={text} />
      </div>
      <pre className={styles.preBlock}>{text}</pre>
    </div>
  );
}

interface AdminAuditEntryDrawerProps {
  selectedEntry: AdminAuditEntry;
  drawerVisible: boolean;
  onClose: () => void;
  titleId?: string;
  /** When true, omits Actor info (e.g. Settings "My admin audit"). */
  hideActor?: boolean;
}

export function AdminAuditEntryDrawer({
  selectedEntry,
  drawerVisible,
  onClose,
  titleId = 'audit-drawer-title',
  hideActor = false,
}: AdminAuditEntryDrawerProps) {
  const { t } = useTranslation();
  const raw = selectedEntry as unknown as Record<string, unknown>;
  const beforeState = raw.beforeState;
  const afterState = raw.afterState;

  return (
    <>
      <div
        role="presentation"
        onClick={onClose}
        className={styles.drawerOverlay}
        style={{
          opacity: drawerVisible ? 1 : 0,
          transition: `opacity ${DRAWER_MS}ms cubic-bezier(0.4, 0, 0.2, 1)`,
          pointerEvents: drawerVisible ? 'auto' : 'none',
        }}
        aria-hidden
      />
      <aside
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        className={styles.drawerAside}
        style={{
          width: DRAWER_WIDTH,
          transform: drawerVisible ? 'translateX(0)' : 'translateX(100%)',
          transition: `transform ${DRAWER_MS}ms cubic-bezier(0.4, 0, 0.2, 1)`,
        }}
      >
        <div className={styles.drawerHeader}>
          <h2 id={titleId} className={styles.drawerTitle}>
            {t('pages:audit.auditEntry')}
          </h2>
          <button
            type="button"
            onClick={onClose}
            aria-label={t('pages:audit.closeDetailPanel')}
            className={styles.drawerCloseBtn}
          >
            ×
          </button>
        </div>
        <div className={styles.drawerBody}>
          <div className={styles.drawerGrid}>
            {!hideActor ? (
              <div>
                <div className={styles.fieldLabel}>
                  {t('pages:audit.drawerUser')}
                </div>
                <div className={styles.fieldValue}>
                  {selectedEntry.actorLabel ?? '—'}
                </div>
              </div>
            ) : null}
            <div>
              <div className={styles.fieldLabel}>
                {t('pages:audit.drawerEntity')}
              </div>
              <div className={styles.fieldValue}>
                <span className={styles.entityTypeMuted}>{selectedEntry.entityType}</span>
                {selectedEntry.entityId != null && selectedEntry.entityId !== '' ? (
                  <>
                    <span className={styles.entitySeparator}> · </span>
                    <span className={styles.entityIdMono}>{selectedEntry.entityId}</span>
                  </>
                ) : null}
              </div>
            </div>
            <div>
              <div className={styles.fieldLabel}>
                {t('pages:audit.drawerAction')}
              </div>
              <div className={styles.fieldValuePlain}>{selectedEntry.action}</div>
            </div>
            <div>
              <div className={styles.fieldLabel}>
                {t('pages:audit.drawerTime')}
              </div>
              <div className={styles.fieldValuePlain} title={formatDateTime(selectedEntry.timestamp)}>
                {relativeTime(selectedEntry.timestamp)}
              </div>
            </div>
          </div>
          {beforeState != null && <JsonStateBlock label={t('pages:audit.beforeState')} value={beforeState} />}
          {afterState != null && <JsonStateBlock label={t('pages:audit.afterState')} value={afterState} />}
        </div>
      </aside>
    </>
  );
}

interface AdminAuditLogTableProps {
  entries: AdminAuditEntry[];
  selectedEntry: AdminAuditEntry | null;
  onSelectEntry: (entry: AdminAuditEntry) => void;
  onToggleEntry: (entry: AdminAuditEntry) => void;
  /** When true, omits the Actor column (e.g. Settings "My admin audit" where the actor is always the viewer). */
  hideActorColumn?: boolean;
  /** Rows per page in the table (default 20; should match API limit). */
  pageSize?: number;
}

export function AdminAuditLogTable({
  entries,
  selectedEntry,
  onSelectEntry,
  onToggleEntry,
  hideActorColumn = false,
  pageSize = 20,
}: AdminAuditLogTableProps) {
  const { t } = useTranslation();
  const columns = useMemo(
    () => buildAdminAuditLogColumns(t, { hideActor: hideActorColumn }),
    [hideActorColumn, t],
  );

  return (
    <DataTable
      hideSearch
      frameless
      columns={columns}
      data={entries}
      pageSize={pageSize}
      emptyMessage={t('pages:audit.noAuditEntries')}
      onRowClick={(entry: AdminAuditEntry) => {
        if (selectedEntry?.id === entry.id) onToggleEntry(entry);
        else onSelectEntry(entry);
      }}
    />
  );
}
