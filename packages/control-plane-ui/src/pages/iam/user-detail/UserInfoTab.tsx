import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import {
  Badge, statusToVariant, FormField, Input, Switch, Button, Card,
} from '@/components/ui';
import { OrgTreeSelect } from '@/components/ui/OrgTreeSelect';
import { formatDateTime } from '@/lib/format';
import type { UseIamUserDetailReturn } from './useIamUserDetail';
import styles from '../_shared/Iam.module.css';

function SourceBadge({ source }: { source?: string }) {
  if (!source || source === 'local') return null;
  const label = source === 'oidc' ? 'SSO (OIDC)' : source === 'saml' ? 'SSO (SAML)' : source === 'scim' ? 'SCIM' : source.toUpperCase();
  return <Badge variant="info">Synced · {label}</Badge>;
}

type Props = Pick<
  UseIamUserDetailReturn,
  | 'user'
  | 'isEditing'
  | 'setIsEditing'
  | 'startEditing'
  | 'editDisplayName'
  | 'setEditDisplayName'
  | 'editEmail'
  | 'setEditEmail'
  | 'editEnabled'
  | 'setEditEnabled'
  | 'editOrgId'
  | 'setEditOrgId'
  | 'editCanAccessCP'
  | 'setEditCanAccessCP'
  | 'saveLoading'
  | 'handleSave'
>;

export function UserInfoTab({
  user,
  isEditing,
  setIsEditing,
  startEditing,
  editDisplayName,
  setEditDisplayName,
  editEmail,
  setEditEmail,
  editEnabled,
  setEditEnabled,
  editOrgId,
  setEditOrgId,
  editCanAccessCP,
  setEditCanAccessCP,
  saveLoading,
  handleSave,
}: Props) {
  const { t } = useTranslation();

  if (!user) return null;

  const isIdPManaged = user.source === 'oidc' || user.source === 'saml' || user.source === 'scim';

  return (
    <>
      <Card>
        {isEditing ? (
          <div className={styles.userInfoFormGrid}>
          <FormField
            label={t('pages:iam.displayName')}
            helpText={isIdPManaged ? t('pages:iam.managedByIdP') : undefined}
          >
            <Input
              name="editDisplayName"
              value={editDisplayName}
              onChange={(e) => setEditDisplayName(e.target.value)}
              disabled={isIdPManaged}
            />
          </FormField>
          <FormField
            label={t('pages:iam.email')}
            helpText={isIdPManaged ? t('pages:iam.managedByIdP') : undefined}
          >
            <Input
              name="editEmail"
              value={editEmail}
              onChange={(e) => setEditEmail(e.target.value)}
              type="email"
              disabled={isIdPManaged}
            />
          </FormField>
          <FormField
            label={t('pages:iam.organization')}
            helpText={isIdPManaged ? t('pages:iam.managedByIdP') : undefined}
          >
            <OrgTreeSelect
              mode="single"
              allowClear={false}
              inlineSearch
              className={styles.userInfoOrgSelect}
              value={editOrgId}
              onChange={(v) => setEditOrgId(v as string)}
              placeholder={t('pages:iam.selectOrg')}
              disabled={isIdPManaged}
            />
          </FormField>
          <FormField label={t('common:enabled')}>
            <Switch checked={editEnabled} onCheckedChange={setEditEnabled} />
          </FormField>
          <FormField label={t('pages:iam.canAccessControlPlane')}>
            <Switch checked={editCanAccessCP} onCheckedChange={setEditCanAccessCP} />
          </FormField>
        </div>
        ) : (
          <div className={`${styles.kvGrid} ${styles.userInfoGrid}`}>
          <div>
            <div className={styles.kvLabel}>{t('pages:iam.displayName')}</div>
            <div className={styles.kvValue}>{user.displayName}</div>
          </div>
          <div>
            <div className={styles.kvLabel}>{t('pages:iam.email')}</div>
            <div className={styles.kvValue}>{user.email || '--'}</div>
          </div>
          <div>
            <div className={styles.kvLabel}>{t('pages:iam.status')}</div>
            <div className={styles.badgeOffset}>
              <Badge variant={statusToVariant(user.status)}>{user.status}</Badge>
            </div>
          </div>
          <div>
            <div className={styles.kvLabel}>{t('pages:iam.canAccessControlPlane')}</div>
            <div className={styles.badgeOffset}>
              <Badge variant={user.canAccessControlPlane ? 'success' : 'default'}>
                {user.canAccessControlPlane ? t('common:yes') : t('common:no')}
              </Badge>
            </div>
          </div>
          <div>
            <div className={styles.kvLabel}>{t('pages:iam.organization')}</div>
            <div className={styles.kvValue}>
              {user.organizationId ? (
                <Link
                  to={`/iam/organizations/${user.organizationId}`}
                  className={styles.primaryLink}
                >
                  {user.organizationName || user.organizationId}
                </Link>
              ) : '--'}
            </div>
          </div>
          <div>
            <div className={styles.kvLabel}>{t('pages:iam.lastLogin')}</div>
            <div className={styles.kvValue}>{formatDateTime(user.lastLoginAt)}</div>
          </div>
          <div>
            <div className={styles.kvLabel}>{t('pages:iam.created')}</div>
            <div className={styles.kvValue}>{formatDateTime(user.createdAt)}</div>
          </div>
          {user.source && user.source !== 'local' && (
            <div>
              <div className={styles.kvLabel}>{t('pages:iam.source')}</div>
              <div className={styles.badgeOffset}>
                <SourceBadge source={user.source} />
              </div>
            </div>
          )}
        </div>
        )}
      </Card>
      <div className={styles.userInfoActions}>
        {isEditing ? (
          <>
            <Button onClick={handleSave} disabled={saveLoading}>
              {saveLoading ? t('pages:iam.saving') : t('common:save')}
            </Button>
            <Button variant="secondary" onClick={() => setIsEditing(false)}>
              {t('common:cancel')}
            </Button>
          </>
        ) : (
          <Button onClick={startEditing}>
            {t('common:edit')}
          </Button>
        )}
      </div>
    </>
  );
}
