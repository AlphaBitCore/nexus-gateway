import { useTranslation } from 'react-i18next';
import { Link } from 'react-router-dom';
import {
  Skeleton, ErrorBanner, AlertDialog, Button, Stack, Badge, statusToVariant,
  Tabs, TabsList, TabsTrigger, TabsContent, Dialog, FormField, Input,
} from '@/components/ui';
import { useIamUserDetail } from './useIamUserDetail';
import { UserInfoTab } from './UserInfoTab';
import { UserPermissionsTab } from './UserPermissionsTab';
import { UserDevicesTab } from './UserDevicesTab';
import styles from '../_shared/Iam.module.css';

export function IamUserDetailPage() {
  const { t } = useTranslation();
  const state = useIamUserDetail();

  const {
    user,
    loading,
    error,
    refetch,
    deletingUser,
    setDeletingUser,
    deleteUser,
    isResettingPassword,
    setIsResettingPassword,
    resetPassword,
    setResetPassword,
    resetPasswordConfirm,
    setResetPasswordConfirm,
    resetPasswordLoading,
    handleResetPassword,
  } = state;

  if (loading) return <Skeleton.DetailPageSkeleton />;
  if (error) return <ErrorBanner message={error.message} onRetry={refetch} />;
  if (!user) return <ErrorBanner message={t('pages:iam.userNotFound')} />;

  const passwordMismatch = resetPassword !== resetPasswordConfirm && resetPasswordConfirm !== '';
  // Federated (SSO) accounts authenticate through their external IdP and have
  // no local password — an admin cannot set one. The reset dialog shows guidance
  // instead of the password form (the backend also rejects it with sso_account).
  const isSsoAccount = !!user.source && user.source !== 'local';

  return (
    <Stack gap="lg">
      <section className={styles.detailHeader}>
        <div className={styles.detailHeaderRow}>
          <Link to="/iam/users" className={styles.detailBackLink} aria-label={t('common:back')}>
            <svg className={styles.detailBackIcon} width="20" height="20" viewBox="0 0 20 20" fill="none" aria-hidden="true">
              <path d="M8.33333 5L3.33333 10L8.33333 15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
              <path d="M4.16667 10H13.3333C15.1743 10 16.6667 11.4924 16.6667 13.3333V15" stroke="currentColor" strokeWidth="1.66667" strokeLinecap="round" strokeLinejoin="round" />
            </svg>
          </Link>
          <div className={styles.detailHeaderText}>
            <h1 className={styles.detailTitle}>{user.displayName}</h1>
            <div className={styles.detailMeta}>
              {user.email && <Badge variant="outline">{user.email}</Badge>}
              <Badge variant={statusToVariant(user.status)}>{user.status}</Badge>
              {user.source && user.source !== 'local' && <Badge variant="info">{user.source.toUpperCase()}</Badge>}
            </div>
          </div>
          <Stack direction="horizontal" gap="sm" align="center" className={styles.detailHeaderActions}>
            <Button variant="secondary" onClick={() => setIsResettingPassword(true)}>
              {t('pages:iam.resetPassword')}
            </Button>
            <Button variant="danger" onClick={() => setDeletingUser(true)}>
              {t('common:delete')}
            </Button>
          </Stack>
        </div>
      </section>

      <Tabs defaultValue="info" className={styles.detailTabs}>
        <TabsList>
          <TabsTrigger value="info">{t('pages:iam.info')}</TabsTrigger>
          <TabsTrigger value="permissions">{t('pages:iam.permissions')}</TabsTrigger>
          <TabsTrigger value="devices">{t('pages:userDetail.tabs.devices')}</TabsTrigger>
        </TabsList>

        <TabsContent value="info">
          <UserInfoTab
            user={state.user}
            isEditing={state.isEditing}
            setIsEditing={state.setIsEditing}
            startEditing={state.startEditing}
            editDisplayName={state.editDisplayName}
            setEditDisplayName={state.setEditDisplayName}
            editEmail={state.editEmail}
            setEditEmail={state.setEditEmail}
            editEnabled={state.editEnabled}
            setEditEnabled={state.setEditEnabled}
            editOrgId={state.editOrgId}
            setEditOrgId={state.setEditOrgId}
            editCanAccessCP={state.editCanAccessCP}
            setEditCanAccessCP={state.setEditCanAccessCP}
            saveLoading={state.saveLoading}
            handleSave={state.handleSave}
          />
        </TabsContent>

        <TabsContent value="permissions">
          <UserPermissionsTab
            allPolicies={user.policyAttachments ?? []}
            showAddRole={state.showAddRole}
            setShowAddRole={state.setShowAddRole}
            selectedGroupId={state.selectedGroupId}
            setSelectedGroupId={state.setSelectedGroupId}
            removingRole={state.removingRole}
            setRemovingRole={state.setRemovingRole}
            currentRoles={state.currentRoles}
            availableGroups={state.availableGroups}
            addToGroup={state.addToGroup}
            addGroupLoading={state.addGroupLoading}
            removeFromGroup={state.removeFromGroup}
            removeGroupLoading={state.removeGroupLoading}
            directPolicies={state.directPolicies}
            showAttachPolicy={state.showAttachPolicy}
            setShowAttachPolicy={state.setShowAttachPolicy}
            selectedPolicyId={state.selectedPolicyId}
            setSelectedPolicyId={state.setSelectedPolicyId}
            detachingPolicy={state.detachingPolicy}
            setDetachingPolicy={state.setDetachingPolicy}
            availablePolicies={state.availablePolicies}
            attachPolicy={state.attachPolicy}
            attachPolicyLoading={state.attachPolicyLoading}
            detachPolicy={state.detachPolicy}
            detachPolicyLoading={state.detachPolicyLoading}
          />
        </TabsContent>

        <TabsContent value="devices">
          {user.id && <UserDevicesTab userId={user.id} />}
        </TabsContent>


      </Tabs>

      <Dialog
        open={isResettingPassword}
        onOpenChange={(open) => {
          if (!open) {
            setIsResettingPassword(false);
            setResetPassword('');
            setResetPasswordConfirm('');
          }
        }}
        title={t('pages:iam.resetPassword')}
        size="sm"
      >
        {isSsoAccount ? (
          <Stack gap="md">
            <p>{t('pages:iam.resetPasswordSsoManaged')}</p>
            <Stack direction="horizontal" gap="sm" justify="end">
              <Button variant="secondary" onClick={() => setIsResettingPassword(false)}>
                {t('common:close')}
              </Button>
            </Stack>
          </Stack>
        ) : (
          <Stack gap="md">
            <FormField label={t('pages:iam.newPassword')}>
              <Input
                name="resetPassword"
                type="password"
                value={resetPassword}
                onChange={(e) => setResetPassword(e.target.value)}
                placeholder={t('pages:iam.newPasswordPlaceholder')}
              />
            </FormField>
            <FormField
              label={t('pages:iam.confirmPassword')}
              error={passwordMismatch ? t('pages:iam.passwordMismatch') : undefined}
            >
              <Input
                name="resetPasswordConfirm"
                type="password"
                value={resetPasswordConfirm}
                onChange={(e) => setResetPasswordConfirm(e.target.value)}
                placeholder={t('pages:iam.confirmPasswordPlaceholder')}
              />
            </FormField>
            <Stack direction="horizontal" gap="sm" justify="end">
              <Button
                variant="secondary"
                onClick={() => {
                  setIsResettingPassword(false);
                  setResetPassword('');
                  setResetPasswordConfirm('');
                }}
              >
                {t('common:cancel')}
              </Button>
              <Button
                onClick={handleResetPassword}
                disabled={resetPasswordLoading || !resetPassword || passwordMismatch}
              >
                {resetPasswordLoading ? t('pages:iam.saving') : t('pages:iam.resetPassword')}
              </Button>
            </Stack>
          </Stack>
        )}
      </Dialog>

      <AlertDialog
        open={deletingUser}
        onOpenChange={(open) => { if (!open) setDeletingUser(false); }}
        title={t('pages:iam.deleteUser')}
        description={t('pages:iam.deleteUserConfirm', { name: user.displayName })}
        confirmLabel={t('common:delete')}
        onConfirm={() => deleteUser(undefined as never)}
        variant="danger"
      />
    </Stack>
  );
}
