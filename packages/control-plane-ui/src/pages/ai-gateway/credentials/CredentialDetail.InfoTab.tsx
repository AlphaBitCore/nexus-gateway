import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import clsx from 'clsx';
import {
  Badge, statusToVariant,
  FormField, Input, Switch, Tooltip, Button, Stack, Card, Select,
} from '@/components/ui';
import type { Credential, Provider } from '@/api/types';
import { formatDateTime } from '@/lib/format';
import styles from './CredentialDetail.module.css';

function rotationBadgeClass(state: string, s: Record<string, string>): string {
  switch (state) {
    case 'pending_rotation': return s.rotationStatePendingRotation;
    case 'validating': return s.rotationStateValidating;
    case 'rotated': return s.rotationStateRotated;
    case 'completed': return s.rotationStateCompleted;
    case 'failed': return s.rotationStateFailed;
    default: return s.rotationStateNone;
  }
}

export interface CredentialInfoTabProps {
  credential: Credential;
  provider: Provider | null;
  rotationState: string;
  canUpdate: boolean;
  isEditing: boolean;
  startEditing: () => void;
  handleSave: () => void;
  updating: boolean;
  setIsEditing: (v: boolean) => void;
  editName: string;
  setEditName: (v: string) => void;
  editEnabled: boolean;
  setEditEnabled: (v: boolean) => void;
  editApiKey: string;
  setEditApiKey: (v: string) => void;
  editWeight: number;
  setEditWeight: (v: number) => void;
  editStatus: string;
  setEditStatus: (v: string) => void;
  editExpiresAt: string;
  setEditExpiresAt: (v: string) => void;
}

export function CredentialInfoTab({
  credential,
  provider,
  rotationState,
  canUpdate,
  isEditing,
  startEditing,
  handleSave,
  updating,
  setIsEditing,
  editName,
  setEditName,
  editEnabled,
  setEditEnabled,
  editApiKey,
  setEditApiKey,
  editWeight,
  setEditWeight,
  editStatus,
  setEditStatus,
  editExpiresAt,
  setEditExpiresAt,
}: CredentialInfoTabProps) {
  const { t } = useTranslation();
  return (
    <>
      {canUpdate && !isEditing ? (
        <div className={styles.credentialInfoToolbar}>
          <Button onClick={startEditing}>{t('common:edit')}</Button>
        </div>
      ) : null}
      <Card>
        {isEditing ? (
          <Stack gap="md">
            <div className={`${styles.credentialEditThreeColGrid} ${styles.credentialEditableFields}`}>
              <FormField label={t('pages:credentials.name')} required>
                <Input name="editName" value={editName} onChange={(e) => setEditName(e.target.value)} required />
              </FormField>
              <FormField
                label={t('pages:credentials.newApiKeyLabel')}
                helpText={t('pages:credentials.newApiKeyHelpText')}
              >
                <Input name="editApiKey" value={editApiKey} onChange={(e) => setEditApiKey(e.target.value)} type="password" placeholder={t('pages:credentials.placeholderApiKeyHint')} />
              </FormField>
              <FormField label={t('pages:providers.credExpiresAtLabel')} helpText={t('pages:providers.credExpiresAtHelp')}>
                <Input
                  name="editExpiresAt"
                  type="date"
                  value={editExpiresAt}
                  onChange={(e) => setEditExpiresAt(e.target.value)}
                />
              </FormField>
            </div>
            <div className={`${styles.credentialEditThreeColGrid} ${styles.credentialEditableFields}`}>
              <FormField label={t('pages:credentials.selectionWeightLabel')} helpText={t('pages:credentials.selectionWeightHelp')}>
                <Input
                  name="editWeight"
                  type="number"
                  min={0}
                  max={10000}
                  value={String(editWeight)}
                  onChange={(e) => setEditWeight(Number(e.target.value))}
                />
              </FormField>
              <FormField label={t('pages:credentials.poolStatusLabel')} helpText={t('pages:credentials.poolStatusHelp')}>
                <Select
                  value={editStatus}
                  onValueChange={setEditStatus}
                  options={[
                    { value: 'active', label: t('pages:credentials.poolStatus_active') },
                    { value: 'retiring', label: t('pages:credentials.poolStatus_retiring') },
                  ]}
                />
              </FormField>
              <div className={styles.credentialEditSwitchField}>
                <div className={styles.credentialEditSwitchStack}>
                  <Tooltip content={t('pages:credentials.enabledTooltip')}>
                    <span className={styles.credentialEditSwitchLabel}>{t('pages:credentials.enabledLabel')}</span>
                  </Tooltip>
                  <Switch checked={editEnabled} onCheckedChange={setEditEnabled} />
                </div>
              </div>
            </div>
            <div className={styles.infoSectionDivider} />
            <div className={`${styles.kvGrid} ${styles.credentialReadonlyFields}`}>
              <div>
                <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                  <span className={styles.kvLabel}>{t('pages:credentials.provider')}</span>
                  <Tooltip content={t('pages:credentials.providerTooltip')}>
                    <span className={styles.helpIcon}>?</span>
                  </Tooltip>
                </Stack>
                <div className={styles.kvValue}>
                  {provider ? (
                    <Link to={`/ai-gateway/providers/${provider.id}`} className={styles.link}>{provider.displayName || provider.name}</Link>
                  ) : credential.providerId}
                </div>
              </div>
              <div>
                <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                  <span className={styles.kvLabel}>{t('pages:credentials.rotationState')}</span>
                  <Tooltip content={t('pages:credentials.rotationStateTooltip')}>
                    <span className={styles.helpIcon}>?</span>
                  </Tooltip>
                </Stack>
                <div className={styles.badgeOffset}><span className={clsx(styles.rotationBadge, rotationBadgeClass(rotationState, styles))}>{rotationState.replace(/_/g, ' ')}</span></div>
              </div>
              <div>
                <div className={styles.kvLabel}>{t('pages:credentials.created')}</div>
                <div className={styles.kvValue}>{formatDateTime(credential.createdAt)}</div>
              </div>
              <div>
                <div className={styles.kvLabel}>{t('pages:credentials.lastUpdated')}</div>
                <div className={styles.kvValue}>{formatDateTime(credential.updatedAt)}</div>
              </div>
              <div>
                <div className={styles.kvLabel}>{t('pages:credentials.lastUsedLabel')}</div>
                <div className={styles.kvValue}>{formatDateTime(credential.lastUsedAt)}</div>
              </div>
              <div>
                <div className={styles.kvLabel}>{t('pages:credentials.totalUsageCount')}</div>
                <div className={styles.kvValueMono}>{credential.totalUsageCount.toLocaleString()}</div>
              </div>
            </div>
          </Stack>
        ) : (
          <Stack gap="md">
            <div className={`${styles.kvGrid} ${styles.credentialEditableFields}`}>
              <div>
                <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                  <span className={styles.kvLabel}>{t('pages:credentials.name')}</span>
                  <Tooltip content={t('pages:credentials.nameTooltip')}>
                    <span className={styles.helpIcon}>?</span>
                  </Tooltip>
                </Stack>
                <div className={styles.kvValueBold}>{credential.name}</div>
              </div>
              <div>
                <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                  <span className={styles.kvLabel}>{t('pages:credentials.status')}</span>
                  <Tooltip content={t('pages:credentials.statusTooltip')}>
                    <span className={styles.helpIcon}>?</span>
                  </Tooltip>
                </Stack>
                <div className={styles.badgeOffset}><Badge variant={statusToVariant(credential.enabled ? 'enabled' : 'disabled')}>{credential.enabled ? t('common:enabled') : t('common:disabled')}</Badge></div>
              </div>
              <div>
                <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                  <span className={styles.kvLabel}>{t('pages:credentials.storedSecret')}</span>
                  <Tooltip content={t('pages:credentials.storedSecretTooltip')}>
                    <span className={styles.helpIcon}>?</span>
                  </Tooltip>
                </Stack>
                <div className={styles.kvValueMono}>{t('pages:credentials.notDisplayed')}</div>
              </div>
              <div>
                <div className={styles.kvLabel}>{t('pages:credentials.expires')}</div>
                <div className={styles.kvValue}>
                  {credential.expiresAt ? (
                    <Stack direction="horizontal" gap="xs" align="center">
                      <span>{formatDateTime(credential.expiresAt)}</span>
                      {new Date(credential.expiresAt) < new Date() ? (
                        <Badge variant="danger">{t('pages:credentials.expiresOverdue')}</Badge>
                      ) : rotationState === 'pending_rotation' ? (
                        <Badge variant="warning">{t('pages:credentials.expiringSoon')}</Badge>
                      ) : null}
                    </Stack>
                  ) : '—'}
                </div>
              </div>
              <div>
                <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                  <span className={styles.kvLabel}>{t('pages:credentials.selectionWeightLabel')}</span>
                  <Tooltip content={t('pages:credentials.selectionWeightHelp')}>
                    <span className={styles.helpIcon}>?</span>
                  </Tooltip>
                </Stack>
                <div className={styles.kvValueMono}>{credential.selectionWeight ?? 100}</div>
              </div>
              <div>
                <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                  <span className={styles.kvLabel}>{t('pages:credentials.poolStatusLabel')}</span>
                  <Tooltip content={t('pages:credentials.poolStatusHelp')}>
                    <span className={styles.helpIcon}>?</span>
                  </Tooltip>
                </Stack>
                <div className={styles.badgeOffset}>
                  <Badge variant={
                    credential.status === 'retiring' ? 'warning' :
                    credential.status === 'retired' ? 'default' : 'success'
                  }>
                    {t(`pages:credentials.poolStatus_${credential.status ?? 'active'}`)}
                  </Badge>
                </div>
              </div>
            </div>
            <div className={styles.infoSectionDivider} />
            <div className={`${styles.kvGrid} ${styles.credentialReadonlyFields}`}>
              <div>
                <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                  <span className={styles.kvLabel}>{t('pages:credentials.provider')}</span>
                  <Tooltip content={t('pages:credentials.providerTooltip')}>
                    <span className={styles.helpIcon}>?</span>
                  </Tooltip>
                </Stack>
                <div className={styles.kvValue}>
                  {provider ? (
                    <Link to={`/ai-gateway/providers/${provider.id}`} className={styles.link}>{provider.displayName || provider.name}</Link>
                  ) : credential.providerId}
                </div>
              </div>
              <div>
                <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                  <span className={styles.kvLabel}>{t('pages:credentials.rotationState')}</span>
                  <Tooltip content={t('pages:credentials.rotationStateTooltip')}>
                    <span className={styles.helpIcon}>?</span>
                  </Tooltip>
                </Stack>
                <div className={styles.badgeOffset}><span className={clsx(styles.rotationBadge, rotationBadgeClass(rotationState, styles))}>{rotationState.replace(/_/g, ' ')}</span></div>
              </div>
              <div>
                <div className={styles.kvLabel}>{t('pages:credentials.created')}</div>
                <div className={styles.kvValue}>{formatDateTime(credential.createdAt)}</div>
              </div>
              <div>
                <div className={styles.kvLabel}>{t('pages:credentials.lastUpdated')}</div>
                <div className={styles.kvValue}>{formatDateTime(credential.updatedAt)}</div>
              </div>
              <div>
                <Stack direction="horizontal" gap="xs" align="center" className={styles.kvLabelRow}>
                  <span className={styles.kvLabel}>{t('pages:credentials.lastRotated')}</span>
                  <Tooltip content={t('pages:credentials.lastRotatedTooltip')}>
                    <span className={styles.helpIcon}>?</span>
                  </Tooltip>
                </Stack>
                <div className={styles.kvValue}>{formatDateTime(credential.lastRotatedAt)}</div>
              </div>
              <div>
                <div className={styles.kvLabel}>{t('pages:credentials.lastUsedLabel')}</div>
                <div className={styles.kvValue}>{formatDateTime(credential.lastUsedAt)}</div>
              </div>
              <div>
                <div className={styles.kvLabel}>{t('pages:credentials.lastSuccess')}</div>
                <div className={styles.kvValue}>{formatDateTime(credential.lastSuccessAt)}</div>
              </div>
              <div>
                <div className={styles.kvLabel}>{t('pages:credentials.lastFailure')}</div>
                <div className={styles.kvValue}>
                  {credential.lastFailureAt ? <span className={styles.dangerText}>{formatDateTime(credential.lastFailureAt)}</span> : '--'}
                  {credential.lastFailureReason && (
                    <div className={styles.failureDetail}>{credential.lastFailureReason}</div>
                  )}
                </div>
              </div>
              <div>
                <div className={styles.kvLabel}>{t('pages:credentials.totalUsageCount')}</div>
                <div className={styles.kvValueMono}>{credential.totalUsageCount.toLocaleString()}</div>
              </div>
              {credential.retireAt && (
              <div>
                <div className={styles.kvLabel}>{t('pages:credentials.retireAt')}</div>
                <div className={styles.kvValue}>{formatDateTime(credential.retireAt)}</div>
              </div>
              )}
              <div>
                <span className={styles.kvLabel}>{t('pages:credentials.reliability')}</span>
                <div className={styles.badgeOffset}>
                  <span className={styles.reliabilityHintText}>{t('pages:credentials.seeReliabilityTab')}</span>
                </div>
              </div>
            </div>
          </Stack>
        )}
      </Card>
      {isEditing ? (
        <Stack direction="horizontal" gap="sm" className={styles.credentialEditActions}>
          <Button onClick={handleSave} disabled={updating || !editName} loading={updating}>
            {t('common:save')}
          </Button>
          <Button variant="secondary" onClick={() => setIsEditing(false)}>{t('common:cancel')}</Button>
        </Stack>
      ) : null}
    </>
  );
}
