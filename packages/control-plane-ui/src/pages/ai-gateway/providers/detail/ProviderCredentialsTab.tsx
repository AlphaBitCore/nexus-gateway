import { useState, useCallback } from 'react';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { 
  Badge,
  Button,
  Card,
  Input,
  Stack,
  statusToVariant
} from '@/components/ui';
import type { ProviderDetailState } from './useProviderDetail';
import { fmtDate } from './useProviderDetail';
import styles from './ProviderDetail.module.css';

interface ProviderCredentialsTabProps {
  detail: ProviderDetailState;
}

export function ProviderCredentialsTab({ detail }: ProviderCredentialsTabProps) {
  const { t } = useTranslation();
  const {
    id,
    credentials,
    canUpdate, canDelete, canCreateCredential,
    showCredForm, setShowCredForm,
    newCredForm,
    createCredential, credCreating,
    editingCredId, setEditingCredId,
    editCredForm,
    handleCredUpdate, credUpdating,
    startEditingCred,
    toggleCredEnabled,
    setDeletingCred,
  } = detail;

  const [expandedIds, setExpandedIds] = useState<Set<string>>(() => new Set());
  const toggleExpanded = useCallback((credId: string) => {
    setExpandedIds(prev => {
      const next = new Set(prev);
      if (next.has(credId)) next.delete(credId);
      else next.add(credId);
      return next;
    });
  }, []);

  const credName = newCredForm.watch('credName');
  const credApiKey = newCredForm.watch('credApiKey');
  const newCredEnabled = newCredForm.watch('newCredEnabled');
  const credExpiresAt = newCredForm.watch('credExpiresAt');

  const editCredName = editCredForm.watch('editCredName');
  const editCredApiKey = editCredForm.watch('editCredApiKey');
  const editCredEnabled = editCredForm.watch('editCredEnabled');
  const editCredExpiresAt = editCredForm.watch('editCredExpiresAt');

  return (
    <Card>
      <div className={styles.toolbarEnd}>
        {canCreateCredential && (
          <Button onClick={() => setShowCredForm(!showCredForm)}>{t('pages:providers.addCredential')}</Button>
        )}
      </div>

      {showCredForm && (
        <div className={styles.inlineFormRow}>
          <div className={styles.flexWrap}>
            <div className={styles.flexField}>
              <label className={styles.inlineLabel}>{t('pages:providers.newCredNameLabel')}</label>
              <Input value={credName} onChange={e => newCredForm.setValue('credName', e.target.value)} placeholder={t('pages:providers.placeholderCredName')} className={styles.inlineInput} />
            </div>
            <div className={styles.flexField}>
              <label className={styles.inlineLabel}>{t('pages:providers.newCredApiKeyLabel')}</label>
              <Input value={credApiKey} onChange={e => newCredForm.setValue('credApiKey', e.target.value)} placeholder={t('pages:providers.placeholderApiKeyHint')} type="password" className={styles.inlineInput} />
            </div>
            <div className={styles.flexField}>
              <label className={styles.inlineLabel}>{t('pages:providers.credExpiresAtLabel')}</label>
              <Input value={credExpiresAt} onChange={e => newCredForm.setValue('credExpiresAt', e.target.value)} type="date" className={styles.inlineInput} />
            </div>
            <div className={styles.flexFieldAuto}>
              <Button variant="ghost" size="sm" onClick={() => newCredForm.setValue('newCredEnabled', !newCredEnabled)}
                className={newCredEnabled ? styles.enableToggleEnabled : styles.enableToggle}
              >
                {newCredEnabled ? t('common:enabled') : t('common:disabled')}
              </Button>
            </div>
          </div>
          <Stack direction="horizontal" gap="xs" className={styles.justifyEnd}>
            <Button variant="secondary" size="sm" onClick={() => { setShowCredForm(false); newCredForm.reset(); }}>{t('common:cancel')}</Button>
            <Button size="sm"
              onClick={() => { if (credName && credApiKey && id) createCredential({ name: credName, providerId: id, apiKey: credApiKey, enabled: newCredEnabled, expiresAt: credExpiresAt ? `${credExpiresAt}T00:00:00Z` : undefined }); }}
              disabled={credCreating || !credName || !credApiKey}
            >{credCreating ? t('pages:providers.saving') : t('common:create')}</Button>
          </Stack>
        </div>
      )}

      {/* Editing credential inline */}
      {editingCredId && (() => {
        const c = credentials.find(cr => cr.id === editingCredId);
        if (!c) return null;
        return (
          <div className={styles.inlineFormRowHighlight}>
            <div className={styles.editingTitle}>{t('pages:providers.editing', { name: c.name })}</div>
            <div className={styles.flexWrap}>
              <div className={styles.flexField}>
                <label className={styles.inlineLabel}>{t('pages:providers.name')}</label>
                <Input value={editCredName} onChange={e => editCredForm.setValue('editCredName', e.target.value)} className={styles.inlineInput} />
              </div>
              <div className={styles.flexField}>
                <label className={styles.inlineLabel}>{t('pages:providers.newApiKeyHint')}</label>
                <Input value={editCredApiKey} onChange={e => editCredForm.setValue('editCredApiKey', e.target.value)} placeholder={t('pages:providers.placeholderApiKeyHint')} type="password" className={styles.inlineInput} />
              </div>
              <div className={styles.flexField}>
                <label className={styles.inlineLabel}>{t('pages:providers.credExpiresAtLabel')}</label>
                <Input value={editCredExpiresAt} onChange={e => editCredForm.setValue('editCredExpiresAt', e.target.value)} type="date" className={styles.inlineInput} />
              </div>
              <div className={styles.flexFieldAuto}>
                <Button variant="ghost" size="sm" onClick={() => editCredForm.setValue('editCredEnabled', !editCredEnabled)}
                  className={editCredEnabled ? styles.enableToggleEnabled : styles.enableToggle}
                >
                  {editCredEnabled ? t('common:enabled') : t('common:disabled')}
                </Button>
              </div>
            </div>
            <Stack direction="horizontal" gap="xs" className={styles.justifyEnd}>
              <Button variant="secondary" size="sm" onClick={() => setEditingCredId(null)}>{t('common:cancel')}</Button>
              <Button size="sm" onClick={handleCredUpdate} disabled={credUpdating || !editCredName}>
                {credUpdating ? t('pages:providers.saving') : t('common:save')}
              </Button>
            </Stack>
          </div>
        );
      })()}

      {credentials.length === 0 ? (
        <div className={styles.emptyState}>{t('pages:providers.noCredentials')}</div>
      ) : (
        <div className={styles.overflowAuto}>
          <table className={styles.table}>
            <thead>
              <tr>
                <th className={styles.thChevron} aria-hidden="true" />
                {[
                  t('pages:providers.credTableName'),
                  t('pages:providers.credTableStatus'),
                  t('pages:providers.credTablePoolStatus'),
                  t('pages:providers.credTableCircuit'),
                  t('pages:providers.credTableExpires'),
                  t('pages:providers.credTableLastUsed'),
                  t('pages:providers.credTableActions'),
                ].map(h => (
                  <th key={h} className={styles.th}>{h}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {credentials.map(c => {
                const isExpanded = expandedIds.has(c.id);
                const rows = [
                  <tr
                    key={c.id}
                    className={styles.tableRow}
                    onClick={() => toggleExpanded(c.id)}
                    aria-expanded={isExpanded}
                  >
                    <td className={styles.tdChevron}>
                      <button
                        type="button"
                        className={styles.chevronBtn}
                        aria-label={isExpanded ? 'Collapse row' : 'Expand row'}
                        aria-expanded={isExpanded}
                        onClick={(e) => { e.stopPropagation(); toggleExpanded(c.id); }}
                      >
                        <span aria-hidden="true">{isExpanded ? '▼' : '▶'}</span>
                      </button>
                    </td>
                    <td className={styles.tdBold}>{c.name}</td>
                    <td className={styles.td} onClick={e => e.stopPropagation()}>
                      <Button variant="ghost" size="sm" onClick={() => toggleCredEnabled({ id: c.id, enabled: !c.enabled })}
                        className={c.enabled ? styles.enableToggleEnabled : styles.enableToggle}
                      >
                        {c.enabled ? t('common:enabled') : t('common:disabled')}
                      </Button>
                    </td>
                    <td className={styles.td}>
                      {(() => {
                        const status = c.status ?? 'active';
                        const variant = status === 'active' ? 'success' : status === 'retiring' ? 'warning' : 'default';
                        return <Badge variant={variant}>{t(`pages:credentials.poolStatus_${status}`, { defaultValue: status })}</Badge>;
                      })()}
                    </td>
                    <td className={styles.td}>
                      {(() => {
                        const state = c.circuitState ?? 'closed';
                        if (state === 'closed') return <span>—</span>;
                        const variant = state === 'open' ? 'danger' : 'warning';
                        const label = t(`pages:credentials.circuit_${state}`, { defaultValue: state });
                        const reason = c.circuitReason
                          ? t(`pages:credentials.circuitReason_${c.circuitReason}`, { defaultValue: c.circuitReason })
                          : undefined;
                        return <Badge variant={variant} title={reason}>{label}</Badge>;
                      })()}
                    </td>
                    <td className={styles.td}>
                      {c.expiresAt ? (
                        <>
                          {fmtDate(c.expiresAt)}
                          {' '}
                          {new Date(c.expiresAt) < new Date() ? (
                            <Badge variant="danger">{t('pages:providers.credOverdueBadge')}</Badge>
                          ) : c.rotationState === 'pending_rotation' ? (
                            <Badge variant="warning">{t('pages:providers.credExpiringBadge')}</Badge>
                          ) : null}
                        </>
                      ) : '—'}
                    </td>
                    <td className={styles.td}>{fmtDate(c.lastUsedAt)}</td>
                    <td className={styles.td} onClick={e => e.stopPropagation()}>
                      <Stack direction="horizontal" gap="xs">
                        {canUpdate && (
                          <Button variant="secondary" size="sm" onClick={() => startEditingCred(c)}>{t('common:edit')}</Button>
                        )}
                        {canDelete && (
                          <Button variant="danger" size="sm" onClick={() => setDeletingCred(c)}>{t('common:delete')}</Button>
                        )}
                      </Stack>
                    </td>
                  </tr>,
                ];
                if (isExpanded) {
                  rows.push(
                    <tr key={`${c.id}-expanded`} className={styles.expandedPanelRow}>
                      <td colSpan={8} className={styles.expandedPanelCell}>
                        <div className={styles.expandedKvGrid}>
                          <div className={styles.expandedKv}>
                            <div className={styles.expandedKvLabel}>{t('pages:providers.credTableRotation')}</div>
                            <div className={styles.expandedKvValue}>
                              <Badge variant={statusToVariant(c.rotationState ?? 'unknown')}>{c.rotationState ?? 'unknown'}</Badge>
                            </div>
                          </div>
                          <div className={styles.expandedKv}>
                            <div className={styles.expandedKvLabel}>{t('pages:providers.credTableWeight')}</div>
                            <div className={styles.expandedKvValueMono}>{c.selectionWeight ?? 100}</div>
                          </div>
                          <div className={styles.expandedKv}>
                            <div className={styles.expandedKvLabel}>{t('pages:providers.credTableUsageCount')}</div>
                            <div className={styles.expandedKvValueMono}>{c.totalUsageCount.toLocaleString()}</div>
                          </div>
                          <div className={styles.expandedKv}>
                            <div className={styles.expandedKvLabel}>{t('pages:providers.credTableLastSuccess')}</div>
                            <div className={styles.expandedKvValue}>{fmtDate(c.lastSuccessAt)}</div>
                          </div>
                          <div className={styles.expandedKv}>
                            <div className={styles.expandedKvLabel}>{t('pages:providers.credTableLastFailure')}</div>
                            <div className={styles.expandedKvValue}>
                              {fmtDate(c.lastFailureAt)}
                              {c.lastFailureReason && (
                                <div className={styles.failureReason}>{c.lastFailureReason}</div>
                              )}
                            </div>
                          </div>
                          <div className={styles.expandedKv}>
                            <div className={styles.expandedKvLabel}>{t('pages:providers.credTableCreated')}</div>
                            <div className={styles.expandedKvValue}>{fmtDate(c.createdAt)}</div>
                          </div>
                        </div>
                        <div className={styles.expandedFooter}>
                          <Link to={`/ai-gateway/credentials/${c.id}`} className={styles.viewFullLink}>
                            {t('pages:credentials.viewFullDetail')} →
                          </Link>
                        </div>
                      </td>
                    </tr>,
                  );
                }
                return rows;
              })}
            </tbody>
          </table>
        </div>
      )}
    </Card>
  );
}
